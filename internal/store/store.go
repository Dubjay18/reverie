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
	Limit  int       // 0 means DefaultLimit
}

const (
	DefaultLimit = 1000
	MaxLimit     = 10000
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
