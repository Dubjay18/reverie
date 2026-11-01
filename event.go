package reverie

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Scope identifies whose memory an event belongs to. Every field is required
// for writes; wildcards exist only on explicit read paths.
type Scope struct {
	Tenant  string `json:"tenant"`
	Agent   string `json:"agent"`
	Project string `json:"project"`
	User    string `json:"user"`
}

var ErrBadScope = errors.New("reverie: scope fields tenant, agent, project and user are all required")

func (s Scope) Validate() error {
	if s.Tenant == "" || s.Agent == "" || s.Project == "" || s.User == "" {
		return ErrBadScope
	}
	return nil
}

// Size limits keep one hostile or buggy client from bloating the log.
const (
	MaxTaskID  = 256
	MaxPayload = 64 << 10
)

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

// Origin says who produced an event's content. Dreams use it to decide how
// much to trust a fact: tool output alone never earns trust, and recalled
// memory is never evidence (it would let memory reinforce itself).
type Origin string

const (
	OriginUser     Origin = "user_input"
	OriginAgent    Origin = "agent_decision"
	OriginTool     Origin = "tool_output"
	OriginSystem   Origin = "system"
	OriginRecalled Origin = "recalled"
)

type Event struct {
	ID        ulid.ULID       `json:"id"`
	Scope     Scope           `json:"scope"`
	TaskID    string          `json:"task_id"`
	Kind      EventKind       `json:"kind"`
	Origin    Origin          `json:"origin"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	TokenCost int             `json:"token_cost,omitempty"`
}

var (
	ErrNoTaskID    = errors.New("reverie: event has no task id")
	ErrBadKind     = errors.New("reverie: unknown event kind")
	ErrBadOrigin   = errors.New("reverie: unknown or missing event origin")
	ErrNoID        = errors.New("reverie: event has no id")
	ErrNoTime      = errors.New("reverie: event has no timestamp")
	ErrBadPayload  = errors.New("reverie: payload is not valid json")
	ErrNegativeTok = errors.New("reverie: negative token cost")
	ErrTaskIDLong  = errors.New("reverie: task id is longer than 256 bytes")
	ErrPayloadBig  = errors.New("reverie: payload is larger than 64 KB")
)

var validKinds = map[EventKind]struct{}{
	TaskStarted: {}, Message: {}, ToolCalled: {}, ToolResult: {},
	Decision: {}, Feedback: {}, TaskEnded: {},
}

var validOrigins = map[Origin]struct{}{
	OriginUser: {}, OriginAgent: {}, OriginTool: {}, OriginSystem: {}, OriginRecalled: {},
}

// Validate is the contract: an event that fails it is rejected before any write.
func (e Event) Validate() error {
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	switch {
	case e.ID == (ulid.ULID{}):
		return ErrNoID
	case e.TaskID == "":
		return ErrNoTaskID
	case e.At.IsZero():
		return ErrNoTime
	case e.TokenCost < 0:
		return ErrNegativeTok
	case len(e.TaskID) > MaxTaskID:
		return ErrTaskIDLong
	case len(e.Payload) > MaxPayload:
		return ErrPayloadBig
	}
	if _, ok := validKinds[e.Kind]; !ok {
		return fmt.Errorf("%w: %q", ErrBadKind, e.Kind)
	}
	if _, ok := validOrigins[e.Origin]; !ok {
		return fmt.Errorf("%w: %q", ErrBadOrigin, e.Origin)
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return ErrBadPayload
	}
	return nil
}

// NewID returns a new ULID. IDs are monotonic and safe for concurrent use.
func NewID() ulid.ULID { return ulid.Make() }

// NewEvent builds an event stamped with a fresh ID and the current time.
func NewEvent(s Scope, task string, kind EventKind, origin Origin, payload json.RawMessage) Event {
	return Event{ID: NewID(), Scope: s, TaskID: task, Kind: kind, Origin: origin, At: time.Now().UTC(), Payload: payload}
}
