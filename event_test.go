package reverie

import (
	"errors"
	"testing"
	"time"
)

func TestEventValidate(t *testing.T) {
	s := Scope{"t", "a", "p", "u"}
	ok := NewEvent(s, "task", Message, OriginUser, []byte(`{"text":"hi"}`))

	cases := []struct {
		name string
		mut  func(*Event)
		want error
	}{
		{"valid", func(e *Event) {}, nil},
		{"no payload is fine", func(e *Event) { e.Payload = nil }, nil},
		{"missing task id", func(e *Event) { e.TaskID = "" }, ErrNoTaskID},
		{"bad kind", func(e *Event) { e.Kind = "nope" }, ErrBadKind},
		{"missing origin", func(e *Event) { e.Origin = "" }, ErrBadOrigin},
		{"unknown origin", func(e *Event) { e.Origin = "robot" }, ErrBadOrigin},
		{"zero id", func(e *Event) { e.ID = [16]byte{} }, ErrNoID},
		{"zero time", func(e *Event) { e.At = time.Time{} }, ErrNoTime},
		{"bad json", func(e *Event) { e.Payload = []byte(`{`) }, ErrBadPayload},
		{"negative tokens", func(e *Event) { e.TokenCost = -1 }, ErrNegativeTok},
		{"empty scope field", func(e *Event) { e.Scope.User = "" }, ErrBadScope},
	}
	for _, c := range cases {
		e := ok
		c.mut(&e)
		if err := e.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
