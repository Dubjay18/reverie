// Package episodes projects events onto episodes. All state lives in the
// store, so a restart cannot reopen a closed task and nothing is shared
// between goroutines.
package episodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/guard"
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
			ep.Title = clip(stringField(ev.Payload, "description"))
		}
		ep.Implicit = false
	case reverie.ToolCalled:
		ep.ToolCalls++
	case reverie.Message:
		if ep.Goal == "" && ev.Origin == reverie.OriginUser {
			ep.Goal = clip(stringField(ev.Payload, "text"))
		}
	case reverie.ToolResult:
		if hasField(ev.Payload, "error") {
			ep.Errors++
			if msg := stringField(ev.Payload, "error"); msg != "" {
				ep.LastError = clip(msg)
			}
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

// parseOutcome treats a missing or unrecognised outcome as partial: the task
// ended, but we cannot claim it succeeded. A source that has no verdict says
// "unknown" explicitly.
func parseOutcome(s string) store.Outcome {
	switch o := store.Outcome(s); o {
	case store.Success, store.Failure, store.Partial, store.Unknown:
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

const (
	indexTextCap  = 4000
	indexEventCap = 500
)

// IndexText is what full-text and vector search see for an episode: its
// title, outcome, and every string found in its events' payloads, capped.
func IndexText(ep store.Episode, events []reverie.Event) string {
	var b strings.Builder
	b.WriteString(ep.Title)
	b.WriteString(" ")
	b.WriteString(string(ep.Outcome))
	for _, ev := range events {
		var v any
		if json.Unmarshal(ev.Payload, &v) != nil {
			continue
		}
		var part strings.Builder
		collectStrings(v, &part, 0)
		s := part.String()
		if len(s) > indexEventCap {
			s = s[:indexEventCap]
		}
		b.WriteString("\n")
		b.WriteString(s)
		if b.Len() >= indexTextCap {
			break
		}
	}
	out := b.String()
	if len(out) > indexTextCap {
		out = out[:indexTextCap]
	}
	return strings.ToValidUTF8(out, "")
}

func collectStrings(v any, b *strings.Builder, depth int) {
	if depth > 4 {
		return
	}
	switch t := v.(type) {
	case string:
		b.WriteString(t)
		b.WriteString(" ")
	case []any:
		for _, e := range t {
			collectStrings(e, b, depth+1)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectStrings(t[k], b, depth+1)
		}
	}
}

// Index writes the episode's search text to the index. Call it when an
// episode closes.
func Index(ctx context.Context, tx store.Tx, ep store.Episode) error {
	events, err := tx.Events().List(ctx, store.EventQuery{TaskID: ep.TaskID, Limit: store.MaxLimit})
	if err != nil {
		return err
	}
	return tx.Index().Upsert(ctx, store.Item{Kind: "episode", RefID: ep.TaskID, Text: IndexText(ep, events)})
}

const gistCap = 160

// clip keeps a one-line, length-capped, control-free excerpt for display.
func clip(s string) string { return guard.OneLine(s, gistCap) }
