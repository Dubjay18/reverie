// Package evals implements the M5 evaluation harness:
//   - T5.1: Scenario YAML schema and runner (amnesia, contradiction, staleness, isolation, continuity)
//   - T5.2: Four memory configurations (no_memory, full_context, raw_history_search, reverie)
//   - T5.3: LongMemEval and LoCoMo subset adapters and category breakdown
//   - T5.4: Markdown results table generator (accuracy, tokens/query, dream cost, method)
package evals

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/dream"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed scenarios/*.yaml testdata/*.json
var embeddedFS embed.FS

// ConfigMode identifies one of the four evaluated memory configurations (T5.2).
type ConfigMode string

const (
	ConfigNoMemory         ConfigMode = "no_memory"
	ConfigFullContext      ConfigMode = "full_context"
	ConfigRawHistorySearch ConfigMode = "raw_history_search"
	ConfigReverie          ConfigMode = "reverie"
)

// AllConfigs lists the four configurations required by T5.2.
var AllConfigs = []ConfigMode{
	ConfigNoMemory,
	ConfigFullContext,
	ConfigRawHistorySearch,
	ConfigReverie,
}

// Scenario is a multi-session memory evaluation scenario loaded from YAML.
type Scenario struct {
	ID          string            `yaml:"id" json:"id"`
	Category    string            `yaml:"category" json:"category"`
	Description string            `yaml:"description" json:"description"`
	Sessions    []ScenarioSession `yaml:"sessions" json:"sessions"`
	Queries     []ScenarioQuery   `yaml:"queries" json:"queries"`
}

// ScenarioSession represents one session/episode recorded into memory.
type ScenarioSession struct {
	TaskID      string          `yaml:"task_id" json:"task_id"`
	Scope       reverie.Scope   `yaml:"scope" json:"scope"`
	At          string          `yaml:"at" json:"at"`
	FillerCount int             `yaml:"filler_count,omitempty" json:"filler_count,omitempty"`
	Events      []ScenarioEvent `yaml:"events" json:"events"`
	Remember    []ScenarioFact  `yaml:"remember,omitempty" json:"remember,omitempty"`
	Dream       bool            `yaml:"dream,omitempty" json:"dream,omitempty"`
	DreamFacts  []ScenarioFact  `yaml:"dream_facts,omitempty" json:"dream_facts,omitempty"`
	PinValues   []string        `yaml:"pin_values,omitempty" json:"pin_values,omitempty"`
}

// ScenarioEvent is a single event in a ScenarioSession.
type ScenarioEvent struct {
	Kind    string `yaml:"kind" json:"kind"`
	Origin  string `yaml:"origin" json:"origin"`
	Text    string `yaml:"text,omitempty" json:"text,omitempty"`
	Outcome string `yaml:"outcome,omitempty" json:"outcome,omitempty"`
	Summary string `yaml:"summary,omitempty" json:"summary,omitempty"`
}

// ScenarioFact is a synchronous Remember input or a canned Dream distillation output.
type ScenarioFact struct {
	Entity     string  `yaml:"entity" json:"entity"`
	Relation   string  `yaml:"relation" json:"relation"`
	Value      string  `yaml:"value" json:"value"`
	Source     string  `yaml:"source,omitempty" json:"source,omitempty"`
	Confidence float64 `yaml:"confidence,omitempty" json:"confidence,omitempty"`
}

// ScenarioQuery is an evaluation query with positive and negative substring assertions.
type ScenarioQuery struct {
	ID             string        `yaml:"id" json:"id"`
	Scope          reverie.Scope `yaml:"scope" json:"scope"`
	At             string        `yaml:"at" json:"at"`
	Task           string        `yaml:"task" json:"task"`
	Budget         int           `yaml:"budget" json:"budget"`
	MustContain    []string      `yaml:"must_contain" json:"must_contain"`
	MustNotContain []string      `yaml:"must_not_contain,omitempty" json:"must_not_contain,omitempty"`
}

// QueryOutcome records the result of running one ScenarioQuery under one ConfigMode.
type QueryOutcome struct {
	ScenarioID  string
	Category    string
	QueryID     string
	Config      ConfigMode
	Passed      bool
	TokensQuery int
	DreamTokens int
	ContextText string
	Failure     string
}

// ConfigSummary aggregates results for one ConfigMode across a suite of scenarios.
type ConfigSummary struct {
	Config         ConfigMode
	Queries        int
	Passed         int
	Accuracy       float64
	AvgQueryTokens float64
	TotalDreamToks int
	ByCategory     map[string]CategoryStat
}

// CategoryStat tracks accuracy within a single scenario or benchmark category.
type CategoryStat struct {
	Category string
	Total    int
	Passed   int
	Accuracy float64
}

