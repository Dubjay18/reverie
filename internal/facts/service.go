package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/ontology"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

var (
	ErrPinned         = errors.New("facts: the fact is pinned; unpin it first")
	ErrNotBelieved    = errors.New("facts: no such fact, or it is already forgotten")
	ErrAlreadyUndone  = errors.New("facts: that change was already reverted")
	ErrForbidden      = errors.New("facts: a less trusted caller cannot change a fact from a more trusted source")
	ErrInstruction    = errors.New("facts: the value reads like an instruction to a model, so it was not stored")
	ErrRevertConflict = errors.New("facts: the fact changed after that run; revert the later change first")
)

const (
	KindFact    = "fact"    // the index and journal kind for facts
	KindEpisode = "episode" // the journal kind for episode consolidation
)

// NewRunID names a group of journal entries that are applied and reverted together.
func NewRunID(prefix string) string { return prefix + ":" + ulid.Make().String() }

// Remember asserts in against what is believed and writes the outcome inside
// tx: the fact(s), the search index, and one journal entry per change, all
// under runID. A rejected fact writes nothing and is reported in the
// Resolution.
type RememberOpt func(*rememberCfg)

type rememberCfg struct {
	phase  string
	tokens int
}

// WithJournalMeta attaches phase and token counts to the journal entries
// written by Remember.
func WithJournalMeta(phase string, tokens int) RememberOpt {
	return func(c *rememberCfg) { c.phase, c.tokens = phase, tokens }
}

func Remember(ctx context.Context, tx store.Tx, onto *ontology.Ontology, in reverie.Fact, now time.Time, runID string, opts ...RememberOpt) (Resolution, error) {
	var cfg rememberCfg
	for _, o := range opts {
		o(&cfg)
	}
	norm, _, err := Normalize(in, onto, now)
	if err != nil {
		return Resolution{}, err
	}
	// Facts from anything less trusted than the user are later printed into
	// prompts, so a value that reads as a command is refused outright.
	if norm.Source != reverie.SourceUser && guard.LooksLikeInstruction(norm.Entity+" "+norm.Value) {
		return Resolution{}, ErrInstruction
	}
	live, err := tx.Facts().Live(ctx, norm.Entity, norm.Relation)
	if err != nil {
		return Resolution{}, err
	}
	res, err := Assert(live, norm, onto, now)
	if err != nil {
		return Resolution{}, err
	}

	before := map[string]reverie.Fact{}
	for _, f := range live {
		before[f.ID] = f
	}
	for _, u := range res.Updates {
		if err := tx.Facts().Save(ctx, u); err != nil {
			return res, err
		}
		if u.Status == reverie.FactSuperseded {
			if err := tx.Index().Delete(ctx, KindFact, u.ID); err != nil {
				return res, err
			}
		}
		if err := journalMeta(ctx, tx, runID, cfg.phase, string(res.Op), before[u.ID], &u, res.Reason, cfg.tokens, now); err != nil {
			return res, err
		}
	}
	if res.Op == OpReject {
		// Nothing changes, but the attempt is on record.
		rejected := norm
		if err := journalMeta(ctx, tx, runID, cfg.phase, "reject", reverie.Fact{}, &rejected, res.Reason, cfg.tokens, now); err != nil {
			return res, err
		}
	}
	if res.Fact.ID != "" {
		if err := tx.Facts().Save(ctx, res.Fact); err != nil {
			return res, err
		}
		if err := syncIndex(ctx, tx, res.Fact); err != nil {
			return res, err
		}
		if err := journalMeta(ctx, tx, runID, cfg.phase, "create", reverie.Fact{}, &res.Fact, string(res.Op)+": "+res.Reason, cfg.tokens, now); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Forget tombstones a believed fact. Pinned facts must be unpinned first,
// and a caller can only forget facts from sources it is trusted at least as
// much as: an agent cannot erase what the user said.
func Forget(ctx context.Context, tx store.Tx, id string, by reverie.FactSource, now time.Time, runID string) error {
	f, err := tx.Facts().Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && f.Status == reverie.FactTombstoned) {
		return ErrNotBelieved
	}
	if err != nil {
		return err
	}
	if f.Pinned {
		return ErrPinned
	}
	if f.Source.Rank() > by.Rank() {
		return ErrForbidden
	}
	g := f
	g.Status = reverie.FactTombstoned
	if err := tx.Facts().Save(ctx, g); err != nil {
		return err
	}
	if err := tx.Index().Delete(ctx, KindFact, id); err != nil {
		return err
	}
	return journal(ctx, tx, runID, "forget", f, &g, "forgotten on request", now)
}

// SetPinned pins or unpins a believed fact.
func SetPinned(ctx context.Context, tx store.Tx, id string, pinned bool, now time.Time, runID string) error {
	f, err := tx.Facts().Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotBelieved
	}
	if err != nil {
		return err
	}
	if !f.Live() {
		return ErrNotBelieved
	}
	g := f
	g.Pinned = pinned
	if err := tx.Facts().Save(ctx, g); err != nil {
		return err
	}
	op := "pin"
	if !pinned {
		op = "unpin"
	}
	return journal(ctx, tx, runID, op, f, &g, "", now)
}

