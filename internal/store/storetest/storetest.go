// Package storetest is the conformance suite every store.Store must pass.
package storetest

import (
	"context"
	"encoding/json"
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
	t.Run("index text search is scoped and tolerant", func(t *testing.T) { indexText(t, open(t)) })
	t.Run("index vector search ranks by cosine and is scoped", func(t *testing.T) { indexVector(t, open(t)) })
	t.Run("index upsert clears the embedding", func(t *testing.T) { indexUpsertClears(t, open(t)) })
	t.Run("embed failures park an item until its text changes", func(t *testing.T) { embedFailuresParkItem(t, open(t)) })
	t.Run("event payload can be rewritten within scope", func(t *testing.T) { updatePayload(t, open(t)) })
	t.Run("facts roundtrip with provenance", func(t *testing.T) { factsRoundtrip(t, open(t)) })
	t.Run("facts reads return only what is believed", func(t *testing.T) { factsLiveOnly(t, open(t)) })
	t.Run("facts asof returns what was true then", func(t *testing.T) { factsAsOf(t, open(t)) })
	t.Run("facts related matches entity or value", func(t *testing.T) { factsRelated(t, open(t)) })
	t.Run("facts are scoped", func(t *testing.T) { factsScoped(t, open(t)) })
	t.Run("journal append get run revert", func(t *testing.T) { journalBasics(t, open(t)) })
	t.Run("index delete removes text and vector", func(t *testing.T) { indexDelete(t, open(t)) })
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
		Goal: "ship it", LastError: "boom", Events: 5, ToolCalls: 2, Errors: 1, Feedback: 1, Implicit: true, Importance: 0.9,
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

func put(t *testing.T, s store.Store, sc reverie.Scope, ref, text string, vec []float32) {
	t.Helper()
	mustTx(t, s, sc, func(x store.Tx) error {
		return x.Index().Upsert(context.Background(), store.Item{Kind: "episode", RefID: ref, Text: text, Embedding: vec})
	})
}

func refs(hits []store.Hit) (out []string) {
	for _, h := range hits {
		out = append(out, h.RefID)
	}
	return
}

func indexText(t *testing.T, s store.Store) {
	ctx := context.Background()
	put(t, s, base, "a", "deploy failed with ERR_42 timeout", nil)
	put(t, s, base, "b", "lint passed cleanly", nil)
	put(t, s, variants()["user"], "z", "ERR_42 in someone else's scope", nil)

	mustTx(t, s, base, func(x store.Tx) error {
		for _, q := range []string{"ERR_42", "err 42", "what is ERR_42?", `deploy "failed"`, "timeout -- OR (", "NEAR AND NOT"} {
			hits, err := x.Index().SearchText(ctx, q, 10)
			if err != nil {
				t.Fatalf("query %q must never be a syntax error: %v", q, err)
			}
			for _, h := range hits {
				if h.RefID == "z" {
					t.Fatalf("query %q leaked another scope's item", q)
				}
			}
		}
		hits, _ := x.Index().SearchText(ctx, "ERR_42", 10)
		if got := refs(hits); len(got) != 1 || got[0] != "a" {
			t.Fatalf("ERR_42 hits: %v", got)
		}
		if hits, err := x.Index().SearchText(ctx, "   ", 10); err != nil || len(hits) != 0 {
			t.Fatalf("blank query: %v %v", hits, err)
		}
		return nil
	})
}

func indexVector(t *testing.T, s store.Store) {
	ctx := context.Background()
	put(t, s, base, "near", "x", []float32{1, 0.1, 0})
	put(t, s, base, "far", "x", []float32{0, 1, 0})
	put(t, s, base, "opposite", "x", []float32{-1, 0, 0})
	put(t, s, base, "noembed", "x", nil)
	put(t, s, variants()["user"], "other-scope", "x", []float32{1, 0, 0})

	mustTx(t, s, base, func(x store.Tx) error {
		hits, err := x.Index().SearchVector(ctx, []float32{2, 0, 0}, 10)
		if err != nil {
			return err
		}
		got := refs(hits)
		want := []string{"near", "far", "opposite"}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("vector order: got %v want %v", got, want)
		}
		if lim, _ := x.Index().SearchVector(ctx, []float32{1, 0, 0}, 1); len(lim) != 1 {
			t.Fatalf("limit ignored")
		}
		pending, _ := x.Index().Pending(ctx, 10)
		if len(pending) != 1 || pending[0].RefID != "noembed" {
			t.Fatalf("pending: %+v", pending)
		}
		return nil
	})
}

