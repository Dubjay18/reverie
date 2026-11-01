// Package rank fuses ranked result lists.
package rank

import "sort"

// rrfK damps the weight of top ranks; 60 is the value from the original RRF paper.
const rrfK = 60

type Scored struct {
	ID    string
	Score float64
}

// RRF merges ranked ID lists (best first) with reciprocal rank fusion:
// score(id) = sum over lists of 1/(rrfK + rank). It needs only ranks, so
// lists with incomparable scores (cosine distance vs ts_rank) fuse cleanly.
// Ties break on ID so output is deterministic.
func RRF(lists ...[]string) []Scored {
	score := map[string]float64{}
	for _, list := range lists {
		for i, id := range list {
			score[id] += 1 / float64(rrfK+i+1)
		}
	}
	out := make([]Scored, 0, len(score))
	for id, s := range score {
		out = append(out, Scored{id, s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}
