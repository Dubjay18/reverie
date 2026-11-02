package facts

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
)

const (
	KindSkill   = "skill"   // journal target "<id>@v<n>"
	KindInsight = "insight" // journal target "<id>"
)

// revertSkill puts a skill version's status back. Use counters are kept:
// they record outcomes that really happened, not a belief to undo. It
// refuses when the status changed after the entry, as for facts.
func revertSkill(ctx context.Context, tx store.Tx, runID string, e store.JournalEntry, now time.Time) error {
	id, v, ok := strings.Cut(e.TargetID, "@v")
	n, err := strconv.Atoi(v)
	if !ok || err != nil {
		return fmt.Errorf("facts: journal entry %d: bad skill target %q", e.ID, e.TargetID)
	}
	cur, err := tx.Skills().Get(ctx, id, n)
	if err != nil {
		return fmt.Errorf("facts: journal entry %d: skill %s: %w", e.ID, e.TargetID, err)
	}
	if len(e.After) > 0 {
		var after store.Skill
		if err := json.Unmarshal(e.After, &after); err != nil {
			return fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
		}
		if cur.Status != after.Status {
			return fmt.Errorf("%w (skill %s%s)", ErrRevertConflict, e.TargetID, laterRun(ctx, tx, e))
		}
	}
	target := cur
	target.Status = store.SkillDeprecated // undoing a create withdraws the skill; nothing is deleted
	if len(e.Before) > 0 {
		var before store.Skill
		if err := json.Unmarshal(e.Before, &before); err != nil {
			return fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
		}
		target.Status = before.Status
	}
	target.UpdatedAt = now
	if err := tx.Skills().Save(ctx, target); err != nil {
		return err
	}
	return journalRevert(ctx, tx, runID, e, KindSkill, cur, target, now)
}

// revertInsight restores an insight as it was, or rejects one the run created.
func revertInsight(ctx context.Context, tx store.Tx, runID string, e store.JournalEntry, now time.Time) error {
	cur, err := tx.Insights().Get(ctx, e.TargetID)
	if err != nil {
		return fmt.Errorf("facts: journal entry %d: insight %s: %w", e.ID, e.TargetID, err)
	}
	if len(e.After) > 0 {
		var after store.Insight
		if err := json.Unmarshal(e.After, &after); err != nil {
			return fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
		}
		if cur.Status != after.Status || cur.Body != after.Body {
			return fmt.Errorf("%w (insight %s%s)", ErrRevertConflict, e.TargetID, laterRun(ctx, tx, e))
		}
	}
	target := cur
	target.Status = store.InsightRejected
	if len(e.Before) > 0 {
		if err := json.Unmarshal(e.Before, &target); err != nil {
			return fmt.Errorf("facts: journal entry %d: %w", e.ID, err)
		}
	}
	if err := tx.Insights().Save(ctx, target); err != nil {
		return err
	}
	return journalRevert(ctx, tx, runID, e, KindInsight, cur, target, now)
}

func journalRevert(ctx context.Context, tx store.Tx, runID string, e store.JournalEntry, kind string, before, after any, now time.Time) error {
	if err := tx.Journal().MarkReverted(ctx, e.ID); err != nil {
		return err
	}
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	a, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.Journal().Append(ctx, store.JournalEntry{
		RunID: "revert:" + runID, Op: "revert", TargetKind: kind, TargetID: e.TargetID,
		Before: b, After: a, Rationale: "undo of " + e.Op, SourceEpisodes: e.SourceEpisodes, At: now,
	})
	return err
}
