// Package episodes projects events onto episodes. All state lives in the
// store, so a restart cannot reopen a closed task and nothing is shared
// between goroutines.
package episodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

// ErrTaskClosed is returned for an event whose task already ended. The event
// is not applied; the caller's transaction should roll back.
var ErrTaskClosed = errors.New("episodes: task is already closed")

// Apply folds ev into its episode inside tx and returns the new state.
// Out-of-order events are accepted: bounds are min/max, counters are sums.
func Apply(ctx context.Context, tx store.Tx, ev reverie.Event) (store.Episode, error) {
	ep, err := tx.Episodes().Get(ctx, ev.TaskID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ep = store.Episode{
			TaskID: ev.TaskID, FirstEvent: ev.ID, LastEvent: ev.ID,
			StartedAt: ev.At, EndedAt: ev.At, Tier: store.Hot,
			Implicit: ev.Kind != reverie.TaskStarted,
		}
	case err != nil:
		return store.Episode{}, err
	case ep.Closed:
		return store.Episode{}, fmt.Errorf("%w: %s", ErrTaskClosed, ev.TaskID)
	}

	if ev.ID.Compare(ep.FirstEvent) < 0 {
		ep.FirstEvent = ev.ID
	}
	if ev.ID.Compare(ep.LastEvent) > 0 {
		ep.LastEvent = ev.ID
	}
	if ev.At.Before(ep.StartedAt) {
		ep.StartedAt = ev.At
	}
	if ev.At.After(ep.EndedAt) {
		ep.EndedAt = ev.At
	}
	ep.Events++

	switch ev.Kind {
	case reverie.TaskStarted:
		if ep.Title == "" {
			ep.Title = stringField(ev.Payload, "description")
		}
		ep.Implicit = false
	case reverie.ToolCalled:
		ep.ToolCalls++
	case reverie.ToolResult:
		if hasField(ev.Payload, "error") {
			ep.Errors++
		}
	case reverie.Feedback:
		ep.Feedback++
	case reverie.TaskEnded:
		finish(&ep, parseOutcome(stringField(ev.Payload, "outcome")))
	}
	return ep, tx.Episodes().Save(ctx, ep)
}

// Sweep closes open episodes idle since before now-idle as abandoned.
func Sweep(ctx context.Context, tx store.Tx, now time.Time, idle time.Duration) ([]store.Episode, error) {
	open, err := tx.Episodes().Open(ctx)
	if err != nil {
		return nil, err
	}
	var closed []store.Episode
	for _, ep := range open {
		if now.Sub(ep.EndedAt) <= idle {
			continue
		}
		finish(&ep, store.Abandoned)
		if err := tx.Episodes().Save(ctx, ep); err != nil {
			return nil, err
		}
		closed = append(closed, ep)
	}
	return closed, nil
}

func finish(ep *store.Episode, o store.Outcome) {
	ep.Closed, ep.Outcome = true, o
	ep.Importance = importance(*ep)
}

// importance is a cheap heuristic set at close; dreams refine it later.
func importance(ep store.Episode) float64 {
	v := 0.5
	switch ep.Outcome {
	case store.Failure:
		v += 0.3
	case store.Partial:
		v += 0.15
	case store.Abandoned:
		v -= 0.2
	}
	if ep.Errors > 0 {
		v += 0.1
	}
	if ep.Feedback > 0 {
		v += 0.2
	}
	if ep.ToolCalls >= 10 {
		v += 0.1
	}
	return min(max(v, 0), 1)
}

// parseOutcome treats an unknown or missing outcome as partial: the task
// ended, but we cannot claim it succeeded.
func parseOutcome(s string) store.Outcome {
	switch o := store.Outcome(s); o {
	case store.Success, store.Failure, store.Partial:
		return o
	}
	return store.Partial
}

func stringField(payload []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func hasField(payload []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return false
	}
	v, ok := m[key]
	return ok && v != nil
}
