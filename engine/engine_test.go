package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

var sc = reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u"}

func ev(s reverie.Scope, task string, kind reverie.EventKind, payload string) reverie.Event {
	return reverie.NewEvent(s, task, kind, reverie.OriginAgent, []byte(payload))
}

func TestAmnesia(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/r.db"

	// session 1: a task fails
	e1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.Record(ctx,
		ev(sc, "deploy", reverie.TaskStarted, `{"description":"deploy the api"}`),
		ev(sc, "deploy", reverie.ToolResult, `{"error":"timeout"}`),
		ev(sc, "deploy", reverie.TaskEnded, `{"outcome":"failure"}`),
		ev(sc, "lint", reverie.TaskStarted, `{"description":"run lint"}`),
		ev(sc, "lint", reverie.TaskEnded, `{"outcome":"success"}`),
	); err != nil {
		t.Fatal(err)
	}
	e1.Close()

	// session 2: new process, same file
	e2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	pack, err := e2.Recall(ctx, sc, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Episodes) != 2 || !strings.Contains(pack.Episodes[0].Text, "deploy the api: failure") {
		t.Fatalf("failure must surface first, got %+v", pack.Episodes)
	}
}

func TestRecallIsScoped(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()
	other := sc
	other.User = "intruder"
	if err := e.Record(ctx, ev(sc, "k", reverie.TaskEnded, `{"outcome":"failure"}`)); err != nil {
		t.Fatal(err)
	}
	pack, err := e.Recall(ctx, other, "", 1000)
	if err != nil || len(pack.Episodes) != 0 {
		t.Fatalf("leaked %+v err %v", pack, err)
	}
}

func TestRecallRespectsBudget(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()
	for _, task := range []string{"a", "b", "c"} {
		if err := e.Record(ctx, ev(sc, task, reverie.TaskEnded, `{"outcome":"success"}`)); err != nil {
			t.Fatal(err)
		}
	}
	pack, _ := e.Recall(ctx, sc, "", 8)
	if pack.Tokens > pack.Budget || pack.Dropped == 0 {
		t.Fatalf("tokens=%d budget=%d dropped=%d", pack.Tokens, pack.Budget, pack.Dropped)
	}
}

func TestRecordValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()
	bad := ev(sc, "", reverie.Message, `{}`)
	err := e.Record(ctx, ev(sc, "ok", reverie.Message, `{}`), bad)
	if !errors.Is(err, reverie.ErrNoTaskID) {
		t.Fatalf("got %v", err)
	}
	if n := count(t, e, sc); n != 0 {
		t.Fatalf("a rejected batch wrote %d events", n)
	}
}

func TestRecordIsIdempotent(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()

	msg := ev(sc, "k", reverie.Message, `{}`)
	for i := 0; i < 2; i++ { // a retried write must not double-count
		if err := e.Record(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	if ep := episode(t, e, "k"); ep.Events != 1 {
		t.Fatalf("retry double-counted: events=%d", ep.Events)
	}
}

func TestLateEventIsKeptButDoesNotChangeClosedEpisode(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()

	if err := e.Record(ctx, ev(sc, "k", reverie.TaskEnded, `{"outcome":"success"}`)); err != nil {
		t.Fatal(err)
	}
	before := episode(t, e, "k")

	// a tool.result arriving after task.ended, in the same call as another task's event
	if err := e.Record(ctx, ev(sc, "other", reverie.Message, `{}`), ev(sc, "k", reverie.ToolResult, `{"error":"late"}`)); err != nil {
		t.Fatalf("late event must not fail the batch: %v", err)
	}
	if got := episode(t, e, "k"); got != before {
		t.Fatalf("closed episode changed:\n before %+v\n after  %+v", before, got)
	}
	if n := count(t, e, sc); n != 3 {
		t.Fatalf("want 3 raw events kept, got %d", n)
	}
	if episode(t, e, "other").Events != 1 {
		t.Fatal("other task in the same batch was lost")
	}
}

func TestFutureTimestampRejected(t *testing.T) {
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()
	far := ev(sc, "k", reverie.Message, `{}`)
	far.At = time.Now().Add(time.Hour)
	if err := e.Record(context.Background(), far); !errors.Is(err, ErrFutureEvent) {
		t.Fatalf("got %v", err)
	}
	near := ev(sc, "k", reverie.Message, `{}`)
	near.At = time.Now().Add(time.Minute)
	if err := e.Record(context.Background(), near); err != nil {
		t.Fatalf("small clock skew must be accepted: %v", err)
	}
}

func TestSweepAbandonsIdleAcrossScopes(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()
	other := sc
	other.User = "u2"
	old := ev(sc, "old", reverie.Message, `{}`)
	old.At = time.Now().Add(-time.Hour)
	if err := e.Record(ctx, old, ev(other, "fresh", reverie.Message, `{}`)); err != nil {
		t.Fatal(err)
	}
	n, err := e.Sweep(ctx, time.Now(), 10*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("swept %d err %v", n, err)
	}
	if ep := episode(t, e, "old"); !ep.Closed || ep.Outcome != store.Abandoned {
		t.Fatalf("old not abandoned: %+v", ep)
	}
}

func episode(t *testing.T, e *Engine, task string) (ep store.Episode) {
	t.Helper()
	err := e.st.For(sc).View(context.Background(), func(tx store.Tx) (err error) {
		ep, err = tx.Episodes().Get(context.Background(), task)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func count(t *testing.T, e *Engine, s reverie.Scope) (n int) {
	t.Helper()
	err := e.st.For(s).Tx(context.Background(), func(tx store.Tx) error {
		evs, err := tx.Events().List(context.Background(), store.EventQuery{})
		n = len(evs)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}
