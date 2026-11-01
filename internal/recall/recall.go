// Package recall turns a task description into a budgeted, cited ContextPack.
package recall

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/rank"
	"github.com/Dubjay18/reverie/internal/store"
)

type Query struct {
	Task      string
	Budget    int
	Vector    []float32 // embedding of Task; nil means full-text only
	AsOf      time.Time // zero means now; ignores episodes that ended later
	Now       time.Time // zero means time.Now
	Tokenizer Tokenizer // nil means Approx
}

const (
	candidateLimit = 50 // per ranked list
	scoredLimit    = 30 // fused hits that get loaded and scored
	hotWindow      = 5  // newest unconsolidated episodes always eligible
	warmFallback   = 30 // with no task text, rank this many recent episodes
	halfLife       = 14 * 24 * time.Hour
	wRelevance     = 0.6 // score = wRelevance*rel + wRecency*recency + wImportance*importance
	wRecency       = 0.2
	wImportance    = 0.2
)

// Run builds the pack from one read-only view of the scope.
func Run(ctx context.Context, sc store.Scoped, q Query) (reverie.ContextPack, error) {
	tok := q.Tokenizer
	if tok == nil {
		tok = Approx{}
	}
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}

	var cands []candidate
	err := sc.View(ctx, func(tx store.Tx) error {
		var err error
		cands, err = gather(ctx, tx, q, now)
		return err
	})
	if err != nil {
		return reverie.ContextPack{Budget: q.Budget}, err
	}
	return assemble(map[string][]candidate{"episode": cands}, q.Budget, tok), nil
}

func gather(ctx context.Context, tx store.Tx, q Query, now time.Time) ([]candidate, error) {
	// relevance: fused rank of the text and vector lists, scaled to 0..1
	rel := map[string]float64{}
	if q.Task != "" {
		text, err := tx.Index().SearchText(ctx, q.Task, candidateLimit)
		if err != nil {
			return nil, err
		}
		lists := [][]string{ids(text)}
		if len(q.Vector) > 0 {
			vec, err := tx.Index().SearchVector(ctx, q.Vector, candidateLimit)
			if err != nil {
				return nil, err
			}
			lists = append(lists, ids(vec))
		}
		fused := rank.RRF(lists...)
		if len(fused) > scoredLimit {
			fused = fused[:scoredLimit]
		}
		for _, f := range fused {
			rel[f.ID] = f.Score / fused[0].Score
		}
	}

	hot, err := tx.Episodes().Recent(ctx, hotWindow+warmFallback)
	if err != nil {
		return nil, err
	}
	byTask := map[string]store.Episode{}
	isHot := map[string]bool{}
	hotSeen := 0
	for _, ep := range hot {
		// Only the hot window is specific to unconsolidated episodes.
		if !ep.Consolidated && hotSeen < hotWindow {
			isHot[ep.TaskID] = true
			hotSeen++
		}
		if q.Task == "" || isHot[ep.TaskID] {
			byTask[ep.TaskID] = ep // no query: recent episodes, consolidated or not, are the candidates
		}
	}
	for id := range rel {
		if _, ok := byTask[id]; ok {
			continue
		}
		ep, err := tx.Episodes().Get(ctx, id)
		if err != nil {
			continue // the index outlived its episode; skip rather than fail the recall
		}
		byTask[id] = ep
	}

	var out []candidate
	for id, ep := range byTask {
		if !ep.Closed || (!q.AsOf.IsZero() && ep.EndedAt.After(q.AsOf)) {
			continue
		}
		age := now.Sub(ep.EndedAt)
		recency := math.Pow(0.5, float64(max(age, 0))/float64(halfLife))
		score := wRelevance*rel[id] + wRecency*recency + wImportance*ep.Importance
		out = append(out, candidate{item: item(ep, isHot[id]), score: score, at: ep.EndedAt})
	}
	return out, nil
}

func ids(hits []store.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.RefID)
	}
	return out
}

func item(ep store.Episode, hot bool) reverie.PackItem {
	ref := reverie.Ref{TaskID: ep.TaskID, From: ep.FirstEvent, To: ep.LastEvent}
	title := ep.Title
	if title == "" {
		title = ep.TaskID
	}
	text := title
	if ep.Goal != "" {
		text += ` — "` + ep.Goal + `"`
	}
	text += fmt.Sprintf(": %s (%d events", ep.Outcome, ep.Events)
	if ep.Errors > 0 {
		text += fmt.Sprintf(", %d errors", ep.Errors)
	}
	text += ")"
	// LastError is deliberately not rendered: it is tool output, and printing
	// it into a prompt would let a hostile tool plant instructions that every
	// later session then reads. It stays reachable through Expand.
	return reverie.PackItem{
		ID:   ep.TaskID,
		Kind: "episode",
		Cite: fmt.Sprintf("%s @%s", ref, ep.EndedAt.UTC().Format("2006-01-02T15:04Z")),
		Text: text,
		Hot:  hot,
	}
}
