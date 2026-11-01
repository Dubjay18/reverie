package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/embed"
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

func TestRecordRedactsSecrets(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir() + "/r.db")
	defer e.Close()
	if err := e.Record(ctx, ev(sc, "k", reverie.Message, `{"text":"password=correcthorsebattery"}`)); err != nil {
		t.Fatal(err)
	}
	var got []reverie.Event
	e.st.For(sc).View(ctx, func(tx store.Tx) (err error) {
		got, err = tx.Events().List(ctx, store.EventQuery{})
		return
	})
	if len(got) != 1 || strings.Contains(string(got[0].Payload), "correcthorse") || !strings.Contains(string(got[0].Payload), "REDACTED") {
		t.Fatalf("payload stored as %s", got[0].Payload)
	}
}

// pickyEmbedder fails on any text containing "POISON", in batches and alone.
type pickyEmbedder struct{ down bool }

func (pickyEmbedder) Dimensions() int { return 4 }
func (p pickyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if p.down {
			return nil, errors.New("connection refused")
		}
		if strings.Contains(t, "POISON") {
			return nil, embed.ErrRejected
		}
		out[i] = []float32{1, float32(len(t)), 0, 0}
	}
	return out, nil
}

func pendingCount(t *testing.T, e *Engine) (n int) {
	t.Helper()
	e.st.For(sc).View(context.Background(), func(tx store.Tx) error {
		p, _ := tx.Index().Pending(context.Background(), 100)
		n = len(p)
		return nil
	})
	return
}

func TestOnePoisonedTextDoesNotBlockTheEmbeddingQueue(t *testing.T) {
	ctx := context.Background()
	e, err := Open(t.TempDir()+"/r.db", WithEmbedder(pickyEmbedder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.stop() // drive the queue by hand
	<-e.done
	for _, task := range []string{"a", "b", "c"} {
		text := "fine " + task
		if task == "b" {
			text = "POISON"
		}
		if err := e.Record(ctx, ev(sc, task, reverie.Message, `{"text":"`+text+`"}`), ev(sc, task, reverie.TaskEnded, `{"outcome":"success"}`)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < store.MaxEmbedFailures+1; i++ {
		if _, err := e.EmbedPending(ctx, 10); err != nil {
			t.Fatalf("a partial failure is not an outage: %v", err)
		}
	}
	// a and c are embedded; b is parked, so nothing is left pending
	if n := pendingCount(t, e); n != 0 {
		t.Fatalf("%d items still pending: the poisoned one is blocking or looping", n)
	}
	var hits []store.Hit
	e.st.For(sc).View(ctx, func(tx store.Tx) (err error) {
		hits, err = tx.Index().SearchVector(ctx, []float32{1, 10, 0, 0}, 10)
		return
	})
	if len(hits) != 2 {
		t.Fatalf("want a and c embedded, got %v", hits)
	}
}

func TestEmbedderOutageIsReportedAndNothingIsParked(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir()+"/r.db", WithEmbedder(pickyEmbedder{down: true}))
	defer e.Close()
	e.stop()
	<-e.done
	e.Record(ctx, ev(sc, "a", reverie.TaskEnded, `{"outcome":"success"}`))
	for i := 0; i < store.MaxEmbedFailures+2; i++ {
		if _, err := e.EmbedPending(ctx, 10); err == nil {
			t.Fatal("an outage must be returned as an error")
		}
	}
	if n := pendingCount(t, e); n != 1 {
		t.Fatalf("an outage must not park items, pending=%d", n)
	}
}

type blockingEmbedder struct{}

func (blockingEmbedder) Dimensions() int { return 4 }
func (blockingEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHungEmbedderDoesNotStallRecall(t *testing.T) {
	ctx := context.Background()
	e, _ := Open(t.TempDir()+"/r.db", WithEmbedder(blockingEmbedder{}))
	defer e.Close()
	e.embedTimeout = 100 * time.Millisecond
	if err := e.Record(ctx, ev(sc, "k", reverie.Message, `{"text":"ERR_9 happened"}`), ev(sc, "k", reverie.TaskEnded, `{"outcome":"failure"}`)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	pack, err := e.Recall(ctx, sc, "ERR_9", 1000)
	if err != nil || len(pack.Episodes) != 1 {
		t.Fatalf("recall must degrade to full-text: %v %+v", err, pack)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("recall took %v with a hung embedder", time.Since(start))
	}
}
