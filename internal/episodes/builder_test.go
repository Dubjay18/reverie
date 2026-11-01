package episodes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/sqlite"
)

var sc = reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u"}

func open(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(t.TempDir()+"/r.db", sqlite.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func apply(t *testing.T, st store.Store, evs ...reverie.Event) (ep store.Episode, err error) {
	t.Helper()
	err = st.For(sc).Tx(context.Background(), func(tx store.Tx) error {
		for _, ev := range evs {
			ep, err = Apply(context.Background(), tx, ev)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return
}

func ev(task string, kind reverie.EventKind, payload string) reverie.Event {
	return reverie.NewEvent(sc, task, kind, reverie.OriginAgent, []byte(payload))
}

func TestLifecycle(t *testing.T) {
	st := open(t)
	ep, err := apply(t, st,
		ev("k", reverie.TaskStarted, `{"description":"fix login"}`),
		ev("k", reverie.ToolCalled, `{}`),
		ev("k", reverie.ToolResult, `{"error":"boom"}`),
		ev("k", reverie.Feedback, `{}`),
		ev("k", reverie.TaskEnded, `{"outcome":"failure"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Closed || ep.Outcome != store.Failure || ep.Title != "fix login" || ep.Events != 5 ||
		ep.ToolCalls != 1 || ep.Errors != 1 || ep.Feedback != 1 || ep.Implicit {
		t.Fatalf("unexpected episode: %+v", ep)
	}
	if ep.Importance != 1 {
		t.Fatalf("failure + error + feedback should cap at 1, got %v", ep.Importance)
	}
}

func TestClosedStaysClosedAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/r.db"
	st, _ := sqlite.Open(path, sqlite.Config{})
	if _, err := apply(t, st, ev("k", reverie.TaskStarted, `{}`), ev("k", reverie.TaskEnded, `{"outcome":"success"}`)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err := sqlite.Open(path, sqlite.Config{}) // a restart
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := apply(t, st, ev("k", reverie.Message, `{}`)); !errors.Is(err, ErrTaskClosed) {
		t.Fatalf("got %v, want ErrTaskClosed", err)
	}
}

func TestOutOfOrderAndImplicit(t *testing.T) {
	st := open(t)
	first := ev("k", reverie.Message, `{}`)
	second := ev("k", reverie.Message, `{}`)
	// second arrives before first; start event never arrives.
	ep, err := apply(t, st, second, first)
	if err != nil {
		t.Fatal(err)
	}
	if ep.FirstEvent != first.ID || ep.LastEvent != second.ID || !ep.Implicit || ep.Closed {
		t.Fatalf("bounds wrong: %+v", ep)
	}
}

func TestEndedWithoutOutcomeIsPartial(t *testing.T) {
	ep, err := apply(t, open(t), ev("k", reverie.TaskEnded, `{}`))
	if err != nil || ep.Outcome != store.Partial {
		t.Fatalf("got %+v err %v", ep, err)
	}
}

func TestSweepAbandonsIdleOnly(t *testing.T) {
	st := open(t)
	old := ev("old", reverie.Message, `{}`)
	old.At = time.Now().Add(-time.Hour)
	fresh := ev("fresh", reverie.Message, `{}`)
	if _, err := apply(t, st, old, fresh); err != nil {
		t.Fatal(err)
	}
	var swept []store.Episode
	err := st.For(sc).Tx(context.Background(), func(tx store.Tx) (err error) {
		swept, err = Sweep(context.Background(), tx, time.Now(), 10*time.Minute)
		return
	})
	if err != nil || len(swept) != 1 || swept[0].TaskID != "old" || swept[0].Outcome != store.Abandoned {
		t.Fatalf("swept %+v err %v", swept, err)
	}
	if _, err := apply(t, st, ev("old", reverie.Message, `{}`)); !errors.Is(err, ErrTaskClosed) {
		t.Fatalf("abandoned task must stay closed, got %v", err)
	}
}
