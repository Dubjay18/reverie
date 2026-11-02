// Package recall turns a task description into a budgeted, cited ContextPack.
package recall

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/ontology"
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

	var byKind map[string][]candidate
	err := sc.View(ctx, func(tx store.Tx) error {
		var err error
		byKind, err = gather(ctx, tx, q, now)
		return err
	})
	if err != nil {
		return reverie.ContextPack{Budget: q.Budget}, err
	}
	return assemble(byKind, q.Budget, tok), nil
}

const (
	kindEpisode = "episode"
	kindFact    = "fact"
	alwaysShown = 10 // contested facts always eligible (pinned ones are never capped)
	expandNames = 12 // query words used to find related facts
)

var wordRE = regexp.MustCompile(`[\p{L}\p{N}_]{3,}`)

func gather(ctx context.Context, tx store.Tx, q Query, now time.Time) (map[string][]candidate, error) {
	// relevance: fused rank of the text, vector and entity lists, scaled to 0..1
	rel := map[string]float64{}
	if q.Task != "" {
		text, err := tx.Index().SearchText(ctx, q.Task, candidateLimit)
		if err != nil {
			return nil, err
		}
		lists := [][]string{keys(text)}
		if len(q.Vector) > 0 {
			vec, err := tx.Index().SearchVector(ctx, q.Vector, candidateLimit)
			if err != nil {
				return nil, err
			}
			lists = append(lists, keys(vec))
		}
		related, err := relatedFacts(ctx, tx, q.Task)
		if err != nil {
			return nil, err
		}
		if len(related) > 0 {
			lists = append(lists, related)
		}
		fused := rank.RRF(lists...)
		if len(fused) > scoredLimit {
			fused = fused[:scoredLimit]
		}
		for _, f := range fused {
			rel[f.ID] = f.Score / fused[0].Score
		}
	}

	out := map[string][]candidate{}
	if err := gatherEpisodes(ctx, tx, q, now, rel, out); err != nil {
		return nil, err
	}
	if err := gatherFacts(ctx, tx, q, now, rel, out); err != nil {
		return nil, err
	}
	if err := gatherSkills(ctx, tx, q, out); err != nil {
		return nil, err
	}
	if err := gatherInsights(ctx, tx, q, out); err != nil {
		return nil, err
	}
	return out, nil
}

func gatherEpisodes(ctx context.Context, tx store.Tx, q Query, now time.Time, rel map[string]float64, out map[string][]candidate) error {
	recent, err := tx.Episodes().Recent(ctx, hotWindow+warmFallback)
	if err != nil {
		return err
	}
	byTask := map[string]store.Episode{}
	isHot := map[string]bool{}
	hotSeen := 0
	for _, ep := range recent {
		// Only the hot window is specific to unconsolidated episodes.
		if !ep.Consolidated && hotSeen < hotWindow {
			isHot[ep.TaskID] = true
			hotSeen++
		}
		if q.Task == "" || isHot[ep.TaskID] {
			byTask[ep.TaskID] = ep // no query: recent episodes, consolidated or not, are the candidates
		}
	}
	for key := range rel {
		kind, id, _ := strings.Cut(key, "/")
		if kind != kindEpisode {
			continue
		}
		if _, ok := byTask[id]; ok {
			continue
		}
		ep, err := tx.Episodes().Get(ctx, id)
		if err != nil {
			continue // the index outlived its episode; skip rather than fail the recall
		}
		byTask[id] = ep
	}
	for id, ep := range byTask {
		if !ep.Closed || (!q.AsOf.IsZero() && ep.EndedAt.After(q.AsOf)) {
			continue
		}
		age := now.Sub(ep.EndedAt)
		recency := math.Pow(0.5, float64(max(age, 0))/float64(halfLife))
		score := wRelevance*rel[kindEpisode+"/"+id] + wRecency*recency + wImportance*ep.Importance
		out[kindEpisode] = append(out[kindEpisode], candidate{item: item(ep, isHot[id]), score: score, at: ep.EndedAt})
	}
	return nil
}

