package dream

import (
	"context"
	"math"
	"regexp"
	"strings"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

const defaultReplayLimit = 50

// ReplayPhase selects unconsolidated closed episodes ordered by importance,
// loads their non-recalled events, and clusters them by shared task prefix or
// entity keywords (and by embedding cosine similarity >= 0.80 when vectors are
// provided).
type ReplayPhase struct {
	Limit   int
	Vectors map[string][]float32 // optional taskID -> embedding vector
}

func (p ReplayPhase) Name() string { return "replay" }

var clusterWordRE = regexp.MustCompile(`[a-z0-9_]{3,}`)

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "from": true, "into": true, "task": true, "session": true,
}

func (p ReplayPhase) Run(ctx context.Context, view ReadView, st *State) error {
	limit := p.Limit
	if limit <= 0 {
		limit = defaultReplayLimit
	}
	eps, err := view.Unconsolidated(ctx, limit)
	if err != nil {
		return err
	}
	if st.EpisodeOrigins == nil {
		st.EpisodeOrigins = map[string][]reverie.Origin{}
	}
	eventsByTask := map[string][]reverie.Event{}
	for _, ep := range eps {
		evs, err := view.Events(ctx, store.EventQuery{TaskID: ep.TaskID, From: ep.FirstEvent, To: ep.LastEvent})
		if err != nil {
			return err
		}
		var kept []reverie.Event
		var origins []reverie.Origin
		for _, ev := range evs {
			origins = append(origins, ev.Origin)
			// Rule 1 bootstrap guard: recalled events never count as evidence
			// for dreaming.
			if ev.Origin == reverie.OriginRecalled {
				continue
			}
			kept = append(kept, ev)
		}
		st.EpisodeOrigins[ep.TaskID] = origins
		eventsByTask[ep.TaskID] = kept
	}

	var clusters []Cluster
	for _, ep := range eps {
		key := clusterKey(ep)
		placed := false
		for i := range clusters {
			if sameCluster(&clusters[i], ep, key, p.Vectors) {
				clusters[i].Episodes = append(clusters[i].Episodes, ep)
				clusters[i].Events[ep.TaskID] = eventsByTask[ep.TaskID]
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, Cluster{
				Key:      key,
				Episodes: []store.Episode{ep},
				Events:   map[string][]reverie.Event{ep.TaskID: eventsByTask[ep.TaskID]},
			})
		}
	}
	st.Clusters = clusters
	return nil
}

func clusterKey(ep store.Episode) string {
	if prefix, _, ok := strings.Cut(strings.ToLower(ep.TaskID), ":"); ok && len(prefix) >= 3 && prefix != "session" && prefix != "task" {
		return prefix
	}
	for _, w := range clusterWordRE.FindAllString(strings.ToLower(ep.Title+" "+ep.Goal), -1) {
		if !stopWords[w] {
			return w
		}
	}
	return strings.ToLower(ep.TaskID)
}

func sameCluster(c *Cluster, ep store.Episode, key string, vecs map[string][]float32) bool {
	if c.Key == key {
		return true
	}
	if len(vecs) > 0 {
		v1 := vecs[ep.TaskID]
		for _, other := range c.Episodes {
			v2 := vecs[other.TaskID]
			if len(v1) > 0 && len(v1) == len(v2) && cosine(v1, v2) >= 0.80 {
				return true
			}
		}
	}
	return false
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