func indexUpsertClears(t *testing.T, s store.Store) {
	ctx := context.Background()
	put(t, s, base, "a", "old text", []float32{1, 0})
	put(t, s, base, "a", "new text", nil) // text changed, so the old vector is stale
	mustTx(t, s, base, func(x store.Tx) error {
		if hits, _ := x.Index().SearchVector(ctx, []float32{1, 0}, 5); len(hits) != 0 {
			t.Fatalf("stale vector survived: %v", refs(hits))
		}
		if hits, _ := x.Index().SearchText(ctx, "old", 5); len(hits) != 0 {
			t.Fatalf("stale text survived")
		}
		if hits, _ := x.Index().SearchText(ctx, "new", 5); len(hits) != 1 {
			t.Fatalf("new text not found")
		}
		stale, err := x.Index().SetEmbedding(ctx, "episode", "a", "old text", []float32{0, 1})
		if err != nil || stale {
			t.Fatalf("a vector computed for old text must not apply to new text: applied=%v err=%v", stale, err)
		}
		applied, err := x.Index().SetEmbedding(ctx, "episode", "a", "new text", []float32{0, 1})
		if err != nil || !applied {
			t.Fatalf("matching text must apply: applied=%v err=%v", applied, err)
		}
		return nil
	})
	mustTx(t, s, base, func(x store.Tx) error {
		hits, _ := x.Index().SearchVector(ctx, []float32{0, 1}, 5)
		if len(hits) != 1 {
			t.Fatalf("SetEmbedding not applied")
		}
		return nil
	})
}

func embedFailuresParkItem(t *testing.T, s store.Store) {
	ctx := context.Background()
	put(t, s, base, "poison", "x", nil)
	put(t, s, base, "fine", "y", nil)
	for i := 0; i < store.MaxEmbedFailures; i++ {
		mustTx(t, s, base, func(x store.Tx) error {
			pending, err := x.Index().Pending(ctx, 10)
			if err != nil || len(pending) != 2 {
				t.Fatalf("attempt %d: pending %d err %v", i, len(pending), err)
			}
			return x.Index().MarkEmbedFailed(ctx, "episode", "poison")
		})
	}
	mustTx(t, s, base, func(x store.Tx) error {
		pending, _ := x.Index().Pending(ctx, 10)
		if len(pending) != 1 || pending[0].RefID != "fine" {
			t.Fatalf("a repeatedly failing item must be parked, got %+v", pending)
		}
		return nil
	})
	put(t, s, base, "poison", "x edited", nil) // new text gets a fresh start
	mustTx(t, s, base, func(x store.Tx) error {
		if pending, _ := x.Index().Pending(ctx, 10); len(pending) != 2 {
			t.Fatalf("upsert must reset failures, got %d pending", len(pending))
		}
		return nil
	})
}

var f0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fact(id, entity, rel, value string) reverie.Fact {
	return reverie.Fact{ID: id, Entity: entity, Relation: rel, Value: value, Confidence: 0.8, Source: reverie.SourceDistilled,
		Status: reverie.FactActive, ValidFrom: f0, RecordedAt: f0.Add(time.Hour)}
}

func saveFacts(t *testing.T, s store.Store, sc reverie.Scope, fs ...reverie.Fact) {
	t.Helper()
	mustTx(t, s, sc, func(x store.Tx) error {
		for _, f := range fs {
			if err := x.Facts().Save(context.Background(), f); err != nil {
				return err
			}
		}
		return nil
	})
}

func factsRoundtrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	want := fact("f1", "jay", "uses", "Go")
	want.EntityType, want.Pinned, want.Source = "person", true, reverie.SourceUser
	want.EpisodeIDs = []string{"ep-a", "ep-b"}
	saveFacts(t, s, base, want)
	want.Value = "Go 1.26" // upsert
	want.EpisodeIDs = []string{"ep-c"}
	saveFacts(t, s, base, want)
	mustTx(t, s, base, func(x store.Tx) error {
		got, err := x.Facts().Get(ctx, "f1")
		if err != nil {
			return err
		}
		if !got.ValidFrom.Equal(want.ValidFrom) || !got.RecordedAt.Equal(want.RecordedAt) || !got.ValidTo.IsZero() {
			t.Fatalf("times: %+v", got)
		}
		got.ValidFrom, got.RecordedAt = want.ValidFrom, want.RecordedAt
		if got.Value != "Go 1.26" || got.Source != reverie.SourceUser || !got.Pinned || got.EntityType != "person" ||
			len(got.EpisodeIDs) != 1 || got.EpisodeIDs[0] != "ep-c" {
			t.Fatalf("roundtrip: %+v", got)
		}
		if _, err := x.Facts().Get(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		return nil
	})
}

