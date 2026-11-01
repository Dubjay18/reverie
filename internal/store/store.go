// Package store defines the storage contract. A Store hands out Scoped
// handles; no method on a handle accepts a scope, so a query cannot be
// written without one and cannot reach another scope's rows.
package store

import (
	"context"
	"encoding/json"
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
	Facts() FactRepo
	Journal() JournalRepo
	Dreams() DreamRepo
}

type EventRepo interface {
	// Append stores ev. It reports false, with no error, when an event with
	// that ID already exists, so retried writes are harmless.
	Append(ctx context.Context, ev reverie.Event) (inserted bool, err error)
	List(ctx context.Context, q EventQuery) ([]reverie.Event, error)
	// UpdatePayload rewrites one event's payload. The log is append-only, so
	// this exists only for scrubbing secrets out of data stored before they
	// were redacted on the way in.
	UpdatePayload(ctx context.Context, id ulid.ULID, payload json.RawMessage) error
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
	// Unconsolidated returns closed episodes no dream has processed yet, most
	// important first.
	Unconsolidated(ctx context.Context, limit int) ([]Episode, error)
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
	// Delete removes an item from search (a superseded or forgotten fact).
	Delete(ctx context.Context, kind, refID string) error
	// SearchText matches any word of query (full-text, best first).
	SearchText(ctx context.Context, query string, limit int) ([]Hit, error)
	// SearchVector ranks embedded items by cosine similarity, best first.
	SearchVector(ctx context.Context, vec []float32, limit int) ([]Hit, error)
}

// FactQuery narrows Query and AsOf. Zero values mean no filter.
type FactQuery struct {
	Entity        string
	Relation      string
	OnlyContested bool
	OnlyPinned    bool
	Limit         int
}

// FactRepo stores facts. Reads return only what is currently believed
// (valid_to unset, status active or contested) unless they are AsOf; the
// filter lives here so a caller cannot forget it.
type FactRepo interface {
	// Save inserts or replaces a fact and its provenance.
	Save(ctx context.Context, f reverie.Fact) error
	Get(ctx context.Context, id string) (reverie.Fact, error) // any status; ErrNotFound if absent
	// Live returns the believed facts for one entity and relation, oldest first.
	Live(ctx context.Context, entity, relation string) ([]reverie.Fact, error)
	Query(ctx context.Context, q FactQuery) ([]reverie.Fact, error)
	// AsOf returns what was true at t, including facts since superseded.
	AsOf(ctx context.Context, q FactQuery, t time.Time) ([]reverie.Fact, error)
	// Related returns believed facts whose entity or value equals one of
	// names (case-insensitive): one hop of entity expansion.
	Related(ctx context.Context, names []string, limit int) ([]reverie.Fact, error)
}

// JournalEntry records one change with enough to undo it.
type JournalEntry struct {
	ID             int64
	RunID          string // empty for a manual change
	Phase          string
	Op             string
	TargetKind     string // "fact"
	TargetID       string
	Before, After  json.RawMessage // the target before and after; nil when it did not exist
	Rationale      string
	SourceEpisodes []string
	Tokens         int
	At             time.Time
	Reverted       bool
}

type JournalRepo interface {
	Append(ctx context.Context, e JournalEntry) (int64, error)
	Get(ctx context.Context, id int64) (JournalEntry, error) // ErrNotFound if absent
	ByRun(ctx context.Context, runID string) ([]JournalEntry, error)
	// Recent returns the newest entries first.
	Recent(ctx context.Context, limit int) ([]JournalEntry, error)
	MarkReverted(ctx context.Context, id int64) error
}

// Dream run statuses.
const (
	RunRunning = "running"
	RunDone    = "done"
	RunFailed  = "failed"
	RunAborted = "aborted" // the process died or the lease lapsed before it finished
)

type DreamRun struct {
	ID        string
	Trigger   string // manual | pressure | idle | cron | hook
	Status    string
	StartedAt time.Time
	EndedAt   time.Time
	Budget    int // tokens allowed
	Spent     int // tokens used
	Report    json.RawMessage
}

// DreamRepo tracks dream runs and the per-scope lease that keeps two dreams
// from running at once.
type DreamRepo interface {
	// Acquire takes the scope's lease for ttl. It reports false, with no
	// error, when another run holds an unexpired lease. On success any run
	// still marked running is marked aborted: if we hold the lease, nobody
	// else is running.
	Acquire(ctx context.Context, runID string, ttl time.Duration, now time.Time) (bool, error)
	// Renew extends an held lease by ttl from now. Reports false when runID no longer holds the lease.
	Renew(ctx context.Context, runID string, ttl time.Duration, now time.Time) (bool, error)
	Release(ctx context.Context, runID string) error
	StartRun(ctx context.Context, r DreamRun) error
	FinishRun(ctx context.Context, r DreamRun) error
	GetRun(ctx context.Context, id string) (DreamRun, error) // ErrNotFound if absent
	Runs(ctx context.Context, limit int) ([]DreamRun, error) // newest first
}
