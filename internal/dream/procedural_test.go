package dream

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/store"
)

func text(kind reverie.EventKind, origin reverie.Origin, s string) reverie.Event {
	p, _ := json.Marshal(map[string]string{"text": s})
	return reverie.Event{ID: reverie.NewID(), Kind: kind, Origin: origin, Payload: p}
}

// toolTask records a task: the user's goal, then each tool call and result.
func toolTask(t *testing.T, sc store.Scoped, id string, at time.Time, outcome store.Outcome, goal string, tools ...string) {
	t.Helper()
	evs := []reverie.Event{text(reverie.Message, reverie.OriginUser, goal)}
	for _, tl := range tools {
		c, _ := json.Marshal(map[string]string{"tool": tl})
		r, _ := json.Marshal(map[string]string{"tool": tl, "output": "ok"})
		evs = append(evs,
			reverie.Event{ID: reverie.NewID(), Kind: reverie.ToolCalled, Origin: reverie.OriginAgent, Payload: c},
			reverie.Event{ID: reverie.NewID(), Kind: reverie.ToolResult, Origin: reverie.OriginTool, Payload: r})
	}
	appendTask(t, sc, id, at, outcome, evs...)
}

func skillVersions(t *testing.T, sc store.Scoped, id string) (out []store.Skill) {
	t.Helper()
	if err := sc.View(context.Background(), func(tx store.Tx) (err error) {
		out, err = tx.Skills().Versions(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func episode(t *testing.T, sc store.Scoped, id string) (ep store.Episode) {
	t.Helper()
	if err := sc.View(context.Background(), func(tx store.Tx) (err error) {
		ep, err = tx.Episodes().Get(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ep
}

func revert(t *testing.T, sc store.Scoped, run string, at time.Time) {
	t.Helper()
	if err := sc.Tx(context.Background(), func(tx store.Tx) error {
		_, err := facts.RevertRun(context.Background(), tx, run, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

var deploySteps = []string{"git_status", "npm_build", "vercel_deploy"}

func TestSynthesizeDraftsCandidateNewVersionAndReverts(t *testing.T) {
	st := openTestStore(t)
	sc := st.For(testScope)
	ctx := context.Background()

	// One success is not a pattern; a failure with the same tools does not count.
	toolTask(t, sc, "deploy:1", d0, store.Success, "deploy the frontend", "git_status", "npm_build", "npm_build", "vercel_deploy")
	toolTask(t, sc, "deploy:x", d0.Add(time.Minute), store.Failure, "deploy the frontend", deploySteps...)
	rep1, err := Run(ctx, sc, Config{Now: d0.Add(time.Hour)})
	if err != nil || rep1.SkillsProposed != 0 || len(skillVersions(t, sc, "deploy")) != 0 {
		t.Fatalf("one success drafted a skill: %+v %v", rep1, err)
	}

	// A second success in a later dream: the first is found by looking back.
	toolTask(t, sc, "deploy:2", d0.Add(2*time.Hour), store.Success, "deploy frontend to prod", deploySteps...)
	rep2, err := Run(ctx, sc, Config{Now: d0.Add(3 * time.Hour)})
	if err != nil || rep2.SkillsProposed != 1 {
		t.Fatalf("second dream: %+v %v", rep2, err)
	}
	vs := skillVersions(t, sc, "deploy")
	if len(vs) != 1 || vs[0].Version != 1 || vs[0].Status != store.SkillCandidate || !slices.Equal(vs[0].Steps, deploySteps) ||
		!slices.Equal(vs[0].EpisodeIDs, []string{"deploy:2", "deploy:1"}) || vs[0].Uses != 0 || vs[0].Agent != "" {
		t.Fatalf("draft: %+v", vs)
	}
	var entries []store.JournalEntry
	_ = sc.View(ctx, func(tx store.Tx) (err error) {
		entries, err = tx.Journal().ByRun(ctx, rep2.RunID)
		return err
	})
	if !slices.ContainsFunc(entries, func(e store.JournalEntry) bool {
		return e.TargetKind == "skill" && e.TargetID == "deploy@v1" && e.Op == "create" && e.Phase == "synthesize"
	}) {
		t.Fatalf("skill create not journaled: %+v", entries)
	}

	// The same procedure again adds no version; a new procedure for the same
	// kind of task becomes v2, even though v1's evidence outnumbers it.
	toolTask(t, sc, "deploy:3", d0.Add(4*time.Hour), store.Success, "deploy the frontend", deploySteps...)
	toolTask(t, sc, "deploy:4", d0.Add(5*time.Hour), store.Success, "deploy via containers", "docker_build", "kubectl_apply")
	toolTask(t, sc, "deploy:5", d0.Add(6*time.Hour), store.Success, "deploy via containers", "docker_build", "kubectl_apply")
	rep3, err := Run(ctx, sc, Config{Now: d0.Add(7 * time.Hour)})
	if err != nil || rep3.SkillsProposed != 1 {
		t.Fatalf("third dream: %+v %v", rep3, err)
	}
	vs = skillVersions(t, sc, "deploy")
	if len(vs) != 2 || vs[1].Version != 2 || !slices.Equal(vs[1].Steps, []string{"docker_build", "kubectl_apply"}) || vs[1].Status != store.SkillCandidate {
		t.Fatalf("v2: %+v", vs)
	}

	// Reverting the second dream withdraws v1 and un-consolidates only what
	// that dream consolidated: deploy:1 was consolidated by the first dream.
	revert(t, sc, rep2.RunID, d0.Add(8*time.Hour))
	if vs := skillVersions(t, sc, "deploy"); vs[0].Status != store.SkillDeprecated || vs[1].Status != store.SkillCandidate {
		t.Fatalf("after revert: %+v", vs)
	}
	if !episode(t, sc, "deploy:1").Consolidated {
		t.Fatal("revert un-consolidated an episode the reverted dream only looked back at")
	}
	if episode(t, sc, "deploy:2").Consolidated {
		t.Fatal("revert left the dream's own episode consolidated")
	}
}

func TestSynthesizeIgnoresRecalledToolCalls(t *testing.T) {
	st := openTestStore(t)
	sc := st.For(testScope)
	for i, id := range []string{"echo:1", "echo:2"} {
		c, _ := json.Marshal(map[string]string{"tool": "memory_recall"})
		evs := []reverie.Event{text(reverie.Message, reverie.OriginUser, "do it")}
		for j := 0; j < 3; j++ {
			evs = append(evs, reverie.Event{ID: reverie.NewID(), Kind: reverie.ToolCalled, Origin: reverie.OriginRecalled, Payload: c})
		}
		appendTask(t, sc, id, d0.Add(time.Duration(i)*time.Minute), store.Success, evs...)
	}
	rep, err := Run(context.Background(), sc, Config{Now: d0.Add(time.Hour)})
	if err != nil || rep.SkillsProposed != 0 {
		t.Fatalf("recalled calls drafted a skill: %+v %v", rep, err)
	}
}

func countFacts(t *testing.T, sc store.Scoped) int {
	t.Helper()
	var fs []reverie.Fact
	_ = sc.View(context.Background(), func(tx store.Tx) (err error) {
		fs, err = tx.Facts().Query(context.Background(), store.FactQuery{})
		return err
	})
	return len(fs)
}

func TestRecombineInsightIsCappedHiddenUntilConfirmedAndNeverAFact(t *testing.T) {
	st := openTestStore(t)
	sc := st.For(testScope)
	ctx := context.Background()
	model := &llm.Fake{Rules: []llm.Rule{{
		Match:  "Shared:",
		Output: map[string]any{"insights": []map[string]any{{"body": "stripe webhook retries spike right after deploys", "confidence": 0.95}}},
		Tokens: 120,
	}}}
	appendTask(t, sc, "billing:1", d0, store.Success, text(reverie.Message, reverie.OriginUser, "fix stripe webhook retries"))
	appendTask(t, sc, "release:1", d0.Add(time.Minute), store.Success, text(reverie.Message, reverie.OriginUser, "deploy stripe webhook handler"))

	rep, err := Run(ctx, sc, Config{Now: d0.Add(time.Hour), LLM: model, Recombine: true})
	if err != nil || rep.InsightsProposed != 1 {
		t.Fatalf("recombine: %+v %v", rep, err)
	}
	var ins []store.Insight
	_ = sc.View(ctx, func(tx store.Tx) (err error) {
		ins, err = tx.Insights().List(ctx, store.InsightQuery{})
		return err
	})
	if len(ins) != 1 || ins[0].Confidence != 0.5 || ins[0].Status != store.InsightSpeculative ||
		!slices.Equal(slices.Sorted(slices.Values(ins[0].EpisodeIDs)), []string{"billing:1", "release:1"}) || !slices.Contains(ins[0].Entities, "stripe") {
		t.Fatalf("insight: %+v", ins)
	}
	pack := func() reverie.ContextPack {
		p, err := recall.Run(ctx, sc, recall.Query{Task: "stripe webhook", Budget: 1000, Now: d0.Add(10 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := pack(); len(p.Insights) != 0 {
		t.Fatalf("speculative insight surfaced: %+v", p.Insights)
	}
	if n := countFacts(t, sc); n != 0 {
		t.Fatalf("recombine wrote %d fact(s)", n)
	}

	// A later, independent episode about the same things confirms it.
	appendTask(t, sc, "support:1", d0.Add(2*time.Hour), store.Success, text(reverie.Message, reverie.OriginUser, "customers report stripe webhook alerts"))
	rep2, err := Run(ctx, sc, Config{Now: d0.Add(3 * time.Hour), LLM: model, Recombine: true})
	if err != nil || rep2.InsightsConfirmed != 1 {
		t.Fatalf("confirm: %+v %v", rep2, err)
	}
	p := pack()
	if len(p.Insights) != 1 || !strings.Contains(p.Insights[0].Text, "speculative insight, not a fact") ||
		!strings.Contains(p.Insights[0].Text, "confirmed by support:1") || !strings.Contains(p.Insights[0].Text, "confidence 0.50") {
		t.Fatalf("confirmed insight render: %+v", p.Insights)
	}
	if len(p.Facts) != 0 || countFacts(t, sc) != 0 {
		t.Fatalf("a confirmed insight became a fact: %+v", p.Facts)
	}

	// Reverting the confirming dream hides it again.
	revert(t, sc, rep2.RunID, d0.Add(4*time.Hour))
	if p := pack(); len(p.Insights) != 0 {
		t.Fatalf("reverted confirmation still surfaces: %+v", p.Insights)
	}
}

func TestRecombineIsOptInAndFirstSkippedOnBudget(t *testing.T) {
	st := openTestStore(t)
	sc := st.For(testScope)
	ctx := context.Background()
	model := &llm.Fake{}
	appendTask(t, sc, "billing:1", d0, store.Success, text(reverie.Message, reverie.OriginUser, "fix stripe webhook retries"))
	appendTask(t, sc, "release:1", d0.Add(time.Minute), store.Success, text(reverie.Message, reverie.OriginUser, "deploy stripe webhook handler"))
	if _, err := Run(ctx, sc, Config{Now: d0.Add(time.Hour), LLM: model}); err != nil {
		t.Fatal(err)
	}
	for _, c := range model.Calls {
		if strings.Contains(c.Prompt, "Shared:") {
			t.Fatal("recombine ran without being enabled")
		}
	}

	appendTask(t, sc, "billing:2", d0.Add(2*time.Hour), store.Success, text(reverie.Message, reverie.OriginUser, "stripe webhook signature check"))
	appendTask(t, sc, "release:2", d0.Add(2*time.Hour+time.Minute), store.Success, text(reverie.Message, reverie.OriginUser, "release stripe webhook fix"))
	rep, err := Run(ctx, sc, Config{Now: d0.Add(3 * time.Hour), LLM: model, Recombine: true, Budget: 150})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rep.Skipped, "recombine") || slices.Contains(rep.Skipped, "distill") {
		t.Fatalf("budget order: skipped %v", rep.Skipped)
	}
}