func factsLiveOnly(t *testing.T, s store.Store) {
	ctx := context.Background()
	active := fact("a", "jay", "lives_in", "London")
	contested := fact("c", "jay", "role", "lead")
	contested.Status = reverie.FactContested
	old := fact("o", "jay", "lives_in", "Lagos")
	old.Status, old.ValidTo, old.SupersededBy = reverie.FactSuperseded, f0.AddDate(0, 0, 10), "a"
	gone := fact("g", "jay", "note", "secret")
	gone.Status = reverie.FactTombstoned
	pinned := fact("p", "jay", "name", "Jay")
	pinned.Pinned = true
	saveFacts(t, s, base, active, contested, old, gone, pinned)

	mustTx(t, s, base, func(x store.Tx) error {
		all, _ := x.Facts().Query(ctx, store.FactQuery{})
		ids := map[string]bool{}
		for _, f := range all {
			ids[f.ID] = true
		}
		if len(all) != 3 || !ids["a"] || !ids["c"] || !ids["p"] {
			t.Fatalf("Query must return only active and contested believed facts, got %v", ids)
		}
		if got, _ := x.Facts().Query(ctx, store.FactQuery{OnlyContested: true}); len(got) != 1 || got[0].ID != "c" {
			t.Fatalf("contested filter: %+v", got)
		}
		if got, _ := x.Facts().Query(ctx, store.FactQuery{OnlyPinned: true}); len(got) != 1 || got[0].ID != "p" {
			t.Fatalf("pinned filter: %+v", got)
		}
		if got, _ := x.Facts().Query(ctx, store.FactQuery{Entity: " JAY ", Relation: "lives_in"}); len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("entity/relation filter must normalise case: %+v", got)
		}
		live, _ := x.Facts().Live(ctx, "jay", "lives_in")
		if len(live) != 1 || live[0].ID != "a" {
			t.Fatalf("Live: %+v", live)
		}
		return nil
	})
}

func factsAsOf(t *testing.T, s store.Store) {
	ctx := context.Background()
	lagos := fact("lagos", "jay", "lives_in", "Lagos")
	lagos.Status, lagos.ValidTo, lagos.SupersededBy = reverie.FactSuperseded, f0.AddDate(0, 0, 10), "london"
	london := fact("london", "jay", "lives_in", "London")
	london.ValidFrom = f0.AddDate(0, 0, 10)
	gone := fact("gone", "jay", "lives_in", "Mars")
	gone.Status = reverie.FactTombstoned
	saveFacts(t, s, base, lagos, london, gone)

	mustTx(t, s, base, func(x store.Tx) error {
		at := func(day int) string {
			got, err := x.Facts().AsOf(ctx, store.FactQuery{Entity: "jay", Relation: "lives_in"}, f0.AddDate(0, 0, day))
			if err != nil || len(got) != 1 {
				t.Fatalf("day %d: %+v err %v", day, got, err)
			}
			return got[0].Value
		}
		if v := at(5); v != "Lagos" {
			t.Fatalf("day 5: %s", v)
		}
		if v := at(10); v != "London" {
			t.Fatalf("the boundary day belongs to the new value: %s", v)
		}
		if v := at(50); v != "London" {
			t.Fatalf("day 50: %s", v)
		}
		if got, _ := x.Facts().AsOf(ctx, store.FactQuery{}, f0.AddDate(0, 0, -5)); len(got) != 0 {
			t.Fatalf("before anything was known: %+v", got)
		}
		return nil
	})
}

func factsRelated(t *testing.T, s store.Store) {
	ctx := context.Background()
	saveFacts(t, s, base,
		fact("1", "jay", "works_on", "Reverie"),
		fact("2", "reverie", "uses", "Go"),
		fact("3", "sam", "uses", "Rust"),
	)
	mustTx(t, s, base, func(x store.Tx) error {
		got, err := x.Facts().Related(ctx, []string{"Reverie"}, 10)
		if err != nil {
			return err
		}
		ids := map[string]bool{}
		for _, f := range got {
			ids[f.ID] = true
		}
		if len(got) != 2 || !ids["1"] || !ids["2"] {
			t.Fatalf("should match entity 'reverie' and value 'Reverie' case-insensitively: %+v", got)
		}
		if got, _ := x.Facts().Related(ctx, nil, 10); len(got) != 0 {
			t.Fatalf("no names, no facts")
		}
		if got, err := x.Facts().Related(ctx, []string{"x' OR '1'='1"}, 10); err != nil || len(got) != 0 {
			t.Fatalf("hostile name: %+v %v", got, err)
		}
		return nil
	})
}

