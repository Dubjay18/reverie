package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
)

func skill(id string, v int, status store.SkillStatus, at time.Time) store.Skill {
	return store.Skill{
		ID: id, Version: v, Name: id, Trigger: "when " + id, Steps: []string{"read", "edit", "test"},
		Status: status, Uses: 2, Successes: 1, EpisodeIDs: []string{"e1", "e2"},
		CreatedAt: at, UpdatedAt: at,
	}
}

func skillsRoundtrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	mustTx(t, s, base, func(x store.Tx) error {
		v1 := skill("deploy", 1, store.SkillActive, t0)
		v1.LastUsed = t0.Add(time.Hour)
		for _, sk := range []store.Skill{v1, skill("deploy", 2, store.SkillCandidate, t0.Add(time.Minute)), skill("lint", 1, store.SkillCandidate, t0.Add(2*time.Minute))} {
			if err := x.Skills().Save(ctx, sk); err != nil {
				return err
			}
		}
		got, err := x.Skills().Get(ctx, "deploy", 1)
		if err != nil {
			return err
		}
		if got.Status != store.SkillActive || !slices.Equal(got.Steps, []string{"read", "edit", "test"}) ||
			!slices.Equal(got.EpisodeIDs, []string{"e1", "e2"}) || got.Uses != 2 || got.Successes != 1 ||
			!got.CreatedAt.Equal(t0) || !got.LastUsed.Equal(t0.Add(time.Hour)) || got.Trigger != "when deploy" {
			t.Fatalf("roundtrip: %+v", got)
		}
		latest, err := x.Skills().Get(ctx, "deploy", 0)
		if err != nil || latest.Version != 2 || !latest.LastUsed.IsZero() {
			t.Fatalf("latest: %+v %v", latest, err)
		}
		vs, err := x.Skills().Versions(ctx, "deploy")
		if err != nil || len(vs) != 2 || vs[0].Version != 1 || vs[1].Version != 2 {
			t.Fatalf("versions: %+v %v", vs, err)
		}
		cands, err := x.Skills().List(ctx, store.SkillQuery{Status: store.SkillCandidate})
		if err != nil || len(cands) != 2 || cands[0].ID != "lint" {
			t.Fatalf("candidates newest first: %+v %v", cands, err)
		}
		all, err := x.Skills().List(ctx, store.SkillQuery{Limit: 2})
		if err != nil || len(all) != 2 {
			t.Fatalf("limit: %+v %v", all, err)
		}
		// upsert
		v1.Status, v1.Uses = store.SkillDeprecated, 9
		if err := x.Skills().Save(ctx, v1); err != nil {
			return err
		}
		got, err = x.Skills().Get(ctx, "deploy", 1)
		if err != nil || got.Status != store.SkillDeprecated || got.Uses != 9 {
			t.Fatalf("upsert: %+v %v", got, err)
		}
		if _, err := x.Skills().Get(ctx, "nope", 0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing skill: %v", err)
		}
		return nil
	})
}

// Skills belong to (tenant, project): another user or agent in the project
// sees a shared skill, another project or tenant does not, and a skill
// narrowed to one agent is invisible to, and cannot be overwritten by, others.
func skillsScope(t *testing.T, s store.Store) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	mustTx(t, s, base, func(x store.Tx) error {
		if err := x.Skills().Save(ctx, skill("shared", 1, store.SkillActive, t0)); err != nil {
			return err
		}
		mine := skill("mine", 1, store.SkillActive, t0)
		mine.Agent = base.Agent
		if err := x.Skills().Save(ctx, mine); err != nil {
			return err
		}
		foreign := skill("theirs", 1, store.SkillActive, t0)
		foreign.Agent = "someone-else"
		if err := x.Skills().Save(ctx, foreign); !errors.Is(err, store.ErrScopeMismatch) {
			t.Fatalf("saving another agent's skill: %v", err)
		}
		return nil
	})
	v := variants()
	for name, sc := range v {
		mustTx(t, s, sc, func(x store.Tx) error {
			_, errShared := x.Skills().Get(ctx, "shared", 1)
			_, errMine := x.Skills().Get(ctx, "mine", 1)
			list, err := x.Skills().List(ctx, store.SkillQuery{})
			if err != nil {
				return err
			}
			switch name {
			case "user":
				if errShared != nil || errMine != nil || len(list) != 2 {
					t.Fatalf("same project and agent, other user should see both: %v %v %d", errShared, errMine, len(list))
				}
			case "agent":
				if errShared != nil || !errors.Is(errMine, store.ErrNotFound) || len(list) != 1 {
					t.Fatalf("other agent should see only the shared skill: %v %v %d", errShared, errMine, len(list))
				}
				// and cannot overwrite the narrowed one by reusing its key
				hijack := skill("mine", 1, store.SkillDeprecated, t0)
				if err := x.Skills().Save(ctx, hijack); !errors.Is(err, store.ErrScopeMismatch) {
					t.Fatalf("other agent overwrote a narrowed skill: %v", err)
				}
			default: // tenant, project
				if !errors.Is(errShared, store.ErrNotFound) || len(list) != 0 {
					t.Fatalf("%s leak: %v %d", name, errShared, len(list))
				}
			}
			return nil
		})
	}
	mustTx(t, s, base, func(x store.Tx) error {
		got, err := x.Skills().Get(ctx, "mine", 1)
		if err != nil || got.Status != store.SkillActive || got.Agent != base.Agent {
			t.Fatalf("narrowed skill changed: %+v %v", got, err)
		}
		return nil
	})
}

func insightsRoundtrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	mustTx(t, s, base, func(x store.Tx) error {
		a := store.Insight{ID: "i1", Body: "deploys fail when the cache is cold", Entities: []string{"deploy", "cache"},
			Confidence: 0.5, Status: store.InsightSpeculative, EpisodeIDs: []string{"e1", "e2"}, CreatedAt: t0}
		b := store.Insight{ID: "i2", Body: "b", Confidence: 0.4, Status: store.InsightConfirmed, EpisodeIDs: []string{"e3", "e4"},
			ConfirmedBy: []string{"e5"}, CreatedAt: t0.Add(time.Minute), ConfirmedAt: t0.Add(time.Hour)}
		for _, in := range []store.Insight{a, b} {
			if err := x.Insights().Save(ctx, in); err != nil {
				return err
			}
		}
		got, err := x.Insights().Get(ctx, "i1")
		if err != nil || got.Body != a.Body || !slices.Equal(got.Entities, a.Entities) || got.Confidence != 0.5 ||
			got.Status != store.InsightSpeculative || !slices.Equal(got.EpisodeIDs, a.EpisodeIDs) ||
			len(got.ConfirmedBy) != 0 || !got.ConfirmedAt.IsZero() || !got.CreatedAt.Equal(t0) {
			t.Fatalf("roundtrip: %+v %v", got, err)
		}
		conf, err := x.Insights().List(ctx, store.InsightQuery{Status: store.InsightConfirmed})
		if err != nil || len(conf) != 1 || conf[0].ID != "i2" || !slices.Equal(conf[0].ConfirmedBy, []string{"e5"}) || !conf[0].ConfirmedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("confirmed: %+v %v", conf, err)
		}
		all, err := x.Insights().List(ctx, store.InsightQuery{})
		if err != nil || len(all) != 2 || all[0].ID != "i2" {
			t.Fatalf("newest first: %+v %v", all, err)
		}
		a.Status = store.InsightRejected
		if err := x.Insights().Save(ctx, a); err != nil {
			return err
		}
		if got, _ := x.Insights().Get(ctx, "i1"); got.Status != store.InsightRejected {
			t.Fatalf("upsert: %+v", got)
		}
		if _, err := x.Insights().Get(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		return nil
	})
	for name, sc := range variants() {
		mustTx(t, s, sc, func(x store.Tx) error {
			if _, err := x.Insights().Get(ctx, "i1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s leak: %v", name, err)
			}
			if l, err := x.Insights().List(ctx, store.InsightQuery{}); err != nil || len(l) != 0 {
				t.Fatalf("%s list leak: %d %v", name, len(l), err)
			}
			return nil
		})
	}
}

func proceduralViewReadOnly(t *testing.T, s store.Store) {
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	err := s.For(base).View(ctx, func(x store.Tx) error {
		return x.Skills().Save(ctx, skill("x", 1, store.SkillCandidate, t0))
	})
	err2 := s.For(base).View(ctx, func(x store.Tx) error {
		return x.Insights().Save(ctx, store.Insight{ID: "x", Body: "x", Status: store.InsightSpeculative, CreatedAt: t0})
	})
	if err == nil || err2 == nil {
		t.Fatalf("view wrote: %v %v", err, err2)
	}
	mustTx(t, s, base, func(x store.Tx) error {
		if _, err := x.Skills().Get(ctx, "x", 0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("skill write through view persisted: %v", err)
		}
		if _, err := x.Insights().Get(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("insight write through view persisted: %v", err)
		}
		return nil
	})
}
