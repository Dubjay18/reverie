package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/ontology"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/telemetry"
)

const DefaultLeaseTTL = 5 * time.Minute
const DefaultDreamBudget = 4000

var (
	ErrLeaseBusy = errors.New("dream: another dream run already holds the lease for this scope")
	ErrLeaseLost = errors.New("dream: lease expired or was stolen before commit")
)

// Report summarises what a dream run did. It is stored as JSON on dream_runs.report.
type Report struct {
	RunID      string   `json:"run_id"`
	Trigger    string   `json:"trigger"`
	Status     string   `json:"status"`
	Episodes   int      `json:"episodes"`
	EpisodeIDs []string `json:"episode_ids,omitempty"`
	Learned    int      `json:"learned"`
	Merged     int      `json:"merged"`
	Superseded int      `json:"superseded"`
	Contested  int      `json:"contested"`
	Forgotten  int      `json:"forgotten"`
	Rejected   int      `json:"rejected"`
	// Procedural memory (v1.1).
	SkillsProposed    int      `json:"skills_proposed,omitempty"`
	InsightsProposed  int      `json:"insights_proposed,omitempty"`
	InsightsConfirmed int      `json:"insights_confirmed,omitempty"`
	TokensSpent       int      `json:"tokens_spent"`
	TokensBudget      int      `json:"tokens_budget"`
	Skipped           []string `json:"skipped,omitempty"`
}

func (r Report) Summary() string {
	s := fmt.Sprintf("dream %s (%s): consolidated %d episode(s), learned %d fact(s), merged %d, superseded %d, contested %d, rejected %d (tokens %d/%d)",
		r.RunID, r.Trigger, r.Episodes, r.Learned, r.Merged, r.Superseded, r.Contested, r.Rejected, r.TokensSpent, r.TokensBudget)
	if r.SkillsProposed+r.InsightsProposed+r.InsightsConfirmed > 0 {
		s += fmt.Sprintf("; proposed %d skill(s), drew %d insight(s), confirmed %d", r.SkillsProposed, r.InsightsProposed, r.InsightsConfirmed)
	}
	return s
}

// Config configures a single dream run.
type Config struct {
	Trigger  string // manual | pressure | idle | hook | cron
	Budget   int    // token cap; default DefaultDreamBudget
	LeaseTTL time.Duration
	Now      time.Time
	LLM      llm.LLM
	Ontology *ontology.Ontology
	Tracer   *telemetry.Tracer
	// Recombine adds the Recombine phase (speculative insights). It is off by
	// default: insights ship behind eval evidence (PRD M6).
	Recombine  bool
	Phases     []Phase                 // optional override; defaults to Replay -> Distill -> ReconcileReflect -> Synthesize [-> Recombine] -> Forget
	BeforeSave func(tx store.Tx) error // test hook to inject mid-commit failure
}

