package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Dubjay18/reverie"
)

// BenchmarkItem is a single entry in the LongMemEval or LoCoMo JSON subset fixtures.
type BenchmarkItem struct {
	ID        string            `json:"id"`
	Benchmark string            `json:"benchmark"` // "LongMemEval" or "LoCoMo"
	Category  string            `json:"category"`
	Sessions  []ScenarioSession `json:"sessions"`
	Query     ScenarioQuery     `json:"query"`
}

// BenchmarkReport summarizes end-to-end execution of LongMemEval and LoCoMo subsets.
type BenchmarkReport struct {
	Summaries     map[ConfigMode]ConfigSummary
	ByCategory    []CategoryStat
	WorstCategory CategoryStat
}

// LoadBenchmarkSubsets loads both LongMemEval and LoCoMo subset fixtures and
// converts them into Scenarios that run through the unified runner.
func LoadBenchmarkSubsets() ([]Scenario, error) {
	files := []string{
		"testdata/longmemeval_subset.json",
		"testdata/locomo_subset.json",
	}
	var out []Scenario
	for _, path := range files {
		raw, err := embeddedFS.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var items []BenchmarkItem
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, it := range items {
			defScope := reverie.Scope{
				Tenant:  "bench",
				Agent:   "bench-agent",
				Project: it.Benchmark,
				User:    "bench-user",
			}
			sessions := make([]ScenarioSession, len(it.Sessions))
			for i, s := range it.Sessions {
				if s.Scope == (reverie.Scope{}) {
					s.Scope = defScope
				}
				sessions[i] = s
			}
			q := it.Query
			if q.ID == "" {
				q.ID = it.ID + "-q"
			}
			if q.Scope == (reverie.Scope{}) {
				q.Scope = defScope
			}
			out = append(out, Scenario{
				ID:          it.ID,
				Category:    it.Benchmark + "/" + it.Category,
				Description: fmt.Sprintf("%s (%s)", it.ID, it.Category),
				Sessions:    sessions,
				Queries:     []ScenarioQuery{q},
			})
		}
	}
	return out, nil
}

// RunBenchmarkSubsets runs all LongMemEval and LoCoMo subset items across all
// four configurations and identifies Reverie's worst-performing category (T5.3).
func RunBenchmarkSubsets(ctx context.Context, tempDir string) (BenchmarkReport, []QueryOutcome, error) {
	scenarios, err := LoadBenchmarkSubsets()
	if err != nil {
		return BenchmarkReport{}, nil, err
	}
	var allOutcomes []QueryOutcome
	for _, mode := range AllConfigs {
		for _, sc := range scenarios {
			outs, err := RunScenario(ctx, tempDir, sc, mode)
			if err != nil {
				return BenchmarkReport{}, nil, err
			}
			allOutcomes = append(allOutcomes, outs...)
		}
	}

	summaries := map[ConfigMode]ConfigSummary{}
	for _, mode := range AllConfigs {
		summaries[mode] = Summarize(mode, allOutcomes)
	}

	revSum := summaries[ConfigReverie]
	var cats []CategoryStat
	for _, cs := range revSum.ByCategory {
		cats = append(cats, cs)
	}
	sort.Slice(cats, func(i, j int) bool {
		if cats[i].Accuracy != cats[j].Accuracy {
			return cats[i].Accuracy < cats[j].Accuracy
		}
		return cats[i].Category < cats[j].Category
	})
	var worst CategoryStat
	if len(cats) > 0 {
		worst = cats[0]
	}
	return BenchmarkReport{
		Summaries:     summaries,
		ByCategory:    cats,
		WorstCategory: worst,
	}, allOutcomes, nil
}