func factsScoped(t *testing.T, s store.Store) {
	ctx := context.Background()
	saveFacts(t, s, base, fact("same-id", "jay", "uses", "Go"))
	other := variants()["user"]
	saveFacts(t, s, other, fact("same-id", "jay", "uses", "Rust")) // same id, another scope
	for sc, want := range map[reverie.Scope]string{base: "Go", other: "Rust"} {
		mustTx(t, s, sc, func(x store.Tx) error {
			f, err := x.Facts().Get(ctx, "same-id")
			if err != nil || f.Value != want {
				t.Fatalf("%s sees %q (err %v), want %q", sc.User, f.Value, err, want)
			}
			return nil
		})
	}
	for dim, sc := range variants() {
		if dim == "user" {
			continue
		}
		mustTx(t, s, sc, func(x store.Tx) error {
			if got, _ := x.Facts().Query(ctx, store.FactQuery{}); len(got) != 0 {
				t.Fatalf("%s: leaked %d facts", dim, len(got))
			}
			return nil
		})
	}
}

func journalBasics(t *testing.T, s store.Store) {
	ctx := context.Background()
	var id1, id2 int64
	mustTx(t, s, base, func(x store.Tx) (err error) {
		before := json.RawMessage(`{"value":"Lagos"}`)
		id1, err = x.Journal().Append(ctx, store.JournalEntry{Op: "supersede", TargetKind: "fact", TargetID: "f1", Before: before,
			After: json.RawMessage(`{"value":"London"}`), Rationale: "moved", SourceEpisodes: []string{"ep1"}, At: f0})
		if err != nil {
			return err
		}
		id2, err = x.Journal().Append(ctx, store.JournalEntry{RunID: "run1", Phase: "reconcile", Op: "create", TargetKind: "fact", TargetID: "f2", At: f0})
		return err
	})
	mustTx(t, s, base, func(x store.Tx) error {
		e, err := x.Journal().Get(ctx, id1)
		if err != nil || e.Op != "supersede" || string(e.Before) != `{"value":"Lagos"}` || len(e.SourceEpisodes) != 1 || e.Reverted || !e.At.Equal(f0) {
			t.Fatalf("get: %+v %v", e, err)
		}
		if e2, _ := x.Journal().Get(ctx, id2); e2.Before != nil || e2.After != nil {
			t.Fatalf("absent before/after must stay nil: %+v", e2)
		}
		if run, _ := x.Journal().ByRun(ctx, "run1"); len(run) != 1 || run[0].ID != id2 {
			t.Fatalf("by run: %+v", run)
		}
		if manual, _ := x.Journal().ByRun(ctx, ""); len(manual) != 1 || manual[0].ID != id1 {
			t.Fatalf("manual entries: %+v", manual)
		}
		if recent, _ := x.Journal().Recent(ctx, 1); len(recent) != 1 || recent[0].ID != id2 {
			t.Fatalf("recent should be newest first: %+v", recent)
		}
		if err := x.Journal().MarkReverted(ctx, id1); err != nil {
			return err
		}
		if e, _ := x.Journal().Get(ctx, id1); !e.Reverted {
			t.Fatal("not marked reverted")
		}
		if err := x.Journal().MarkReverted(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
		return nil
	})
	mustTx(t, s, variants()["user"], func(x store.Tx) error {
		if _, err := x.Journal().Get(ctx, id1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another scope read the journal: %v", err)
		}
		return nil
	})
}

func indexDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	put(t, s, base, "x", "findable words", []float32{1, 0})
	put(t, s, base, "y", "other words", []float32{0, 1})
	mustTx(t, s, base, func(x store.Tx) error { return x.Index().Delete(ctx, "episode", "x") })
	mustTx(t, s, base, func(x store.Tx) error {
		if hits, _ := x.Index().SearchText(ctx, "findable", 5); len(hits) != 0 {
			t.Fatal("deleted text still searchable")
		}
		if hits, _ := x.Index().SearchVector(ctx, []float32{1, 0}, 5); len(hits) != 1 || hits[0].RefID != "y" {
			t.Fatalf("deleted vector still searchable: %v", refs(hits))
		}
		return x.Index().Delete(ctx, "episode", "never-existed") // a no-op, not an error
	})
}

func updatePayload(t *testing.T, s store.Store) {
	ctx := context.Background()
	ev := event(base, "k", reverie.Message)
	mustTx(t, s, base, func(x store.Tx) error { _, err := x.Events().Append(ctx, ev); return err })
	mustTx(t, s, base, func(x store.Tx) error {
		if err := x.Events().UpdatePayload(ctx, ev.ID, []byte(`{"text":"clean"}`)); err != nil {
			return err
		}
		got, _ := x.Events().List(ctx, store.EventQuery{})
		if len(got) != 1 || string(got[0].Payload) != `{"text":"clean"}` {
			t.Fatalf("payload not rewritten: %+v", got)
		}
		return nil
	})
	mustTx(t, s, variants()["user"], func(x store.Tx) error {
		if err := x.Events().UpdatePayload(ctx, ev.ID, []byte(`{}`)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another scope rewrote the event: %v", err)
		}
		return nil
	})
}