func gatherFacts(ctx context.Context, tx store.Tx, q Query, now time.Time, rel map[string]float64, out map[string][]candidate) error {
	byID := map[string]reverie.Fact{}
	add := func(fs []reverie.Fact) {
		for _, f := range fs {
			byID[f.ID] = f
		}
	}
	// What the user pinned, and what is in dispute, is shown whatever the query.
	pinned, err := tx.Facts().Query(ctx, store.FactQuery{OnlyPinned: true, Limit: store.MaxLimit})
	if err != nil {
		return err
	}
	contested, err := tx.Facts().Query(ctx, store.FactQuery{OnlyContested: true, Limit: alwaysShown})
	if err != nil {
		return err
	}
	add(pinned)
	add(contested)
	for key := range rel {
		kind, id, _ := strings.Cut(key, "/")
		if kind != kindFact {
			continue
		}
		if f, err := tx.Facts().Get(ctx, id); err == nil && f.Live() {
			byID[id] = f
		}
	}
	if q.Task == "" { // no query: the most trusted facts about the user
		top, err := tx.Facts().Query(ctx, store.FactQuery{Limit: warmFallback})
		if err != nil {
			return err
		}
		add(top)
	}
	onto := ontology.Default()
	for id, f := range byID {
		if !q.AsOf.IsZero() && f.RecordedAt.After(q.AsOf) {
			continue
		}
		age := now.Sub(f.RecordedAt)
		if !f.Pinned {
			if rel, err := onto.Relation(f.Relation); err == nil && rel.TTLDays > 0 {
				refAt := f.ValidFrom
				if f.RecordedAt.After(refAt) {
					refAt = f.RecordedAt
				}
				if !refAt.IsZero() && now.Sub(refAt) >= time.Duration(rel.TTLDays)*24*time.Hour {
					continue
				}
			}
		}
		recency := math.Pow(0.5, float64(max(age, 0))/float64(halfLife))
		score := wRelevance*rel[kindFact+"/"+id] + wRecency*recency + wImportance*f.Confidence
		if f.Pinned || f.Status == reverie.FactContested {
			score += 2 // never crowded out by relevance
		}
		out[kindFact] = append(out[kindFact], candidate{item: factItem(f), score: score, at: f.RecordedAt})
	}
	return nil
}

// relatedFacts finds facts about the things the query mentions (one hop),
// then facts about what those facts point to (a second hop). Order is the
// order of discovery, nearest first, so it ranks as a list.
func relatedFacts(ctx context.Context, tx store.Tx, task string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, w := range wordRE.FindAllString(strings.ToLower(task), -1) {
		if !seen[w] && len(names) < expandNames {
			seen[w] = true
			names = append(names, w)
		}
	}
	var out []string
	got := map[string]bool{}
	for hop := 0; hop < 2 && len(names) > 0; hop++ {
		fs, err := tx.Facts().Related(ctx, names, candidateLimit)
		if err != nil {
			return nil, err
		}
		names = names[:0]
		for _, f := range fs {
			if !got[f.ID] {
				got[f.ID] = true
				out = append(out, kindFact+"/"+f.ID)
			}
			// A value is a name to follow only if it looks like one: a short
			// token, not a sentence; and "user" would match every note.
			next := []string{f.Entity}
			if v := strings.ToLower(f.Value); len(strings.Fields(v)) <= 3 {
				next = append(next, v)
			}
			for _, n := range next {
				if n != "user" && !seen[n] && len(names) < expandNames {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
	}
	return out, nil
}

func keys(hits []store.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Kind+"/"+h.RefID)
	}
	return out
}

// factItem renders a fact as data: the value is quoted and capped, and
// anything not stated by the user is marked unverified, so a value an agent
// was talked into storing cannot pass for an instruction or for the user's word.
func factItem(f reverie.Fact) reverie.PackItem {
	text := guard.OneLine(f.Entity+" "+strings.ReplaceAll(f.Relation, "_", " "), 80) + ": " +
		strconv.Quote(guard.OneLine(f.Value, 240))
	var tags []string
	if f.Source != reverie.SourceUser {
		tags = append(tags, "unverified")
	}
	if f.Pinned {
		tags = append(tags, "pinned")
	}
	if f.Status == reverie.FactContested {
		tags = append(tags, "contested: another value is also recorded; ask before relying on it")
	}
	tags = append(tags, strings.ReplaceAll(string(f.Source), "_", " "))
	text += " [" + strings.Join(tags, "; ") + "]"
	return reverie.PackItem{
		ID:     f.ID,
		Kind:   kindFact,
		Cite:   fmt.Sprintf("fact:%s @%s", f.ID, f.RecordedAt.UTC().Format("2006-01-02T15:04Z")),
		Text:   text,
		Source: f.Source,
		Status: f.Status,
	}
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
		Kind: kindEpisode,
		Cite: fmt.Sprintf("%s @%s", ref, ep.EndedAt.UTC().Format("2006-01-02T15:04Z")),
		Text: text,
		Hot:  hot,
	}
}
