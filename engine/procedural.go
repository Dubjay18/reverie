package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

var ErrInsightState = errors.New("engine: only a speculative insight can be confirmed or rejected")

// Skills lists the skills s can see: its project's shared skills and those
// narrowed to its agent.
func (e *Engine) Skills(ctx context.Context, s reverie.Scope, q store.SkillQuery) (out []store.Skill, err error) {
	err = e.st.For(s).View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Skills().List(ctx, q)
		return err
	})
	return out, err
}

// SkillOutcome reports one use of the skill named by ref (the "skill:<id>@v<n>"
// in a pack citation) and applies the promotion and deprecation gate.
func (e *Engine) SkillOutcome(ctx context.Context, s reverie.Scope, ref string, success bool) (sk store.Skill, tr skills.Transition, err error) {
	id, v, err := skills.ParseRef(ref)
	if err != nil {
		return sk, tr, err
	}
	err = e.st.For(s).Tx(ctx, func(tx store.Tx) (err error) {
		sk, tr, err = skills.RecordUse(ctx, tx, id, v, success, e.now().UTC(), skills.NewRunID())
		return err
	})
	return sk, tr, err
}

// SkillRollback withdraws the active version of a skill and reactivates the
// previous proven one. It returns the run id that Revert takes to undo it.
func (e *Engine) SkillRollback(ctx context.Context, s reverie.Scope, id string) (restored store.Skill, runID string, err error) {
	if id, _, err = skills.ParseRef(id); err != nil {
		return restored, "", err
	}
	runID = skills.NewRunID()
	err = e.st.For(s).Tx(ctx, func(tx store.Tx) (err error) {
		restored, err = skills.Rollback(ctx, tx, id, e.now().UTC(), runID)
		return err
	})
	return restored, runID, err
}

// Insights lists insights in s, newest first.
func (e *Engine) Insights(ctx context.Context, s reverie.Scope, q store.InsightQuery) (out []store.Insight, err error) {
	err = e.st.For(s).View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Insights().List(ctx, q)
		return err
	})
	return out, err
}

// ConfirmInsight is the user confirming a speculative insight, so Recall may
// show it. It stays an insight: confirmation never turns it into a fact.
func (e *Engine) ConfirmInsight(ctx context.Context, s reverie.Scope, id string) (string, error) {
	return e.setInsight(ctx, s, id, store.InsightConfirmed)
}

// RejectInsight withdraws a speculative insight.
func (e *Engine) RejectInsight(ctx context.Context, s reverie.Scope, id string) (string, error) {
	return e.setInsight(ctx, s, id, store.InsightRejected)
}

func (e *Engine) setInsight(ctx context.Context, s reverie.Scope, id string, to store.InsightStatus) (string, error) {
	runID := "insight:" + ulid.Make().String()
	now := e.now().UTC()
	err := e.st.For(s).Tx(ctx, func(tx store.Tx) error {
		cur, err := tx.Insights().Get(ctx, id)
		if err != nil {
			return err
		}
		if cur.Status != store.InsightSpeculative {
			return ErrInsightState
		}
		next := cur
		next.Status = to
		if to == store.InsightConfirmed {
			next.ConfirmedBy = append(slices.Clone(cur.ConfirmedBy), "user")
			next.ConfirmedAt = now
		}
		b, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		a, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := tx.Insights().Save(ctx, next); err != nil {
			return err
		}
		op := "confirm"
		if to == store.InsightRejected {
			op = "reject_insight" // "reject" is skipped by RevertRun; this one is undoable
		}
		_, err = tx.Journal().Append(ctx, store.JournalEntry{RunID: runID, Op: op, TargetKind: facts.KindInsight, TargetID: id,
			Before: b, After: a, Rationale: "by the user", SourceEpisodes: cur.EpisodeIDs, At: now})
		return err
	})
	return runID, err
}
