package reverie

import (
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Ref points at raw events: one task, optionally narrowed to an inclusive ULID
// range. It is the handle a ContextPack item carries so an agent can ask for
// the evidence behind a summary.
type Ref struct {
	TaskID   string
	From, To ulid.ULID // zero means unbounded on that side
	FactID   string    // "fact:<id>": the events behind a fact; TaskID and the range are unused
}

var ErrBadRef = errors.New("reverie: malformed ref, want ep:<task>, ep:<task>#<from>..<to> or fact:<id>")

// String renders "ep:<task>" or "ep:<task>#<from>..<to>".
func (r Ref) String() string {
	if r.FactID != "" {
		return "fact:" + r.FactID
	}
	if r.From == (ulid.ULID{}) && r.To == (ulid.ULID{}) {
		return "ep:" + r.TaskID
	}
	return fmt.Sprintf("ep:%s#%s..%s", r.TaskID, bound(r.From), bound(r.To))
}

func bound(u ulid.ULID) string {
	if u == (ulid.ULID{}) {
		return ""
	}
	return u.String()
}

// ParseRef reads the String form. Anything after the first whitespace is
// ignored, so a whole citation line can be pasted in.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if f := strings.Fields(s); len(f) > 0 {
		s = f[0]
	}
	if id, ok := strings.CutPrefix(s, "fact:"); ok {
		if id == "" {
			return Ref{}, ErrBadRef
		}
		return Ref{FactID: id}, nil
	}
	rest, ok := strings.CutPrefix(s, "ep:")
	if !ok || rest == "" {
		return Ref{}, ErrBadRef
	}
	task, rng, hasRange := cutLast(rest, "#")
	if !hasRange {
		return Ref{TaskID: rest}, nil
	}
	from, to, ok := strings.Cut(rng, "..")
	if !ok {
		return Ref{TaskID: rest}, nil // '#' was part of the task id
	}
	// A range was written, so it must be valid: silently widening a bad range
	// to "the whole task" would hand back more than the caller asked for.
	r := Ref{TaskID: task}
	var err error
	if from != "" {
		if r.From, err = ulid.ParseStrict(from); err != nil {
			return Ref{}, ErrBadRef
		}
	}
	if to != "" {
		if r.To, err = ulid.ParseStrict(to); err != nil {
			return Ref{}, ErrBadRef
		}
	}
	if r.From != (ulid.ULID{}) && r.To != (ulid.ULID{}) && r.From.Compare(r.To) > 0 {
		return Ref{}, ErrBadRef
	}
	return r, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
