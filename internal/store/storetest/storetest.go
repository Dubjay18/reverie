// Package storetest is the conformance suite every store.Store must pass.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

var base = reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u"}

func event(s reverie.Scope, task string, kind reverie.EventKind) reverie.Event {
	return reverie.NewEvent(s, task, kind, reverie.OriginUser, []byte(`{"text":"x"}`))
}

func Run(t *testing.T, open func(t *testing.T) store.Store) {
	t.Run("append is idempotent", func(t *testing.T) { appendIdempotent(t, open(t)) })
	t.Run("list is ordered and filterable", func(t *testing.T) { listOrdered(t, open(t)) })
	t.Run("append rejects foreign scope", func(t *testing.T) { foreignScope(t, open(t)) })
	t.Run("scopes are isolated on every dimension", func(t *testing.T) { isolation(t, open(t)) })
	t.Run("same task id in two scopes does not collide", func(t *testing.T) { sameTaskTwoScopes(t, open(t)) })
	t.Run("tx rolls back on error", func(t *testing.T) { rollback(t, open(t)) })
	t.Run("incomplete scope is rejected", func(t *testing.T) { badScope(t, open(t)) })
	t.Run("episode roundtrip", func(t *testing.T) { episodeRoundtrip(t, open(t)) })
	t.Run("recent returns closed newest first", func(t *testing.T) { recent(t, open(t)) })
	t.Run("same event id in two scopes persists in both", func(t *testing.T) { sameULIDTwoScopes(t, open(t)) })
	t.Run("view reads committed data and cannot write", func(t *testing.T) { viewReadOnly(t, open(t)) })
	t.Run("nested tx fails instead of hanging", func(t *testing.T) { nestedTx(t, open(t)) })
	t.Run("scopes lists scopes with data", func(t *testing.T) { scopes(t, open(t)) })
}

func tx(t *testing.T, s store.Store, sc reverie.Scope, fn func(store.Tx) error) error {
	t.Helper()
	return s.For(sc).Tx(context.Background(), fn)
}

func mustTx(t *testing.T, s store.Store, sc reverie.Scope, fn func(store.Tx) error) {
	t.Helper()
	if err := tx(t, s, sc, fn); err != nil {
		t.Fatal(err)
	}
}

func appendIdempotent(t *testing.T, s store.Store) {
	ev := event(base, "k1", reverie.Message)
	mustTx(t, s, base, func(x store.Tx) error {
		for i, want := range []bool{true, false} {
			got, err := x.Events().Append(context.Background(), ev)
			if err != nil || got != want {
				t.Fatalf("append #%d: inserted=%v err=%v, want inserted=%v", i, got, err, want)
			}
		}
		return nil
	})
}