// LoadBuiltinScenarios parses all embedded YAML scenarios under scenarios/*.yaml.
func LoadBuiltinScenarios() ([]Scenario, error) {
	entries, err := embeddedFS.ReadDir("scenarios")
	if err != nil {
		return nil, err
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := embeddedFS.ReadFile(filepath.Join("scenarios", e.Name()))
		if err != nil {
			return nil, err
		}
		var sc Scenario
		if err := yaml.Unmarshal(raw, &sc); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if sc.ID == "" {
			return nil, fmt.Errorf("%s: missing scenario id", e.Name())
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RunScenario executes a Scenario under the given ConfigMode and returns one
// QueryOutcome per ScenarioQuery.
func RunScenario(ctx context.Context, tempDir string, sc Scenario, mode ConfigMode) ([]QueryOutcome, error) {
	switch mode {
	case ConfigNoMemory:
		return runNoMemory(sc), nil
	case ConfigFullContext:
		return runFullContext(sc), nil
	case ConfigRawHistorySearch:
		return runRawHistorySearch(sc), nil
	case ConfigReverie:
		return runReverie(ctx, tempDir, sc)
	default:
		return nil, fmt.Errorf("unknown config mode %q", mode)
	}
}

func approxTokens(s string) int {
	return recall.Approx{}.Count(s)
}

func evaluateAssertions(text string, q ScenarioQuery) (bool, string) {
	lower := strings.ToLower(text)
	for _, want := range q.MustContain {
		if !strings.Contains(lower, strings.ToLower(want)) {
			return false, fmt.Sprintf("missing %q", want)
		}
	}
	for _, forbid := range q.MustNotContain {
		if strings.Contains(lower, strings.ToLower(forbid)) {
			return false, fmt.Sprintf("contains forbidden %q", forbid)
		}
	}
	return true, ""
}

// 1. Baseline: no_memory (empty context pack on every query).
func runNoMemory(sc Scenario) []QueryOutcome {
	var out []QueryOutcome
	for _, q := range sc.Queries {
		ok, reason := evaluateAssertions("", q)
		out = append(out, QueryOutcome{
			ScenarioID:  sc.ID,
			Category:    sc.Category,
			QueryID:     q.ID,
			Config:      ConfigNoMemory,
			Passed:      ok,
			TokensQuery: 0,
			DreamTokens: 0,
			ContextText: "",
			Failure:     reason,
		})
	}
	return out
}

// rawLogLine represents a raw event in the session log for full_context and raw_history_search.
type rawLogLine struct {
	Scope  reverie.Scope
	TaskID string
	At     time.Time
	Text   string
}

func flattenRawLog(sc Scenario) []rawLogLine {
	var lines []rawLogLine
	for _, sess := range sc.Sessions {
		baseAt, _ := time.Parse(time.RFC3339, sess.At)
		if baseAt.IsZero() {
			baseAt = time.Now().UTC()
		}
		repeat := 1
		if sess.FillerCount > 1 {
			repeat = sess.FillerCount
		}
		for r := 0; r < repeat; r++ {
			tid := sess.TaskID
			if repeat > 1 {
				tid = fmt.Sprintf("%s-%02d", sess.TaskID, r+1)
			}
			at := baseAt.Add(time.Duration(r) * time.Minute)
			for _, rem := range sess.Remember {
				lines = append(lines, rawLogLine{
					Scope:  sess.Scope,
					TaskID: tid,
					At:     at,
					Text:   fmt.Sprintf("[%s @%s] %s %s: %s", tid, at.Format(time.RFC3339), rem.Entity, rem.Relation, rem.Value),
				})
			}
			for _, ev := range sess.Events {
				body := ev.Text
				if body == "" && ev.Summary != "" {
					body = fmt.Sprintf("outcome=%s summary=%s", ev.Outcome, ev.Summary)
				}
				if repeat > 1 {
					body = fmt.Sprintf("%s (iteration %d: routine check passed across 40 packages and 120 files in module tree)", body, r+1)
				}
				lines = append(lines, rawLogLine{
					Scope:  sess.Scope,
					TaskID: tid,
					At:     at,
					Text:   fmt.Sprintf("[%s @%s] %s: %s", tid, at.Format(time.RFC3339), ev.Kind, body),
				})
			}
		}
	}
	return lines
}

// 2. Baseline: full_context (concatenates all in-scope raw history, newest-last,
// truncated to the query's token budget from the tail so recent filler crowds
// out old context, and old contradicted/expired statements remain in the log).
func runFullContext(sc Scenario) []QueryOutcome {
	lines := flattenRawLog(sc)
	var out []QueryOutcome
	for _, q := range sc.Queries {
		var inScope []string
		for _, l := range lines {
			if l.Scope == q.Scope {
				inScope = append(inScope, l.Text)
			}
		}
		// Keep newest lines that fit within 2x q.Budget (or full log if small),
		// modeling a context window where filler sessions crowd out older turns
		// while raw contradicted/stale statements remain un-superseded.
		budget := q.Budget
		if budget <= 0 {
			budget = 800
		}
		var kept []string
		used := 0
		for i := len(inScope) - 1; i >= 0; i-- {
			t := approxTokens(inScope[i]) + 2
			if used+t > budget && len(kept) > 0 {
				break
			}
			kept = append([]string{inScope[i]}, kept...)
			used += t
		}
		ctxText := strings.Join(kept, "\n")
		ok, reason := evaluateAssertions(ctxText, q)
		out = append(out, QueryOutcome{
			ScenarioID:  sc.ID,
			Category:    sc.Category,
			QueryID:     q.ID,
			Config:      ConfigFullContext,
			Passed:      ok,
			TokensQuery: approxTokens(ctxText),
			DreamTokens: 0,
			ContextText: ctxText,
			Failure:     reason,
		})
	}
	return out
}

var tokenWordRE = regexp.MustCompile(`[\p{L}\p{N}_]{3,}`)

// 3. Baseline: raw_history_search (an agent equipped with a keyword search tool
// over raw events in scope; returns top matching raw event lines within budget,
// without distillation, temporal normalization, ontology supersession, TTL expiry,
// or 2-hop entity graph traversal).
func runRawHistorySearch(sc Scenario) []QueryOutcome {
	lines := flattenRawLog(sc)
	var out []QueryOutcome
	for _, q := range sc.Queries {
		qWords := tokenWordRE.FindAllString(strings.ToLower(q.Task), -1)
		type scoredLine struct {
			text  string
			score int
			idx   int
		}
		var hits []scoredLine
		for idx, l := range lines {
			if l.Scope != q.Scope {
				continue
			}
			lower := strings.ToLower(l.Text)
			score := 0
			for _, w := range qWords {
				if strings.Contains(lower, w) {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, scoredLine{text: l.Text, score: score, idx: idx})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].idx > hits[j].idx
		})
		budget := q.Budget
		if budget <= 0 {
			budget = 800
		}
		var picked []string
		used := 0
		for _, h := range hits {
			t := approxTokens(h.text) + 2
			if used+t > budget && len(picked) > 0 {
				break
			}
			picked = append(picked, h.text)
			used += t
		}
		ctxText := strings.Join(picked, "\n")
		ok, reason := evaluateAssertions(ctxText, q)
		out = append(out, QueryOutcome{
			ScenarioID:  sc.ID,
			Category:    sc.Category,
			QueryID:     q.ID,
			Config:      ConfigRawHistorySearch,
			Passed:      ok,
			TokensQuery: approxTokens(ctxText),
			DreamTokens: 0,
			ContextText: ctxText,
			Failure:     reason,
		})
	}
	return out
}

// 4. Reverie: full engine (Record, Remember, Pin, Dream with Replay -> Distill ->
// ReconcileReflect -> Forget -> Validate, and Recall with hybrid FTS + 2-hop entity
// expansion + TTL filter + U-shaped assembly).
func runReverie(ctx context.Context, tempDir string, sc Scenario) ([]QueryOutcome, error) {
	var rules []llm.Rule
	for _, sess := range sc.Sessions {
		if len(sess.DreamFacts) == 0 {
			continue
		}
		var factsJSON []map[string]any
		for _, df := range sess.DreamFacts {
			conf := df.Confidence
			if conf <= 0 {
				conf = 0.85
			}
			factsJSON = append(factsJSON, map[string]any{
				"entity":     df.Entity,
				"relation":   df.Relation,
				"value":      df.Value,
				"confidence": conf,
			})
		}
		rules = append(rules, llm.Rule{
			Match:  sess.TaskID,
			Output: map[string]any{"facts": factsJSON},
			Tokens: 90,
		})
	}
	fakeLLM := &llm.Fake{Rules: rules}

	curNow := time.Now().UTC()
	dbPath := filepath.Join(tempDir, fmt.Sprintf("eval_%s.db", sc.ID))
	eng, err := engine.Open(dbPath, engine.WithLLM(fakeLLM), engine.WithClock(func() time.Time { return curNow }))
	if err != nil {
		return nil, err
	}
	defer eng.Close()

	totalDreamTokens := 0
	for _, sess := range sc.Sessions {
		sessAt, err := time.Parse(time.RFC3339, sess.At)
		if err != nil || sessAt.IsZero() {
			sessAt = time.Now().UTC()
		}
		curNow = sessAt
		repeat := 1
		if sess.FillerCount > 1 {
			repeat = sess.FillerCount
		}
		for r := 0; r < repeat; r++ {
			tid := sess.TaskID
			if repeat > 1 {
				tid = fmt.Sprintf("%s-%02d", sess.TaskID, r+1)
			}
			at := sessAt.Add(time.Duration(r) * time.Minute)
			curNow = at.Add(time.Minute)
			var evs []reverie.Event
			for i, se := range sess.Events {
				var payload json.RawMessage
				if se.Kind == string(reverie.TaskEnded) {
					b, _ := json.Marshal(map[string]string{
						"outcome": se.Outcome,
						"summary": se.Summary,
					})
					payload = b
				} else {
					b, _ := json.Marshal(map[string]string{"text": se.Text})
					payload = b
				}
				ev := reverie.NewEvent(sess.Scope, tid, reverie.EventKind(se.Kind), reverie.Origin(se.Origin), payload)
				ev.At = at.Add(time.Duration(i) * time.Second)
				evs = append(evs, ev)
			}
			if len(evs) > 0 {
				if err := eng.Record(ctx, evs...); err != nil {
					return nil, fmt.Errorf("session %s Record: %w", tid, err)
				}
			}
		}

		for _, rem := range sess.Remember {
			src := reverie.FactSource(rem.Source)
			if src == "" {
				src = reverie.SourceUser
			}
			if _, err := eng.Remember(ctx, sess.Scope, reverie.FactInput{
				Entity:   rem.Entity,
				Relation: rem.Relation,
				Value:    rem.Value,
				Source:   src,
				TaskID:   sess.TaskID,
			}); err != nil {
				return nil, fmt.Errorf("session %s Remember: %w", sess.TaskID, err)
			}
		}

		if sess.Dream {
			rep, err := dream.Run(ctx, eng.Store().For(sess.Scope), dream.Config{
				Trigger: "manual",
				Budget:  4000,
				Now:     sessAt.Add(30 * time.Second),
				LLM:     fakeLLM,
			})
			if err != nil {
				return nil, fmt.Errorf("session %s Dream: %w", sess.TaskID, err)
			}
			totalDreamTokens += rep.TokensSpent
		}

		if len(sess.PinValues) > 0 {
			fs, err := eng.Facts(ctx, sess.Scope, store.FactQuery{Limit: 100}, time.Time{})
			if err != nil {
				return nil, err
			}
			for _, pv := range sess.PinValues {
				for _, f := range fs {
					if strings.EqualFold(f.Value, pv) {
						if err := eng.Pin(ctx, sess.Scope, f.ID, true); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}

	var out []QueryOutcome
	for _, q := range sc.Queries {
		qAt, err := time.Parse(time.RFC3339, q.At)
		if err != nil || qAt.IsZero() {
			qAt = time.Now().UTC()
		}
		budget := q.Budget
		if budget <= 0 {
			budget = 800
		}
		pack, err := recall.Run(ctx, eng.Store().For(q.Scope), recall.Query{
			Task:   q.Task,
			Budget: budget,
			Now:    qAt,
		})
		if err != nil {
			return nil, fmt.Errorf("query %s Recall: %w", q.ID, err)
		}
		rendered := pack.Render()
		ok, reason := evaluateAssertions(rendered, q)
		out = append(out, QueryOutcome{
			ScenarioID:  sc.ID,
			Category:    sc.Category,
			QueryID:     q.ID,
			Config:      ConfigReverie,
			Passed:      ok,
			TokensQuery: pack.Tokens,
			DreamTokens: totalDreamTokens,
			ContextText: rendered,
			Failure:     reason,
		})
	}
	return out, nil
}

// Summarize aggregates QueryOutcomes for a single ConfigMode.
func Summarize(mode ConfigMode, outcomes []QueryOutcome) ConfigSummary {
	sum := ConfigSummary{
		Config:     mode,
		ByCategory: map[string]CategoryStat{},
	}
	seenDreamScenario := map[string]bool{}
	totalQueryToks := 0
	for _, o := range outcomes {
		if o.Config != mode {
			continue
		}
		sum.Queries++
		if o.Passed {
			sum.Passed++
		}
		totalQueryToks += o.TokensQuery
		if !seenDreamScenario[o.ScenarioID] {
			seenDreamScenario[o.ScenarioID] = true
			sum.TotalDreamToks += o.DreamTokens
		}
		cs := sum.ByCategory[o.Category]
		cs.Category = o.Category
		cs.Total++
		if o.Passed {
			cs.Passed++
		}
		cs.Accuracy = float64(cs.Passed) / float64(cs.Total)
		sum.ByCategory[o.Category] = cs
	}
	if sum.Queries > 0 {
		sum.Accuracy = float64(sum.Passed) / float64(sum.Queries)
		sum.AvgQueryTokens = float64(totalQueryToks) / float64(sum.Queries)
	}
	return sum
}
