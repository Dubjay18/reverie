// Package store defines the storage contract. A Store hands out Scoped
// handles; no method on a handle accepts a scope, so a query cannot be
// written without one and cannot reach another scope's rows.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/oklog/ulid/v2"
)

var (
	ErrNotFound         = errors.New("store: not found")
	ErrScopeMismatch    = errors.New("store: event scope does not match the scoped handle")
	ErrEmbedderMismatch = errors.New("store: database was built with a different embedding model or dimension")
)

type Store interface {
	// For binds a scope. It is the only place a scope is accepted.
	For(s reverie.Scope) Scoped
	// Scopes lists every scope with data. It exists for background
	// maintenance (idle sweeps, dreams); request paths must not use it.
	Scopes(ctx context.Context) ([]reverie.Scope, error)
	Close() error
}

type Scoped interface {
	// Tx runs fn in one write transaction: commit if it returns nil, roll
	// back otherwise. Do not start another Tx, View or Scopes call from
	// inside fn: writers are serialised, so it waits on itself until ctx ends.
	Tx(ctx context.Context, fn func(Tx) error) error
	// View runs fn in a read-only transaction that does not block writers.
	// Any write attempted through the Tx fails.
	View(ctx context.Context, fn func(Tx) error) error
}

type Tx interface {
	Events() EventRepo
	Episodes() EpisodeRepo
	Index() IndexRepo
}

type EventRepo interface {
	// Append stores ev. It reports false, with no error, when an event with
	// that ID already exists, so retried writes are harmless.
	Append(ctx context.Context, ev reverie.Event) (inserted bool, err error)
	List(ctx context.Context, q EventQuery) ([]reverie.Event, error)
}

// EventQuery selects events in ULID (time) order. Zero values mean no filter.
type EventQuery struct {
	TaskID string
	After  ulid.ULID // exclusive
	From   ulid.ULID // inclusive lower bound
	To     ulid.ULID // inclusive upper bound
	Limit  int       // 0 means DefaultLimit; clamped to MaxLimit
}

const (
	DefaultLimit     = 1000
	MaxLimit         = 10000
	MaxEmbedFailures = 3
)

type EpisodeRepo interface {
	Get(ctx context.Context, taskID string) (Episode, error) // ErrNotFound if absent
	Save(ctx context.Context, ep Episode) error              // upsert
	Open(ctx context.Context) ([]Episode, error)
	// Recent returns closed episodes, newest first.
	Recent(ctx context.Context, limit int) ([]Episode, error)
}

type Outcome string

const (
	Success   Outcome = "success"
	Failure   Outcome = "failure"
	Partial   Outcome = "partial"
	Abandoned Outcome = "abandoned"
	Unknown   Outcome = "unknown" // ended, but the source gave no verdict
)

type Tier string

const (
	Hot  Tier = "hot"
	Warm Tier = "warm"
	Cold Tier = "cold"
)

// Episode is the projection of one task's events.
type Episode struct {
	TaskID       string
	Title        string
	Goal         string // first thing the user said in the task, clipped
	LastError    string // most recent tool error, clipped
	FirstEvent   ulid.ULID
	LastEvent    ulid.ULID
	StartedAt    time.Time
	EndedAt      time.Time
	Closed       bool
	Outcome      Outcome // empty while open
	Events       int
	ToolCalls    int
	Errors       int
	Feedback     int
	Implicit     bool // opened by a non-start event
	Importance   float64
	Tier         Tier
	Consolidated bool
}

// Item is one retrievable thing: a piece of text, optionally with a vector.
// Kind and RefID point back at the thing it describes (an episode's task id).
type Item struct {
	Kind      string
	RefID     string
	Text      string
	Embedding []float32 // nil until embedded; the length must match the store's pinned dimension
}

type Hit struct {
	Kind  string
	RefID string
}

// IndexRepo holds the derived retrieval index. It is rebuildable from the
// typed tables and never the source of truth. Search returns ranked lists;
// fusing them is the caller's job (internal/rank).
type IndexRepo interface {
	// Upsert replaces the item's text and clears its embedding and failure count.
	Upsert(ctx context.Context, it Item) error
	// SetEmbedding stores vec for the item whose text is still text. It
	// reports false when the item changed or vanished since the vector was
	// computed, so a stale vector never lands on newer text.
	SetEmbedding(ctx context.Context, kind, refID, text string, vec []float32) (applied bool, err error)
	// Pending lists items waiting for an embedding, skipping any that failed
	// MaxEmbedFailures times.
	Pending(ctx context.Context, limit int) ([]Item, error)
	// MarkEmbedFailed counts one failed attempt against the item.
	MarkEmbedFailed(ctx context.Context, kind, refID string) error
	// SearchText matches any word of query (full-text, best first).
	SearchText(ctx context.Context, query string, limit int) ([]Hit, error)
	// SearchVector ranks embedded items by cosine similarity, best first.
	SearchVector(ctx context.Context, vec []float32, limit int) ([]Hit, error)
}