// RevertRun undoes every change journaled under runID, newest first, by
// putting each target back as it was. A fact the run created is tombstoned,
// not deleted: nothing is hard-deleted.
//
// It refuses (ErrRevertConflict) when a fact has changed since the run
// touched it, because restoring the old state would silently throw away the
// later change; revert the later run first. Because a revert is journaled as
// a run of its own ("revert:<run>"), it can itself be reverted to redo.
func RevertRun(ctx context.Context, tx store.Tx, runID string, now time.Time) (int, error) {
	entries, err := tx.Journal().ByRun(ctx, runID)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 && !strings.HasPrefix(runID, "dream:") {
		return 0, store.ErrNotFound
	}
	undone := 0
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Reverted || e.Op == "reject" {
			continue
		}
		if e.TargetKind == KindEpisode {
			curEp, err := tx.Episodes().Get(ctx, e.TargetID)
			if err != nil {
				return undone, fmt.Errorf("facts: journal entry %d: episode %s: %w", e.ID, e.TargetID, err)
			}
			var targetEp store.Episode
			if len(e.Before) > 0 {
				if err := json.Unmarshal(e.Before, &targetEp); err != nil {
					return undone, fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
				}
			} else {
				targetEp = curEp
				targetEp.Consolidated = false
				targetEp.Tier = store.Hot
			}
			if err := tx.Episodes().Save(ctx, targetEp); err != nil {
				return undone, err
			}
			if err := tx.Journal().MarkReverted(ctx, e.ID); err != nil {
				return undone, err
			}
			beforeBytes, _ := json.Marshal(curEp)
			afterBytes, _ := json.Marshal(targetEp)
			if _, err := tx.Journal().Append(ctx, store.JournalEntry{
				RunID:          "revert:" + runID,
				Op:             "revert",
				TargetKind:     KindEpisode,
				TargetID:       e.TargetID,
				Before:         beforeBytes,
				After:          afterBytes,
				Rationale:      "undo of " + e.Op,
				SourceEpisodes: []string{e.TargetID},
				At:             now,
			}); err != nil {
				return undone, err
			}
			undone++
			continue
		}
		if e.TargetKind != KindFact {
			continue
		}
		cur, err := tx.Facts().Get(ctx, e.TargetID)
		if err != nil {
			return undone, fmt.Errorf("facts: journal entry %d: fact %s: %w", e.ID, e.TargetID, err)
		}
		if len(e.After) > 0 {
			var after reverie.Fact
			if err := json.Unmarshal(e.After, &after); err != nil {
				return undone, fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
			}
			if !sameFact(cur, after) {
				return undone, fmt.Errorf("%w (fact %s%s)", ErrRevertConflict, e.TargetID, laterRun(ctx, tx, e))
			}
		}
		var target reverie.Fact
		if len(e.Before) > 0 {
			if err := json.Unmarshal(e.Before, &target); err != nil {
				return undone, fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
			}
		} else {
			target = cur
			target.Status = reverie.FactTombstoned
		}
		if err := tx.Facts().Save(ctx, target); err != nil {
			return undone, err
		}
		if err := syncIndex(ctx, tx, target); err != nil {
			return undone, err
		}
		if err := tx.Journal().MarkReverted(ctx, e.ID); err != nil {
			return undone, err
		}
		if err := journal(ctx, tx, "revert:"+runID, "revert", cur, &target, "undo of "+e.Op, now); err != nil {
			return undone, err
		}
		undone++
	}
	// If this was a dream run, also restore Consolidated=false on the episodes
	// that its journal entries or dream report consolidated.
	if err := unmarkDreamEpisodes(ctx, tx, runID, entries, &undone); err != nil {
		return undone, err
	}
	if undone == 0 {
		return 0, ErrAlreadyUndone
	}
	return undone, nil
}