// Run executes one dream cycle for sc:
//  1. Acquires the per-scope lease and records a running DreamRun.
//  2. Runs phases against a read-only ReadView (Replay -> Distill -> ReconcileReflect).
//  3. Runs the Validate gate (internal/guard.ValidateProposal) over every proposal.
//  4. Commits accepted proposals, journal rows, consolidated episode flags, and
//     the finished DreamRun in a single write transaction.
func Run(ctx context.Context, sc store.Scoped, cfg Config) (Report, error) {
	now := cfg.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	wallStart := time.Now()
	clockNow := func() time.Time {
		return now.Add(time.Since(wallStart))
	}
	budgetLimit := cfg.Budget
	if budgetLimit <= 0 {
		budgetLimit = DefaultDreamBudget
	}
	ttl := cfg.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	onto := cfg.Ontology
	if onto == nil {
		onto = ontology.Default()
	}
	trigger := cfg.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	runID := facts.NewRunID("dream")
	tr := cfg.Tracer
	if tr == nil {
		tr = telemetry.NewTracer(nil)
	}
	ctx, runSpan := tr.Start(ctx, "dream.run", map[string]any{
		"operation": "dream",
		"trigger":   trigger,
		"run_id":    runID,
		"budget":    budgetLimit,
	})
	var runErr error
	defer func() {
		runSpan.End(runErr)
	}()

	var acquired bool
	if err := sc.Tx(ctx, func(tx store.Tx) error {
		ok, err := tx.Dreams().Acquire(ctx, runID, ttl, now)
		if err != nil || !ok {
			return err
		}
		acquired = true
		return tx.Dreams().StartRun(ctx, store.DreamRun{
			ID:        runID,
			Trigger:   trigger,
			Status:    store.RunRunning,
			StartedAt: now,
			Budget:    budgetLimit,
		})
	}); err != nil {
		return Report{}, err
	}
	if !acquired {
		return Report{}, ErrLeaseBusy
	}

	// Ensure lease is released and run marked failed if an unhandled error occurs
	// while we still hold the lease (never clobbering a run already marked aborted).
	committed := false
	defer func() {
		if committed {
			return
		}
		_ = sc.Tx(context.Background(), func(tx store.Tx) error {
			if cur, err := tx.Dreams().GetRun(context.Background(), runID); err == nil && cur.Status == store.RunRunning {
				_ = tx.Dreams().FinishRun(context.Background(), store.DreamRun{
					ID:      runID,
					Status:  store.RunFailed,
					EndedAt: clockNow(),
				})
			}
			return tx.Dreams().Release(context.Background(), runID)
		})
	}()

	st := &State{
		Now:            now,
		Budget:         NewBudget(budgetLimit),
		EpisodeOrigins: map[string][]reverie.Origin{},
	}
	model := telemetry.WrapLLM(cfg.LLM, tr)
	phases := cfg.Phases
	if len(phases) == 0 {
		phases = []Phase{
			ReplayPhase{},
			DistillPhase{LLM: model, Ontology: onto},
			ReconcileReflectPhase{Ontology: onto},
			SynthesizePhase{},
		}
		if cfg.Recombine {
			phases = append(phases, RecombinePhase{LLM: model})
		}
		phases = append(phases, ForgetPhase{Ontology: onto})
	}
	view := NewReadView(sc)
	renewLease := func() error {
		var ok bool
		if err := sc.Tx(ctx, func(tx store.Tx) error {
			var err error
			ok, err = tx.Dreams().Renew(ctx, runID, ttl, clockNow())
			return err
		}); err != nil {
			return err
		}
		if !ok {
			return ErrLeaseLost
		}
		return nil
	}
	for _, ph := range phases {
		if err := ctx.Err(); err != nil {
			runErr = err
			return Report{}, err
		}
		if err := renewLease(); err != nil {
			runErr = err
			return Report{}, err
		}
		beforeSpent := st.Budget.Spent()
		pctx, phSpan := tr.Start(ctx, "dream.phase."+ph.Name(), map[string]any{
			"operation": "dream_phase",
			"phase":     ph.Name(),
			"run_id":    runID,
		})
		err := ph.Run(pctx, view, st)
		phSpan.SetAttr("tokens_spent", st.Budget.Spent()-beforeSpent)
		phSpan.End(err)
		if err != nil {
			runErr = err
			return Report{}, err
		}
	}
	if err := renewLease(); err != nil {
		return Report{}, err
	}

	// Single atomic commit transaction: Validate gate -> Assert + save + journal
	// -> mark episodes consolidated -> finish DreamRun -> release lease.
	rep := Report{
		RunID:        runID,
		Trigger:      trigger,
		Status:       store.RunDone,
		TokensSpent:  st.Budget.Spent(),
		TokensBudget: budgetLimit,
		Skipped:      st.SkippedPhases,
	}
	err := sc.Tx(ctx, func(tx store.Tx) error {
		ok, err := tx.Dreams().Renew(ctx, runID, ttl, clockNow())
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseLost
		}
		for _, p := range st.Proposals {
			if p.Op == "forget" {
				targetID := p.TargetFactID
				if targetID == "" {
					targetID = p.Fact.ID
				}
				cur, err := tx.Facts().Get(ctx, targetID)
				if err != nil {
					rep.Rejected++
					continue
				}
				// Always run the deterministic Validate gate last before writing,
				// including Forget proposals.
				if err := guard.ValidateForget(cur); err != nil {
					rep.Rejected++
					continue
				}
				if err := facts.Forget(ctx, tx, targetID, reverie.SourceDistilled, now, runID); err != nil {
					rep.Rejected++
					continue
				}
				rep.Forgotten++
				continue
			}
			live, err := tx.Facts().Live(ctx, p.Fact.Entity, p.Fact.Relation)
			if err != nil {
				return err
			}
			// Always run the deterministic Validate gate last before writing.
			validated, err := guard.ValidateProposal(p.Fact, live, st.EpisodeOrigins)
			if err != nil {
				rep.Rejected++
				continue
			}
			res, err := facts.Remember(ctx, tx, onto, validated, now, runID, facts.WithJournalMeta(p.Phase, p.Tokens))
			if err != nil {
				if errors.Is(err, facts.ErrInstruction) {
					rep.Rejected++
					continue
				}
				return err
			}
			switch res.Op {
			case facts.OpInsert, facts.OpHistorical:
				rep.Learned++
			case facts.OpMerge:
				rep.Merged++
			case facts.OpSupersede:
				rep.Learned++
				rep.Superseded += len(res.Updates)
			case facts.OpContest:
				rep.Learned++
				rep.Contested++
			case facts.OpReject:
				rep.Rejected++
			}
		}

		if err := commitProcedural(ctx, tx, st, runID, now, &rep); err != nil {
			return err
		}

		// Mark replayed episodes consolidated and journal the change so RevertRun
		// (and reverting that revert) restores both facts and episodes.
		for _, cl := range st.Clusters {
			for _, ep := range cl.Episodes {
				cur, err := tx.Episodes().Get(ctx, ep.TaskID)
				if err != nil {
					return err
				}
				beforeBytes, err := json.Marshal(cur)
				if err != nil {
					return err
				}
				cur.Consolidated = true
				cur.Tier = store.Warm
				if err := tx.Episodes().Save(ctx, cur); err != nil {
					return err
				}
				afterBytes, err := json.Marshal(cur)
				if err != nil {
					return err
				}
				if _, err := tx.Journal().Append(ctx, store.JournalEntry{
					RunID:          runID,
					Phase:          "replay",
					Op:             "consolidate",
					TargetKind:     "episode",
					TargetID:       ep.TaskID,
					Before:         beforeBytes,
					After:          afterBytes,
					Rationale:      "consolidated by dream",
					SourceEpisodes: []string{ep.TaskID},
					At:             now,
				}); err != nil {
					return err
				}
				rep.Episodes++
				rep.EpisodeIDs = append(rep.EpisodeIDs, ep.TaskID)
			}
		}

		if cfg.BeforeSave != nil {
			if err := cfg.BeforeSave(tx); err != nil {
				return err
			}
		}

		rawRep, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		if err := tx.Dreams().FinishRun(ctx, store.DreamRun{
			ID:      runID,
			Status:  store.RunDone,
			EndedAt: clockNow(),
			Spent:   rep.TokensSpent,
			Report:  rawRep,
		}); err != nil {
			return err
		}
		return tx.Dreams().Release(ctx, runID)
	})
	if err != nil {
		runErr = err
		return Report{}, err
	}
	committed = true
	runSpan.SetAttr("episodes", rep.Episodes)
	runSpan.SetAttr("learned", rep.Learned)
	runSpan.SetAttr("merged", rep.Merged)
	runSpan.SetAttr("superseded", rep.Superseded)
	runSpan.SetAttr("contested", rep.Contested)
	runSpan.SetAttr("forgotten", rep.Forgotten)
	runSpan.SetAttr("rejected", rep.Rejected)
	runSpan.SetAttr("tokens_spent", rep.TokensSpent)
	return rep, nil
}

