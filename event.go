package reverie

import (
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"
)

// Scope identifies whose memory an event belongs to.
type Scope struct {
	Tenant  string `json:"tenant"`
	Agent   string `json:"agent"`
	Project string `json:"project"`
	User    string `json:"user"`
}

type EventKind string

const (
	TaskStarted EventKind = "task.started"
	Message     EventKind = "message"
	ToolCalled  EventKind = "tool.called"
	ToolResult  EventKind = "tool.result"
	Decision    EventKind = "decision"
	Feedback    EventKind = "feedback"
	TaskEnded   EventKind = "task.ended"
)

type Event struct {
	ID        ulid.ULID       `json:"id"`
	Scope     Scope           `json:"scope"`
	TaskID    string          `json:"task_id"`
	Kind      EventKind       `json:"kind"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	TokenCost int             `json:"token_cost,omitempty"`
}

// NewID returns a new ULID. IDs are monotonic and safe for concurrent use.
func NewID() ulid.ULID { return ulid.Make() }

// NewEvent builds an event stamped with a fresh ID and the current time.
func NewEvent(s Scope, task string, kind EventKind, payload json.RawMessage) Event {
	return Event{ID: NewID(), Scope: s, TaskID: task, Kind: kind, At: time.Now().UTC(), Payload: payload}
}