func unmarkDreamEpisodes(ctx context.Context, tx store.Tx, runID string, entries []store.JournalEntry, undone *int) error {
	if !strings.HasPrefix(runID, "dream:") {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		for _, epID := range e.SourceEpisodes {
			seen[epID] = true
		}
	}
	if dr, err := tx.Dreams().GetRun(ctx, runID); err == nil && len(dr.Report) > 0 {
		var rep struct {
			EpisodeIDs []string `json:"episode_ids"`
		}
		if json.Unmarshal(dr.Report, &rep) == nil {
			for _, epID := range rep.EpisodeIDs {
				seen[epID] = true
			}
		}
	}
	for epID := range seen {
		ep, err := tx.Episodes().Get(ctx, epID)
		if err != nil || !ep.Consolidated {
			continue
		}
		ep.Consolidated = false
		ep.Tier = store.Hot
		if err := tx.Episodes().Save(ctx, ep); err != nil {
			return err
		}
		if len(entries) == 0 {
			*undone++
		}
	}
	return nil
}

// laterRun names the newest unreverted change to the same fact after e, to
// tell the user what to revert first.
func laterRun(ctx context.Context, tx store.Tx, e store.JournalEntry) string {
	recent, err := tx.Journal().Recent(ctx, 500)
	if err != nil {
		return ""
	}
	for _, r := range recent { // newest first
		if r.TargetID == e.TargetID && r.ID > e.ID && !r.Reverted && r.RunID != e.RunID {
			return ", changed later by run " + r.RunID
		}
	}
	return ""
}

// sameFact compares two facts by value. Times are compared as instants and
// provenance as a set, since both come back from storage in a normal form.
func sameFact(a, b reverie.Fact) bool {
	if a.ID != b.ID || a.Entity != b.Entity || a.EntityType != b.EntityType || a.Relation != b.Relation ||
		a.Value != b.Value || a.Confidence != b.Confidence || a.Source != b.Source || a.Status != b.Status ||
		a.Pinned != b.Pinned || a.SupersededBy != b.SupersededBy {
		return false
	}
	if !a.ValidFrom.Equal(b.ValidFrom) || !a.ValidTo.Equal(b.ValidTo) || !a.RecordedAt.Equal(b.RecordedAt) {
		return false
	}
	x, y := append([]string(nil), a.EpisodeIDs...), append([]string(nil), b.EpisodeIDs...)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}

// syncIndex keeps the search index equal to the set of believed facts.
func syncIndex(ctx context.Context, tx store.Tx, f reverie.Fact) error {
	if !f.Live() {
		return tx.Index().Delete(ctx, KindFact, f.ID)
	}
	return tx.Index().Upsert(ctx, store.Item{Kind: KindFact, RefID: f.ID, Text: Text(f)})
}

// Text is what search sees for a fact: "jay lives in London".
func Text(f reverie.Fact) string {
	return f.Entity + " " + strings.ReplaceAll(f.Relation, "_", " ") + " " + f.Value
}

func journal(ctx context.Context, tx store.Tx, runID, op string, before reverie.Fact, after *reverie.Fact, why string, now time.Time) error {
	return journalMeta(ctx, tx, runID, "", op, before, after, why, 0, now)
}

func journalMeta(ctx context.Context, tx store.Tx, runID, phase, op string, before reverie.Fact, after *reverie.Fact, why string, tokens int, now time.Time) error {
	e := store.JournalEntry{RunID: runID, Phase: phase, Op: op, TargetKind: KindFact, Rationale: why, Tokens: tokens, At: now}
	if before.ID != "" {
		b, err := json.Marshal(before)
		if err != nil {
			return err
		}
		e.Before = b
		e.TargetID = before.ID
	}
	if after != nil {
		a, err := json.Marshal(after)
		if err != nil {
			return err
		}
		e.After = a
		e.TargetID = after.ID
		e.SourceEpisodes = after.EpisodeIDs
	}
	_, err := tx.Journal().Append(ctx, e)
	return err
}
