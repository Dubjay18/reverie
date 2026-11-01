package episodic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Dubjay18/reverie"
	"sync"
	"time"
)

func field(payload []byte, key string) string {
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	if v, ok := m[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func hasField(payload []byte, key string) bool {
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

var (
	ErrNoTaskID   = errors.New("episodic: event has no task id")
	ErrTaskClosed = errors.New("episodic: task is already closed")
)

type Outcome string

const (
	Success   Outcome = "success"
	Failure   Outcome = "failure"
	Partial   Outcome = "partial"
	Abandoned Outcome = "abandoned"
)

func parseOutcome(s string) Outcome {
	switch s {
	case "success":
		return Success
	case "failure":
		return Failure
	case "partial":
		return Partial
	case "abandoned":
		return Abandoned
	default:
		return ""
	}
}

type Store interface {
	SaveEpisode(ctx context.Context, ep *Episode) error
}

type Episode struct {
	Scope                               reverie.Scope
	TaskID                              string
	Title                               string
	FirstEvent                          string
	LastEvent                           string
	StartedAt, EndedAt                  time.Time
	Outcome                             Outcome
	Events, ToolCalls, Errors, Feedback int
	Implicit                            bool // opened by a non-start event
	Importance                          float64
}

type key struct {
	scope reverie.Scope
	task  string
}

type Builder struct {
	mu     sync.Mutex // Record may be called concurrently
	idle   time.Duration
	open   map[key]*Episode
	closed map[key]struct{}
}

func (b *Builder) Apply(ev reverie.Event) (*Episode, error) {
	if ev.TaskID == "" {
		return nil, ErrNoTaskID
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	k := key{scope: ev.Scope, task: ev.TaskID}
	if _, done := b.closed[k]; done {
		return nil, ErrTaskClosed
	}
	ep, ok := b.open[k]
	if !ok {
		ep = &Episode{Scope: ev.Scope, TaskID: ev.TaskID, StartedAt: ev.At, FirstEvent: ev.ID.String(), Implicit: ev.Kind != reverie.TaskStarted}
		b.open[k] = ep
	}
	ep.LastEvent, ep.EndedAt = ev.ID.String(), ev.At
	ep.Events++

	switch ev.Kind {
	case reverie.TaskStarted:
		if ep.Title == "" {
			ep.Title = field(ev.Payload, "description")
		}
	case reverie.ToolCalled:
		ep.ToolCalls++
	case reverie.ToolResult:
		if hasField(ev.Payload, "error") {
			ep.Errors++
		}
	case reverie.Feedback:
		ep.Feedback++
	case reverie.TaskEnded:
		ep.Outcome = parseOutcome(field(ev.Payload, "outcome"))
		return b.finish(k, ep), nil // returns the episode only wehn it closes
	}
	return ep, nil
}

func (b *Builder) finish(k key, ep *Episode) *Episode {
	delete(b.open, k)
	b.closed[k] = struct{}{}
	return ep
}

func (b *Builder) Sweep(now time.Time) []*Episode {
	b.mu.Lock()
	defer b.mu.Unlock()

	var done []*Episode
	for k, ep := range b.open {
		if now.Sub(ep.EndedAt) > b.idle {
			ep.Outcome = Abandoned
			done = append(done, b.finish(k, ep))
		}
	}
	return done
}
