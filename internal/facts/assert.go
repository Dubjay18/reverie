// Package facts resolves new statements against what is already believed.
//
// Assert is a pure function: given the live facts for an (entity, relation)
// and an incoming fact, it says what to write. It never touches a database,
// so every rule below is covered by plain table tests.
package facts

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/ontology"
)

type Op string

const (
	OpInsert     Op = "insert"     // a new value, no conflict
	OpMerge      Op = "merge"      // the same statement again: raise confidence, add provenance
	OpSupersede  Op = "supersede"  // a newer value replaces the old one(s)
	OpContest    Op = "contest"    // a less trusted value disagrees with a more trusted one: keep both, flag both
	OpReject     Op = "reject"     // a user-stated or pinned value wins; the incoming fact is not stored
	OpHistorical Op = "historical" // true in the past: stored already superseded by the current value
)

// Resolution is what to write. Fact is the incoming fact as it should be
// stored (zero for OpMerge and OpReject); Updates are existing facts to
// rewrite in the same transaction.
type Resolution struct {
	Op      Op
	Fact    reverie.Fact
	Updates []reverie.Fact
	Reason  string
}

var (
	ErrEmptyValue      = errors.New("facts: value is required")
	ErrNoEntity        = errors.New("facts: entity is required")
	ErrNoID            = errors.New("facts: fact has no id")
	ErrNoSource        = errors.New("facts: unknown fact source")
	ErrSubjectMismatch = errors.New("facts: relation does not apply to this entity type")
)

// Normalize canonicalises a fact and fills defaults. It is exported so that
// callers (and tests) see the same form Assert compares.
func Normalize(f reverie.Fact, onto *ontology.Ontology, now time.Time) (reverie.Fact, ontology.Relation, error) {
	f.Entity = strings.ToLower(strings.TrimSpace(f.Entity))
	f.Relation = strings.ToLower(strings.TrimSpace(f.Relation))
	f.Value = strings.TrimSpace(f.Value)
	f.EntityType = strings.ToLower(strings.TrimSpace(f.EntityType))
	switch {
	case f.Entity == "":
		return f, ontology.Relation{}, ErrNoEntity
	case f.Value == "":
		return f, ontology.Relation{}, ErrEmptyValue
	case f.ID == "":
		return f, ontology.Relation{}, ErrNoID
	case f.Source.Rank() == 0:
		return f, ontology.Relation{}, fmt.Errorf("%w: %q", ErrNoSource, f.Source)
	}
	rel, err := onto.Relation(f.Relation)
	if err != nil {
		return f, rel, err
	}
	if rel.Subject != "" && f.EntityType != "" && f.EntityType != rel.Subject {
		return f, rel, fmt.Errorf("%w: %q is a %s, %s applies to %s", ErrSubjectMismatch, f.Entity, f.EntityType, rel.Name, rel.Subject)
	}
	if f.Confidence <= 0 {
		f.Confidence = defaultConfidence(f.Source)
	}
	f.Confidence = min(f.Confidence, 1)
	if f.ValidFrom.IsZero() {
		f.ValidFrom = now
	}
	f.RecordedAt = now
	if f.Status == "" {
		f.Status = reverie.FactActive
	}
	return f, rel, nil
}

func defaultConfidence(s reverie.FactSource) float64 {
	switch s {
	case reverie.SourceUser:
		return 1
	case reverie.SourceAgent:
		return 0.9
	case reverie.SourceDistilled:
		return 0.8
	}
	return 0.5
}

