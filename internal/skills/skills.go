// Package skills is procedural memory: versioned skills that a dream drafts
// as candidates and only real use promotes. The success rate is the mean of
// Beta(successes+1, failures+1), so one lucky run does not read as 100%.
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

const (
	// PromoteAfter is how many successful uses a candidate needs, as a
	// candidate, before it becomes active.
	PromoteAfter = 3
	// MinRate is the Beta success rate a skill must keep to be (or stay) active.
	MinRate = 0.5
	// DeprecateMinUses is how much evidence an active skill needs before a
	// low rate deprecates it.
	DeprecateMinUses = 5

	KindSkill = "skill" // journal target kind
)

var (
	ErrBadRef     = errors.New("skills: malformed skill ref, want skill:<id>@v<n> or <id>@v<n>")
	ErrDeprecated = errors.New("skills: that skill version is deprecated")
	ErrNotActive  = errors.New("skills: no active version to roll back")
)

// Rate is the mean of Beta(successes+1, failures+1).
func Rate(s store.Skill) float64 {
	return float64(s.Successes+1) / float64(s.Uses+2)
}

// Ref is the citation handle for one version: "skill:<id>@v<n>".
func Ref(s store.Skill) string { return "skill:" + TargetID(s.ID, s.Version) }

// TargetID is the journal target id for one version: "<id>@v<n>".
func TargetID(id string, version int) string { return id + "@v" + strconv.Itoa(version) }

// ParseRef reads "skill:<id>@v<n>", "<id>@v<n>" or "<id>" (version 0: latest).
// Anything after the first whitespace is ignored, so a citation can be pasted.
func ParseRef(s string) (id string, version int, err error) {
	s = strings.TrimSpace(s)
	if f := strings.Fields(s); len(f) > 0 {
		s = f[0]
	}
	s = strings.TrimPrefix(s, "skill:")
	id, v, hasV := strings.Cut(s, "@v")
	if id == "" {
		return "", 0, ErrBadRef
	}
	if !hasV {
		return id, 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return "", 0, ErrBadRef
	}
	return id, n, nil
}

// NewRunID names a group of journal entries made outside a dream.
func NewRunID() string { return "skill:" + ulid.Make().String() }

// Transition is what a reported outcome did to a skill's status.
type Transition string

const (
	Kept      Transition = ""
	Promoted  Transition = "promote"
	Deprecate Transition = "deprecate"
)

// RecordUse counts one reported outcome against a skill version and applies
// the gate: a candidate with PromoteAfter successes and a rate of at least
// MinRate becomes active (and replaces any other active version of the same
// skill); an active skill whose rate falls below MinRate after
// DeprecateMinUses uses is deprecated. Status changes are journaled under
// runID; the counters are not, since they record what happened, not a belief.
func RecordUse(ctx context.Context, tx store.Tx, id string, version int, success bool, now time.Time, runID string) (store.Skill, Transition, error) {
	sk, err := tx.Skills().Get(ctx, id, version)
	if err != nil {
		return sk, Kept, err
	}
	if sk.Status == store.SkillDeprecated {
		return sk, Kept, ErrDeprecated
	}
	before := sk
	sk.Uses++
	if success {
		sk.Successes++
	}
	sk.LastUsed, sk.UpdatedAt = now, now
	tr := Kept
	switch {
	case sk.Status == store.SkillCandidate && sk.Successes >= PromoteAfter && Rate(sk) >= MinRate:
		sk.Status, tr = store.SkillActive, Promoted
	case sk.Status == store.SkillActive && sk.Uses >= DeprecateMinUses && Rate(sk) < MinRate:
		sk.Status, tr = store.SkillDeprecated, Deprecate
	}
	if tr == Promoted {
		// One active version at a time: the newly proven one wins.
		others, err := tx.Skills().Versions(ctx, id)
		if err != nil {
			return sk, Kept, err
		}
		for _, o := range others {
			if o.Version == sk.Version || o.Status != store.SkillActive {
				continue
			}
			old := o
			o.Status, o.UpdatedAt = store.SkillDeprecated, now
			if err := save(ctx, tx, old, o, runID, "deprecate", fmt.Sprintf("replaced by v%d", sk.Version), now); err != nil {
				return sk, Kept, err
			}
		}
	}
	if tr == Kept {
		return sk, tr, tx.Skills().Save(ctx, sk)
	}
	why := fmt.Sprintf("%d/%d successful uses, rate %.2f", sk.Successes, sk.Uses, Rate(sk))
	return sk, tr, save(ctx, tx, before, sk, runID, string(tr), why, now)
}

// Rollback deprecates the active version of id and reactivates the newest
// earlier version that had earned promotion and still has a rate of at least
// MinRate. It returns the reactivated version, or a zero Skill when there was
// none to go back to (the skill is then simply withdrawn).
func Rollback(ctx context.Context, tx store.Tx, id string, now time.Time, runID string) (store.Skill, error) {
	vs, err := tx.Skills().Versions(ctx, id)
	if err != nil {
		return store.Skill{}, err
	}
	cur := -1
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].Status == store.SkillActive {
			cur = i
			break
		}
	}
	if cur < 0 {
		return store.Skill{}, ErrNotActive
	}
	old := vs[cur]
	dep := old
	dep.Status, dep.UpdatedAt = store.SkillDeprecated, now
	if err := save(ctx, tx, old, dep, runID, "rollback", "rolled back", now); err != nil {
		return store.Skill{}, err
	}
	for i := cur - 1; i >= 0; i-- {
		p := vs[i]
		if p.Successes < PromoteAfter || Rate(p) < MinRate {
			continue
		}
		restored := p
		restored.Status, restored.UpdatedAt = store.SkillActive, now
		why := fmt.Sprintf("restored by rollback of v%d", old.Version)
		if err := save(ctx, tx, p, restored, runID, "rollback", why, now); err != nil {
			return store.Skill{}, err
		}
		return restored, nil
	}
	return store.Skill{}, nil
}

// SaveCandidate stores a new candidate version drafted by a dream and
// journals its creation so the dream can be reverted.
func SaveCandidate(ctx context.Context, tx store.Tx, sk store.Skill, runID, phase, why string, now time.Time) error {
	after, err := json.Marshal(sk)
	if err != nil {
		return err
	}
	if err := tx.Skills().Save(ctx, sk); err != nil {
		return err
	}
	_, err = tx.Journal().Append(ctx, store.JournalEntry{
		RunID: runID, Phase: phase, Op: "create", TargetKind: KindSkill, TargetID: TargetID(sk.ID, sk.Version),
		After: after, Rationale: why, SourceEpisodes: sk.EpisodeIDs, At: now,
	})
	return err
}

func save(ctx context.Context, tx store.Tx, before, after store.Skill, runID, op, why string, now time.Time) error {
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	a, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if err := tx.Skills().Save(ctx, after); err != nil {
		return err
	}
	_, err = tx.Journal().Append(ctx, store.JournalEntry{
		RunID: runID, Op: op, TargetKind: KindSkill, TargetID: TargetID(after.ID, after.Version),
		Before: b, After: a, Rationale: why, SourceEpisodes: after.EpisodeIDs, At: now,
	})
	return err
}
