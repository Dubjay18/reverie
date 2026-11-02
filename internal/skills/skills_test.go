package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/sqlite"
)

var sc = reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u"}
var t0 = time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

func open(t *testing.T) store.Scoped {
	t.Helper()
	st, err := sqlite.Open(t.TempDir()+"/skills.db", sqlite.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.For(sc)
}

func put(t *testing.T, s store.Scoped, sk store.Skill) {
	t.Helper()
	if sk.CreatedAt.IsZero() {
		sk.CreatedAt, sk.UpdatedAt = t0, t0
	}
	if err := s.Tx(context.Background(), func(tx store.Tx) error { return tx.Skills().Save(context.Background(), sk) }); err != nil {
		t.Fatal(err)
	}
}

func use(t *testing.T, s store.Scoped, id string, v int, ok bool) (store.Skill, skills.Transition) {
	t.Helper()
	var sk store.Skill
	var tr skills.Transition
	err := s.Tx(context.Background(), func(tx store.Tx) (err error) {
		sk, tr, err = skills.RecordUse(context.Background(), tx, id, v, ok, t0, skills.NewRunID())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sk, tr
}

func get(t *testing.T, s store.Scoped, id string, v int) store.Skill {
	t.Helper()
	var sk store.Skill
	if err := s.View(context.Background(), func(tx store.Tx) (err error) {
		sk, err = tx.Skills().Get(context.Background(), id, v)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return sk
}

func TestRateIsBetaMean(t *testing.T) {
	for _, c := range []struct {
		uses, ok int
		want     float64
	}{{0, 0, 0.5}, {1, 1, 2.0 / 3}, {3, 3, 0.8}, {10, 0, 1.0 / 12}} {
		if got := skills.Rate(store.Skill{Uses: c.uses, Successes: c.ok}); got != c.want {
			t.Fatalf("rate(%d/%d) = %v, want %v", c.ok, c.uses, got, c.want)
		}
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]struct {
		id string
		v  int
	}{"skill:deploy@v2": {"deploy", 2}, "deploy@v3 @2026": {"deploy", 3}, "deploy": {"deploy", 0}} {
		id, v, err := skills.ParseRef(in)
		if err != nil || id != want.id || v != want.v {
			t.Fatalf("%q -> %q %d %v", in, id, v, err)
		}
	}
	for _, bad := range []string{"", "skill:", "deploy@v0", "deploy@vx"} {
		if _, _, err := skills.ParseRef(bad); !errors.Is(err, skills.ErrBadRef) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// The promotion test from the spec: a candidate is offered as a proposed
// method, becomes active after 3 successful uses, and when the environment
// changes and it keeps failing, its rate drops and it is deprecated.
func TestCandidateGatingPromotionAndDeprecation(t *testing.T) {
	s := open(t)
	put(t, s, store.Skill{ID: "deploy", Version: 1, Name: "deploy", Trigger: "deploy frontend", Steps: []string{"build", "ship"}, Status: store.SkillCandidate})
	pack := func() reverie.ContextPack {
		p, err := recall.Run(context.Background(), s, recall.Query{Task: "deploy the frontend", Budget: 800, Now: t0})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := pack(); len(p.Skills) != 1 || !strings.Contains(p.Skills[0].Text, "proposed method") || !strings.HasPrefix(p.Skills[0].Cite, "skill:deploy@v1") {
		t.Fatalf("candidate should be offered as a proposed method: %+v", p.Skills)
	}

	// A failure in between does not count toward the gate.
	use(t, s, "deploy", 1, true)
	use(t, s, "deploy", 1, false)
	if sk, tr := use(t, s, "deploy", 1, true); tr != skills.Kept || sk.Status != store.SkillCandidate {
		t.Fatalf("promoted after 2 successes: %+v %v", sk, tr)
	}
	sk, tr := use(t, s, "deploy", 1, true)
	if tr != skills.Promoted || sk.Status != store.SkillActive || sk.Successes != 3 || sk.Uses != 4 {
		t.Fatalf("third success should promote: %+v %v", sk, tr)
	}
	if p := pack(); len(p.Skills) != 1 || !strings.Contains(p.Skills[0].Text, "[skill; success rate 0.67 over 4 uses") {
		t.Fatalf("active skill render: %+v", p.Skills)
	}

	// The environment changes: it fails until the Beta rate is below 0.5.
	var last skills.Transition
	fails := 0
	for last != skills.Deprecate {
		_, last = use(t, s, "deploy", 1, false)
		if fails++; fails > 10 {
			t.Fatal("never deprecated")
		}
	}
	got := get(t, s, "deploy", 1)
	if got.Status != store.SkillDeprecated || skills.Rate(got) >= skills.MinRate {
		t.Fatalf("deprecated: %+v rate %.2f", got, skills.Rate(got))
	}
	if p := pack(); len(p.Skills) != 0 {
		t.Fatalf("deprecated skill offered: %+v", p.Skills)
	}
	err := s.Tx(context.Background(), func(tx store.Tx) error {
		_, _, err := skills.RecordUse(context.Background(), tx, "deploy", 1, true, t0, "x")
		return err
	})
	if !errors.Is(err, skills.ErrDeprecated) {
		t.Fatalf("use of deprecated skill: %v", err)
	}
}

// Three lucky successes are not enough if they are buried in failures.
func TestPromotionNeedsRateNotJustCount(t *testing.T) {
	s := open(t)
	put(t, s, store.Skill{ID: "flaky", Version: 1, Name: "flaky", Steps: []string{"a", "b"}, Status: store.SkillCandidate})
	for i := 0; i < 5; i++ {
		use(t, s, "flaky", 1, false)
	}
	for i := 0; i < 3; i++ {
		if _, tr := use(t, s, "flaky", 1, true); tr == skills.Promoted {
			t.Fatalf("promoted with rate %.2f", skills.Rate(get(t, s, "flaky", 1)))
		}
	}
}

func TestPromotionReplacesActiveVersionAndRollbackRestoresIt(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	put(t, s, store.Skill{ID: "deploy", Version: 1, Name: "deploy", Steps: []string{"build", "ship"}, Status: store.SkillActive, Uses: 4, Successes: 4})
	put(t, s, store.Skill{ID: "deploy", Version: 2, Name: "deploy", Steps: []string{"build", "test", "ship"}, Status: store.SkillCandidate})
	for i := 0; i < 3; i++ {
		use(t, s, "deploy", 2, true)
	}
	if v1, v2 := get(t, s, "deploy", 1), get(t, s, "deploy", 2); v1.Status != store.SkillDeprecated || v2.Status != store.SkillActive {
		t.Fatalf("one active version: v1 %s v2 %s", v1.Status, v2.Status)
	}

	run := skills.NewRunID()
	var restored store.Skill
	if err := s.Tx(ctx, func(tx store.Tx) (err error) {
		restored, err = skills.Rollback(ctx, tx, "deploy", t0, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if restored.Version != 1 || get(t, s, "deploy", 1).Status != store.SkillActive || get(t, s, "deploy", 2).Status != store.SkillDeprecated {
		t.Fatalf("rollback: restored %+v", restored)
	}
	// Rollback is journaled, so it can be reverted like any run.
	if err := s.Tx(ctx, func(tx store.Tx) error {
		_, err := facts.RevertRun(ctx, tx, run, t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if get(t, s, "deploy", 1).Status != store.SkillDeprecated || get(t, s, "deploy", 2).Status != store.SkillActive {
		t.Fatal("revert of rollback did not restore v2")
	}
	// With nothing proven to go back to, rollback withdraws the skill.
	put(t, s, store.Skill{ID: "solo", Version: 1, Name: "solo", Steps: []string{"a", "b"}, Status: store.SkillActive})
	if err := s.Tx(ctx, func(tx store.Tx) (err error) {
		restored, err = skills.Rollback(ctx, tx, "solo", t0, skills.NewRunID())
		return err
	}); err != nil || restored.ID != "" || get(t, s, "solo", 1).Status != store.SkillDeprecated {
		t.Fatalf("solo rollback: %+v %v", restored, err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		_, err := skills.Rollback(ctx, tx, "solo", t0, skills.NewRunID())
		return err
	}); !errors.Is(err, skills.ErrNotActive) {
		t.Fatalf("rollback with nothing active: %v", err)
	}
}

func call(tool string, origin reverie.Origin) reverie.Event {
	p, _ := json.Marshal(map[string]string{"tool": tool})
	return reverie.NewEvent(sc, "x", reverie.ToolCalled, origin, p)
}

func TestToolSequenceCollapsesRepeatsAndIgnoresRecalled(t *testing.T) {
	evs := []reverie.Event{
		call("Bash", reverie.OriginAgent), call("Bash", reverie.OriginAgent),
		call("memory_recall", reverie.OriginRecalled),
		{Kind: reverie.ToolResult, Origin: reverie.OriginTool, Payload: json.RawMessage(`{"tool":"Bash"}`)},
		call("Edit File", reverie.OriginAgent), call("Bash", reverie.OriginAgent),
	}
	if got := skills.ToolSequence(evs); !slices.Equal(got, []string{"bash", "edit_file", "bash"}) {
		t.Fatalf("sequence: %v", got)
	}
}

func TestSynthesizeNeedsSupportAndVersionsByKey(t *testing.T) {
	a := []string{"git_status", "npm_build", "vercel_deploy"}
	b := []string{"git_status", "npm_test", "npm_build", "vercel_deploy"} // similar: 2*3/7
	c := []string{"docker_build", "kubectl_apply"}
	if got := skills.Synthesize([]skills.Trace{{EpisodeID: "e1", Key: "deploy", Tools: a}}); len(got) != 0 {
		t.Fatalf("one episode is not enough: %+v", got)
	}
	got := skills.Synthesize([]skills.Trace{
		{EpisodeID: "e1", Key: "deploy", Goal: "deploy the frontend", Tools: a},
		{EpisodeID: "e2", Key: "deploy", Goal: "deploy frontend to prod", Tools: b},
		{EpisodeID: "e3", Key: "deploy", Tools: c},
		{EpisodeID: "e4", Key: "deploy", Tools: c},
		{EpisodeID: "e5", Key: "deploy", Tools: c},
		{EpisodeID: "e6", Key: "lint", Tools: []string{"one"}}, // too short
	})
	if len(got) != 2 || got[0].ID != "deploy" || got[1].ID != "deploy" {
		t.Fatalf("drafts: %+v", got)
	}
	// best supported first; steps are what every trace shares
	if !slices.Equal(got[0].Steps, c) || len(got[0].EpisodeIDs) != 3 {
		t.Fatalf("first draft: %+v", got[0])
	}
	if !slices.Equal(got[1].Steps, a) || !slices.Equal(got[1].EpisodeIDs, []string{"e1", "e2"}) ||
		got[1].Trigger != "deploy: frontend prod" {
		t.Fatalf("second draft: %+v", got[1])
	}
	if s := skills.Similarity(a, b); s < skills.Similar {
		t.Fatalf("similarity %v", s)
	}
}
