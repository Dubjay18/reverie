// Package engine is the public constructor for a reverie.Memory backed by a
// durable store. It lives outside the root package because the root defines
// the types that the store packages import.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/episodes"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/sqlite"
)

// MaxFutureSkew bounds how far ahead of the engine's clock an event may be
// stamped. A far-future time would keep its episode "recently active" forever.
const MaxFutureSkew = 5 * time.Minute

var ErrFutureEvent = errors.New("engine: event timestamp is too far in the future")

type Engine struct {
	st  store.Store
	now func() time.Time
}

var _ reverie.Memory = (*Engine)(nil)

// Open returns a Memory backed by a SQLite file.
func Open(path string) (*Engine, error) {
	st, err := sqlite.Open(path, sqlite.Config{})
	if err != nil {
		return nil, err
	}
	return New(st), nil
}

// New wraps an existing store. The engine takes ownership: Close closes it.
func New(st store.Store) *Engine { return &Engine{st: st, now: time.Now} }

func (e *Engine) Close() error { return e.st.Close() }

// Store exposes the store for tools that read it directly (CLI tail).
func (e *Engine) Store() store.Store { return e.st }

// Record validates every event first, so a bad event writes nothing, then
// writes each scope's events in one transaction. A repeated event ID is
// ignored. An event for a task that already ended is kept in the log (raw
// events are the truth) but does not change the closed episode; this is the
// normal case for async hooks. If a later scope fails, earlier scopes stay
// committed and the error says how many.
func (e *Engine) Record(ctx context.Context, events ...reverie.Event) error {
	limit := e.now().Add(MaxFutureSkew)
	for i, ev := range events {
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		if ev.At.After(limit) {
			return fmt.Errorf("event %d: %w", i, ErrFutureEvent)
		}
	}
	var order []reverie.Scope
	groups := map[reverie.Scope][]reverie.Event{}
	for _, ev := range events {
		if _, ok := groups[ev.Scope]; !ok {
			order = append(order, ev.Scope)
		}
		groups[ev.Scope] = append(groups[ev.Scope], ev)
	}
	for done, sc := range order {
		batch := groups[sc]
		err := e.st.For(sc).Tx(ctx, func(tx store.Tx) error {
			for _, ev := range batch {
				inserted, err := tx.Events().Append(ctx, ev)
				if err != nil {
					return err
				}
				if !inserted {
					continue
				}
				if _, err := episodes.Apply(ctx, tx, ev); err != nil && !errors.Is(err, episodes.ErrTaskClosed) {
					return err
				}
			}
			return nil
		})
		if err != nil {
			if done > 0 {
				return fmt.Errorf("scope %d of %d failed, %d earlier scope(s) already committed: %w", done+1, len(order), done, err)
			}
			return err
		}
	}
	return nil
}

// Sweep closes episodes idle for longer than idle as abandoned, in every
// scope, and returns how many it closed.
func (e *Engine) Sweep(ctx context.Context, now time.Time, idle time.Duration) (int, error) {
	scopes, err := e.st.Scopes(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sc := range scopes {
		err := e.st.For(sc).Tx(ctx, func(tx store.Tx) error {
			closed, err := episodes.Sweep(ctx, tx, now, idle)
			total += len(closed)
			return err
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

const recallEpisodes = 50

// Recall is the M1 stub: recent closed episodes, failures first, within the
// token budget. Hybrid retrieval replaces it in M2. The task argument is
// not used yet.
func (e *Engine) Recall(ctx context.Context, s reverie.Scope, task string, budget int) (reverie.ContextPack, error) {
	pack := reverie.ContextPack{Budget: budget}
	var eps []store.Episode
	err := e.st.For(s).View(ctx, func(tx store.Tx) (err error) {
		eps, err = tx.Episodes().Recent(ctx, recallEpisodes)
		return err
	})
	if err != nil {
		return pack, err
	}
	sort.SliceStable(eps, func(i, j int) bool { return bad(eps[i]) && !bad(eps[j]) })
	for _, ep := range eps {
		text := render(ep)
		tokens := len(text)/4 + 1
		if pack.Tokens+tokens > budget {
			pack.Dropped++
			continue
		}
		pack.Tokens += tokens
		pack.Episodes = append(pack.Episodes, reverie.PackItem{ID: ep.TaskID, Kind: "episode", Text: text, Tokens: tokens})
	}
	return pack, nil
}

func bad(ep store.Episode) bool { return ep.Outcome == store.Failure || ep.Outcome == store.Partial }

func render(ep store.Episode) string {
	title := ep.Title
	if title == "" {
		title = ep.TaskID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (%d events", title, ep.Outcome, ep.Events)
	if ep.Errors > 0 {
		fmt.Fprintf(&b, ", %d errors", ep.Errors)
	}
	b.WriteString(")")
	return b.String()
}
