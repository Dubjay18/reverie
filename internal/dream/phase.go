package dream

import (
	"context"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

// ReadView is the compile-time read-only handle passed to every Phase.
// It exposes read queries over episodes, events, facts, and the search index,
// and cannot obtain a store.Tx or mutate any repository.
type ReadView interface {
	Unconsolidated(ctx context.Context, limit int) ([]store.Episode, error)
	Events(ctx context.Context, q store.EventQuery) ([]reverie.Event, error)
	LiveFacts(ctx context.Context, entity, relation string) ([]reverie.Fact, error)
	QueryFacts(ctx context.Context, q store.FactQuery) ([]reverie.Fact, error)
	// Recent returns closed episodes, newest first (consolidated or not).
	Recent(ctx context.Context, limit int) ([]store.Episode, error)
	SkillVersions(ctx context.Context, id string) ([]store.Skill, error)
	Insights(ctx context.Context, q store.InsightQuery) ([]store.Insight, error)
}

type txReadView struct {
	sc store.Scoped
}

// NewReadView wraps a scoped store handle in a ReadView that only runs
// read-only View transactions and never exposes store.Tx to the caller.
func NewReadView(sc store.Scoped) ReadView { return txReadView{sc: sc} }

func (v txReadView) Unconsolidated(ctx context.Context, limit int) (out []store.Episode, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Episodes().Unconsolidated(ctx, limit)
		return err
	})
	return out, err
}

func (v txReadView) Events(ctx context.Context, q store.EventQuery) (out []reverie.Event, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Events().List(ctx, q)
		return err
	})
	return out, err
}

func (v txReadView) LiveFacts(ctx context.Context, entity, relation string) (out []reverie.Fact, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Facts().Live(ctx, entity, relation)
		return err
	})
	return out, err
}

func (v txReadView) QueryFacts(ctx context.Context, q store.FactQuery) (out []reverie.Fact, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Facts().Query(ctx, q)
		return err
	})
	return out, err
}

func (v txReadView) Recent(ctx context.Context, limit int) (out []store.Episode, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Episodes().Recent(ctx, limit)
		return err
	})
	return out, err
}

func (v txReadView) SkillVersions(ctx context.Context, id string) (out []store.Skill, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Skills().Versions(ctx, id)
		return err
	})
	return out, err
}

func (v txReadView) Insights(ctx context.Context, q store.InsightQuery) (out []store.Insight, err error) {
	err = v.sc.View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Insights().List(ctx, q)
		return err
	})
	return out, err
}

// Proposal is a candidate fact mutation produced by a Phase before Validate
// and Commit.
type Proposal struct {
	Phase        string
	Op           string       // ""/"assert" (default) or "forget" (tombstone TargetFactID)
	TargetFactID string       // set when Op == "forget"
	Fact         reverie.Fact // proposed fact (SourceDistilled or SourceInferred)
	Rationale    string
	Tokens       int
	AnchorTime   time.Time // event timestamp that anchored relative time normalization
}

// Cluster groups related unconsolidated episodes selected by Replay.
type Cluster struct {
	Key      string
	Episodes []store.Episode
	Events   map[string][]reverie.Event // taskID -> non-recalled events
}

// State flows from phase to phase during a single dream run.
type State struct {
	Now            time.Time
	Budget         *Budget
	Clusters       []Cluster
	EpisodeOrigins map[string][]reverie.Origin // taskID -> origins in that episode
	Proposals      []Proposal
	Skills         []SkillProposal   // candidate skills from Synthesize
	Insights       []InsightProposal // speculative insights from Recombine
	Confirmations  []Confirmation    // later episodes that confirm a speculative insight
	SkippedPhases  []string
}

// SkillProposal is a candidate skill version drafted by Synthesize.
type SkillProposal struct {
	Skill     store.Skill
	Rationale string
}

// InsightProposal is a speculative insight drawn by Recombine.
type InsightProposal struct {
	Insight   store.Insight
	Rationale string
	Tokens    int
}

// Confirmation says a later episode bears out a speculative insight.
type Confirmation struct {
	InsightID string
	EpisodeID string
}

// Phase is one step of a dream run. It receives a ReadView (never a store.Tx)
// and reads/appends to State.
type Phase interface {
	Name() string
	Run(ctx context.Context, view ReadView, st *State) error
}