// Assert decides what to do with in. existing may hold any facts; only live
// ones for the same entity and relation are considered.
func Assert(existing []reverie.Fact, in reverie.Fact, onto *ontology.Ontology, now time.Time) (Resolution, error) {
	in, rel, err := Normalize(in, onto, now)
	if err != nil {
		return Resolution{}, err
	}

	var live []reverie.Fact
	for _, f := range existing {
		if f.Entity == in.Entity && f.Relation == in.Relation && f.Live() {
			live = append(live, f)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].ValidFrom.Before(live[j].ValidFrom) })

	// 1. the same statement again
	for _, f := range live {
		if strings.EqualFold(f.Value, in.Value) {
			merged := merge(f, in)
			if in.Source == reverie.SourceUser && f.Status == reverie.FactContested && !pinnedOther(live, f.ID) {
				// The user settled the dispute: their value is active again and
				// every other live value for the key is closed.
				merged.Status = reverie.FactActive
				updates := []reverie.Fact{merged}
				for _, g := range live {
					if g.ID == f.ID || g.Pinned {
						continue
					}
					g.Status, g.ValidTo, g.SupersededBy = reverie.FactSuperseded, in.ValidFrom, f.ID
					updates = append(updates, g)
				}
				return Resolution{Op: OpMerge, Updates: updates, Reason: "the user settled a dispute"}, nil
			}
			return Resolution{Op: OpMerge, Updates: []reverie.Fact{merged}, Reason: "already believed"}, nil
		}
	}

	// 2. a relation that holds many values has no conflicts
	if rel.Cardinality == ontology.Many || len(live) == 0 {
		return Resolution{Op: OpInsert, Fact: in}, nil
	}

	// 3. one value allowed, and a different one is already believed
	for _, f := range live {
		if f.Pinned {
			if in.Source == reverie.SourceUser {
				in.Status = reverie.FactContested
				return Resolution{Op: OpContest, Fact: in, Reason: "conflicts with a pinned fact; unpin it to replace it"}, nil
			}
			return Resolution{Op: OpReject, Reason: "a pinned fact is never changed by a lower-trust source"}, nil
		}
	}
	if in.Source != reverie.SourceUser {
		for _, f := range live {
			if f.Source == reverie.SourceUser {
				return Resolution{Op: OpReject, Reason: "the user's own statement wins"}, nil
			}
		}
		var stronger []reverie.Fact
		for _, f := range live {
			if f.Source.Rank() > in.Source.Rank() {
				f.Status = reverie.FactContested
				stronger = append(stronger, f)
			}
		}
		if len(stronger) > 0 {
			in.Status = reverie.FactContested
			return Resolution{Op: OpContest, Fact: in, Updates: stronger, Reason: "a less trusted source disagrees with a more trusted one"}, nil
		}
	}

	// 4. in is trusted at least as much as everything it disagrees with:
	// the later ValidFrom wins.
	newest := live[len(live)-1].ValidFrom
	if in.ValidFrom.Before(newest) {
		next := live[0] // the earliest live fact that starts after in
		for _, f := range live {
			if f.ValidFrom.After(in.ValidFrom) {
				next = f
				break
			}
		}
		in.Status, in.ValidTo, in.SupersededBy = reverie.FactSuperseded, next.ValidFrom, next.ID
		return Resolution{Op: OpHistorical, Fact: in, Reason: "true earlier than the current value"}, nil
	}
	updates := make([]reverie.Fact, 0, len(live))
	for _, f := range live {
		f.Status, f.ValidTo, f.SupersededBy = reverie.FactSuperseded, in.ValidFrom, in.ID
		updates = append(updates, f)
	}
	return Resolution{Op: OpSupersede, Fact: in, Updates: updates, Reason: "a newer value replaces the old one"}, nil
}

// merge folds in a repeat statement into f: confidence rises (two
// independent statements are stronger than one), provenance accumulates, and
// a more trusted source upgrades the fact's source.
func merge(f, in reverie.Fact) reverie.Fact {
	f.Confidence = 1 - (1-f.Confidence)*(1-in.Confidence)
	if in.Source.Rank() > f.Source.Rank() {
		f.Source = in.Source
	}
	seen := map[string]bool{}
	var ids []string
	for _, id := range append(append([]string{}, f.EpisodeIDs...), in.EpisodeIDs...) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	f.EpisodeIDs = ids
	return f
}

// pinnedOther reports whether a pinned fact other than id is live. A pin
// outranks a restatement, so a dispute with one stays a dispute until the
// user unpins it.
func pinnedOther(live []reverie.Fact, id string) bool {
	for _, f := range live {
		if f.ID != id && f.Pinned {
			return true
		}
	}
	return false
}
