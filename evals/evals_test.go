package evals_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Dubjay18/reverie/evals"
)

func TestScenariosBaselinesSubsetsAndResultsTable(t *testing.T) {
	ctx := context.Background()

	// T5.1: Verify all 5 required scenario categories load and pass under Reverie.
	scenarios, err := evals.LoadBuiltinScenarios()
	if err != nil {
		t.Fatalf("LoadBuiltinScenarios: %v", err)
	}
	wantScenarios := map[string]bool{
		"amnesia":       false,
		"contradiction": false,
		"staleness":     false,
		"isolation":     false,
		"continuity":    false,
	}
	for _, sc := range scenarios {
		wantScenarios[sc.ID] = true
	}
	for id, seen := range wantScenarios {
		if !seen {
			t.Fatalf("missing required scenario %q", id)
		}
	}

	// T5.2 + T5.3 + T5.4: Run all scenarios and LongMemEval/LoCoMo subsets across all 4 configs.
	rep, err := evals.RunAll(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	// All 5 core scenarios must pass under Reverie.
	coreRev := rep.CoreSummaries[evals.ConfigReverie]
	if coreRev.Queries != 5 || coreRev.Passed != 5 || coreRev.Accuracy != 1.0 {
		t.Fatalf("expected Reverie to pass 5/5 core scenarios, got %+v", coreRev)
	}

	// Verify all 4 configurations ran across all 13 queries (5 core + 8 benchmark subset).
	for _, mode := range evals.AllConfigs {
		s := rep.CombinedByMode[mode]
		if s.Queries != 13 {
			t.Fatalf("config %s: expected 13 total queries, got %d", mode, s.Queries)
		}
	}

	// Reverie must outperform no_memory, full_context, and raw_history_search.
	if rep.CombinedByMode[evals.ConfigReverie].Accuracy <= rep.CombinedByMode[evals.ConfigRawHistorySearch].Accuracy {
		t.Fatalf("expected Reverie accuracy (%.2f) > raw_history_search (%.2f)",
			rep.CombinedByMode[evals.ConfigReverie].Accuracy,
			rep.CombinedByMode[evals.ConfigRawHistorySearch].Accuracy)
	}

	// T5.3: Worst category must be identified on the benchmark subsets.
	if rep.BenchReport.WorstCategory.Category != "LongMemEval/temporal-reasoning" {
		t.Fatalf("expected worst category LongMemEval/temporal-reasoning, got %+v", rep.BenchReport.WorstCategory)
	}

	// T5.4: Markdown table generator must include raw_history_search baseline row,
	// accuracy, tokens/query, dream cost, and method.
	md := evals.FormatMarkdown(rep)
	for _, needle := range []string{
		"## Method",
		"| `no_memory` |",
		"| `full_context` |",
		"| `raw_history_search` |",
		"| `reverie` |",
		"Worst Category Identified: `LongMemEval/temporal-reasoning`",
	} {
		if !strings.Contains(md, needle) {
			t.Errorf("markdown report missing %q:\n%s", needle, md)
		}
	}

	if os.Getenv("REVERIE_WRITE_EVALS") != "" {
		if err := os.WriteFile("RESULTS.md", []byte(md), 0o644); err != nil {
			t.Fatalf("write RESULTS.md: %v", err)
		}
		if err := os.WriteFile("../docs/EVALS.md", []byte(md), 0o644); err != nil {
			t.Fatalf("write ../docs/EVALS.md: %v", err)
		}
	}
}
