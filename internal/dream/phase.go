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

// Proposal is a candidate fact mutation produced by a Phase before Validate
// and Commit.
type Proposal struct {
	Phase      string
	Fact       reverie.Fact // proposed fact (SourceDistilled or SourceInferred)
	Rationale  string
	Tokens     int
	AnchorTime time.Time // event timestamp that anchored relative time normalization
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
	SkippedPhases  []string
}

// Phase is one step of a dream run. It receives a ReadView (never a store.Tx)
// and reads/appends to State.
type Phase interface {
	Name() string
	Run(ctx context.Context, view ReadView, st *State) error
}