func listOrdered(t *testing.T, s store.Store) {
	ctx := context.Background()
	var ids []ulid.ULID
	mustTx(t, s, base, func(x store.Tx) error {
		for _, task := range []string{"a", "b", "a"} {
			ev := event(base, task, reverie.Message)
			ids = append(ids, ev.ID)
			if _, err := x.Events().Append(ctx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	mustTx(t, s, base, func(x store.Tx) error {
		all, err := x.Events().List(ctx, store.EventQuery{})
		if err != nil || len(all) != 3 {
			t.Fatalf("list all: %d events, err %v", len(all), err)
		}
		for i := range all {
			if all[i].ID != ids[i] {
				t.Fatalf("event %d out of order", i)
			}
		}
		onlyA, _ := x.Events().List(ctx, store.EventQuery{TaskID: "a"})
		if len(onlyA) != 2 {
			t.Fatalf("task filter: got %d, want 2", len(onlyA))
		}
		after, _ := x.Events().List(ctx, store.EventQuery{After: ids[0]})
		if len(after) != 2 || after[0].ID != ids[1] {
			t.Fatalf("After is not exclusive: got %d", len(after))
		}
		lim, _ := x.Events().List(ctx, store.EventQuery{Limit: 1})
		if len(lim) != 1 {
			t.Fatalf("limit: got %d", len(lim))
		}
		got := all[0]
		if got.Scope != base || got.Origin != reverie.OriginUser || string(got.Payload) != `{"text":"x"}` {
			t.Fatalf("roundtrip lost fields: %+v", got)
		}
		return nil
	})
}

func foreignScope(t *testing.T, s store.Store) {
	other := base
	other.User = "someone-else"
	err := tx(t, s, base, func(x store.Tx) error {
		_, err := x.Events().Append(context.Background(), event(other, "k", reverie.Message))
		return err
	})
	if !errors.Is(err, store.ErrScopeMismatch) {
		t.Fatalf("got %v, want ErrScopeMismatch", err)
	}
}

func variants() map[string]reverie.Scope {
	v := map[string]reverie.Scope{}
	s := base
	s.Tenant = "t2"
	v["tenant"] = s
	s = base
	s.Agent = "a2"
	v["agent"] = s
	s = base
	s.Project = "p2"
	v["project"] = s
	s = base
	s.User = "u2"
	v["user"] = s
	return v
}

func isolation(t *testing.T, s store.Store) {
	ctx := context.Background()
	mustTx(t, s, base, func(x store.Tx) error {
		if _, err := x.Events().Append(ctx, event(base, "secret", reverie.Message)); err != nil {
			return err
		}
		return x.Episodes().Save(ctx, store.Episode{TaskID: "secret", FirstEvent: reverie.NewID(), LastEvent: reverie.NewID(),
			StartedAt: time.Now(), EndedAt: time.Now(), Closed: true, Outcome: store.Success, Tier: store.Hot})
	})
	for dim, sc := range variants() {
		mustTx(t, s, sc, func(x store.Tx) error {
			evs, err := x.Events().List(ctx, store.EventQuery{})
			if err != nil || len(evs) != 0 {
				t.Fatalf("%s: leaked %d events (err %v)", dim, len(evs), err)
			}
			if _, err := x.Episodes().Get(ctx, "secret"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s: episode leaked, err %v", dim, err)
			}
			rec, _ := x.Episodes().Recent(ctx, 10)
			if len(rec) != 0 {
				t.Fatalf("%s: Recent leaked %d episodes", dim, len(rec))
			}
			return nil
		})
	}
}

func sameTaskTwoScopes(t *testing.T, s store.Store) {
	ctx := context.Background()
	other := variants()["user"]
	for _, sc := range []reverie.Scope{base, other} {
		mustTx(t, s, sc, func(x store.Tx) error {
			return x.Episodes().Save(ctx, store.Episode{TaskID: "same", Title: sc.User, FirstEvent: reverie.NewID(), LastEvent: reverie.NewID(),
				StartedAt: time.Now(), EndedAt: time.Now(), Tier: store.Hot})
		})
	}
	for _, sc := range []reverie.Scope{base, other} {
		mustTx(t, s, sc, func(x store.Tx) error {
			ep, err := x.Episodes().Get(ctx, "same")
			if err != nil || ep.Title != sc.User {
				t.Fatalf("scope %s got title %q err %v", sc.User, ep.Title, err)
			}
			return nil
		})
	}
}

func rollback(t *testing.T, s store.Store) {
	ctx := context.Background()
	boom := errors.New("boom")
	err := tx(t, s, base, func(x store.Tx) error {
		if _, err := x.Events().Append(ctx, event(base, "r", reverie.Message)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	mustTx(t, s, base, func(x store.Tx) error {
		evs, _ := x.Events().List(ctx, store.EventQuery{})
		if len(evs) != 0 {
			t.Fatalf("rolled-back event persisted")
		}
		return nil
	})
}

func badScope(t *testing.T, s store.Store) {
	err := tx(t, s, reverie.Scope{Tenant: "t"}, func(store.Tx) error { return nil })
	if !errors.Is(err, reverie.ErrBadScope) {
		t.Fatalf("got %v, want ErrBadScope", err)
	}
}

func episodeRoundtrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Nanosecond)
	want := store.Episode{
		TaskID: "e1", Title: "fix bug", FirstEvent: reverie.NewID(), LastEvent: reverie.NewID(),
		StartedAt: now, EndedAt: now.Add(time.Second), Closed: true, Outcome: store.Failure,
		Events: 5, ToolCalls: 2, Errors: 1, Feedback: 1, Implicit: true, Importance: 0.9,
		Tier: store.Hot, Consolidated: false,
	}
	mustTx(t, s, base, func(x store.Tx) error {
		if _, err := x.Episodes().Get(ctx, "e1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		if err := x.Episodes().Save(ctx, want); err != nil {
			return err
		}
		want.Events = 6 // upsert
		return x.Episodes().Save(ctx, want)
	})
	mustTx(t, s, base, func(x store.Tx) error {
		got, err := x.Episodes().Get(ctx, "e1")
		if err != nil {
			return err
		}
		if !got.StartedAt.Equal(want.StartedAt) || !got.EndedAt.Equal(want.EndedAt) {
			t.Fatalf("times: got %v/%v want %v/%v", got.StartedAt, got.EndedAt, want.StartedAt, want.EndedAt)
		}
		got.StartedAt, got.EndedAt = want.StartedAt, want.EndedAt
		if got != want {
			t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, want)
		}
		return nil
	})
}

func recent(t *testing.T, s store.Store) {
	ctx := context.Background()
	t0 := time.Now().UTC()
	mustTx(t, s, base, func(x store.Tx) error {
		for i, c := range []struct {
			task   string
			closed bool
		}{{"old", true}, {"open", false}, {"new", true}} {
			end := t0.Add(time.Duration(i) * time.Minute)
			if err := x.Episodes().Save(ctx, store.Episode{TaskID: c.task, FirstEvent: reverie.NewID(), LastEvent: reverie.NewID(),
				StartedAt: end, EndedAt: end, Closed: c.closed, Tier: store.Hot}); err != nil {
				return err
			}
		}
		rec, err := x.Episodes().Recent(ctx, 10)
		if err != nil || len(rec) != 2 || rec[0].TaskID != "new" || rec[1].TaskID != "old" {
			t.Fatalf("recent: %+v err %v", rec, err)
		}
		lim, _ := x.Episodes().Recent(ctx, 1)
		if len(lim) != 1 {
			t.Fatalf("limit ignored")
		}
		open, _ := x.Episodes().Open(ctx)
		if len(open) != 1 || open[0].TaskID != "open" {
			t.Fatalf("open: %+v", open)
		}
		return nil
	})
}

func scopes(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, sc := range []reverie.Scope{base, variants()["user"]} {
		mustTx(t, s, sc, func(x store.Tx) error {
			_, err := x.Events().Append(ctx, event(sc, "k", reverie.Message))
			return err
		})
	}
	got, err := s.Scopes(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("scopes: %v err %v", got, err)
	}
}

func sameULIDTwoScopes(t *testing.T, s store.Store) {
	ctx := context.Background()
	other := variants()["user"]
	a := event(base, "k", reverie.Message)
	b := event(other, "k", reverie.Message)
	b.ID = a.ID // same ULID, different scope, different event
	for _, c := range []struct {
		sc reverie.Scope
		ev reverie.Event
	}{{base, a}, {other, b}} {
		mustTx(t, s, c.sc, func(x store.Tx) error {
			inserted, err := x.Events().Append(ctx, c.ev)
			if err != nil || !inserted {
				t.Fatalf("%s: inserted=%v err=%v", c.sc.User, inserted, err)
			}
			return nil
		})
	}
	for _, sc := range []reverie.Scope{base, other} {
		mustTx(t, s, sc, func(x store.Tx) error {
			evs, _ := x.Events().List(ctx, store.EventQuery{})
			if len(evs) != 1 {
				t.Fatalf("%s sees %d events, want 1", sc.User, len(evs))
			}
			return nil
		})
	}
}

func viewReadOnly(t *testing.T, s store.Store) {
	ctx := context.Background()
	mustTx(t, s, base, func(x store.Tx) error {
		_, err := x.Events().Append(ctx, event(base, "k", reverie.Message))
		return err
	})
	err := s.For(base).View(ctx, func(x store.Tx) error {
		evs, err := x.Events().List(ctx, store.EventQuery{})
		if err != nil || len(evs) != 1 {
			t.Fatalf("view saw %d events, err %v", len(evs), err)
		}
		if _, err := x.Events().Append(ctx, event(base, "k2", reverie.Message)); err == nil {
			t.Fatal("write through View must fail")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func nestedTx(t *testing.T, s store.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := s.For(base).Tx(ctx, func(store.Tx) error {
		return s.For(base).Tx(ctx, func(store.Tx) error { return nil })
	})
	if err == nil {
		t.Fatal("nested Tx must fail once the context ends")
	}
}