// commitProcedural runs the Validate gate over skill drafts, insights and
// insight confirmations and writes the accepted ones with their journal rows,
// inside the dream's commit transaction.
func commitProcedural(ctx context.Context, tx store.Tx, st *State, runID string, now time.Time, rep *Report) error {
	for _, sp := range st.Skills {
		sk, err := guard.ValidateSkill(sp.Skill, st.EpisodeOrigins)
		if err != nil {
			rep.Rejected++
			continue
		}
		// The version must still be the next one; nothing may be overwritten.
		latest, err := tx.Skills().Get(ctx, sk.ID, 0)
		switch {
		case err == nil && latest.Version >= sk.Version:
			rep.Rejected++
			continue
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return err
		}
		if err := skills.SaveCandidate(ctx, tx, sk, runID, "synthesize", sp.Rationale, now); err != nil {
			if errors.Is(err, store.ErrScopeMismatch) {
				rep.Rejected++
				continue
			}
			return err
		}
		rep.SkillsProposed++
	}
	for _, ip := range st.Insights {
		in, err := guard.ValidateInsight(ip.Insight, st.EpisodeOrigins)
		if err != nil {
			rep.Rejected++
			continue
		}
		if _, err := tx.Insights().Get(ctx, in.ID); err == nil {
			rep.Rejected++
			continue
		}
		if err := saveInsight(ctx, tx, nil, in, runID, "recombine", "create", ip.Rationale, ip.Tokens, now); err != nil {
			return err
		}
		rep.InsightsProposed++
	}
	for _, c := range st.Confirmations {
		cur, err := tx.Insights().Get(ctx, c.InsightID)
		if err != nil {
			rep.Rejected++
			continue
		}
		if err := guard.ValidateInsightConfirmation(cur, c.EpisodeID, st.EpisodeOrigins); err != nil {
			rep.Rejected++
			continue
		}
		next := cur
		next.Status = store.InsightConfirmed
		next.ConfirmedBy = append(append([]string(nil), cur.ConfirmedBy...), c.EpisodeID)
		next.ConfirmedAt = now
		if err := saveInsight(ctx, tx, &cur, next, runID, "recombine", "confirm", "confirmed by later episode "+c.EpisodeID, 0, now); err != nil {
			return err
		}
		rep.InsightsConfirmed++
	}
	return nil
}

// saveInsight writes an insight and its journal row. Insights live in their
// own table: nothing here, or anywhere else, turns one into a fact.
func saveInsight(ctx context.Context, tx store.Tx, before *store.Insight, after store.Insight, runID, phase, op, why string, tokens int, now time.Time) error {
	e := store.JournalEntry{RunID: runID, Phase: phase, Op: op, TargetKind: facts.KindInsight, TargetID: after.ID,
		Rationale: why, SourceEpisodes: after.EpisodeIDs, Tokens: tokens, At: now}
	if before != nil {
		b, err := json.Marshal(before)
		if err != nil {
			return err
		}
		e.Before = b
	}
	a, err := json.Marshal(after)
	if err != nil {
		return err
	}
	e.After = a
	if err := tx.Insights().Save(ctx, after); err != nil {
		return err
	}
	_, err = tx.Journal().Append(ctx, e)
	return err
}
