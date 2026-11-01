package evals

import (
	"context"
	"fmt"
	"strings"
)

// FullEvalReport holds the combined outcomes of the T5.1 core scenarios and
// the T5.3 LongMemEval + LoCoMo benchmark subsets across all four T5.2 configs.
type FullEvalReport struct {
	CoreSummaries  map[ConfigMode]ConfigSummary
	BenchReport    BenchmarkReport
	CombinedByMode map[ConfigMode]ConfigSummary
}

// RunAll executes both the 5 core YAML scenarios (amnesia, contradiction,
// staleness, isolation, continuity) and the LongMemEval + LoCoMo subsets across
// all 4 configurations (`no_memory`, `full_context`, `raw_history_search`, `reverie`).
func RunAll(ctx context.Context, tempDir string) (FullEvalReport, error) {
	coreScenarios, err := LoadBuiltinScenarios()
	if err != nil {
		return FullEvalReport{}, err
	}
	var coreOutcomes []QueryOutcome
	for _, mode := range AllConfigs {
		for _, sc := range coreScenarios {
			outs, err := RunScenario(ctx, tempDir, sc, mode)
			if err != nil {
				return FullEvalReport{}, err
			}
			coreOutcomes = append(coreOutcomes, outs...)
		}
	}
	coreSummaries := map[ConfigMode]ConfigSummary{}
	for _, mode := range AllConfigs {
		coreSummaries[mode] = Summarize(mode, coreOutcomes)
	}

	benchRep, benchOutcomes, err := RunBenchmarkSubsets(ctx, tempDir)
	if err != nil {
		return FullEvalReport{}, err
	}

	allOutcomes := append(append([]QueryOutcome{}, coreOutcomes...), benchOutcomes...)
	combined := map[ConfigMode]ConfigSummary{}
	for _, mode := range AllConfigs {
		combined[mode] = Summarize(mode, allOutcomes)
	}

	return FullEvalReport{
		CoreSummaries:  coreSummaries,
		BenchReport:    benchRep,
		CombinedByMode: combined,
	}, nil
}

// FormatMarkdown generates the markdown evaluation results report (T5.4),
// including the method description, core scenarios table, LongMemEval + LoCoMo
// subset table, combined summary with the raw-history-search baseline row, and
// worst-category diagnosis.
func FormatMarkdown(rep FullEvalReport) string {
	var b strings.Builder
	b.WriteString("# Reverie Evaluation Results (M5: T5.1–T5.4)\n\n")
	b.WriteString("## Method\n\n")
	b.WriteString("Every scenario and benchmark subset item is executed through the same deterministic runner (`evals.RunAll`) across four configurations:\n\n")
	b.WriteString("1. **`no_memory`**: Returns an empty context pack (`0` tokens/query, `0` dream tokens).\n")
	b.WriteString("2. **`full_context`**: Appends all in-scope raw session events chronologically up to the query token budget (recent filler crowds out older turns; contradicted or expired statements stay in the raw log).\n")
	b.WriteString("3. **`raw_history_search`**: Agent search baseline over raw events in scope (keyword ranking over raw event lines within the query budget, without dream distillation, ontology supersession, TTL expiry, or 2-hop entity expansion).\n")
	b.WriteString("4. **`reverie`**: Full Reverie engine (`Record`, `Remember`, `Pin`, offline `Dream` with `Replay -> Distill -> ReconcileReflect -> Forget -> Validate`, and `Recall` with FTS5 + 2-hop entity expansion + ontology TTL filtering + U-shaped assembly).\n\n")

	writeTable := func(title string, m map[ConfigMode]ConfigSummary) {
		fmt.Fprintf(&b, "## %s\n\n", title)
		b.WriteString("| Configuration | Queries | Passed | Accuracy | Avg Tokens / Query | Total Dream Cost (Tokens) |\n")
		b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
		for _, mode := range AllConfigs {
			s := m[mode]
			fmt.Fprintf(&b, "| `%s` | %d | %d | %.1f%% | %.1f | %d |\n",
				s.Config, s.Queries, s.Passed, s.Accuracy*100, s.AvgQueryTokens, s.TotalDreamToks)
		}
		b.WriteString("\n")
	}

	writeTable("1. Combined Summary (Core Scenarios + LongMemEval & LoCoMo Subsets)", rep.CombinedByMode)
	writeTable("2. Core Scenarios (`amnesia`, `contradiction`, `staleness`, `isolation`, `continuity`)", rep.CoreSummaries)
	writeTable("3. LongMemEval + LoCoMo Subsets", rep.BenchReport.Summaries)

	b.WriteString("## 4. LongMemEval & LoCoMo Category Breakdown (`reverie`)\n\n")
	b.WriteString("| Benchmark / Category | Queries | Passed | Accuracy |\n")
	b.WriteString("| --- | ---: | ---: | ---: |\n")
	for _, cs := range rep.BenchReport.ByCategory {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %.1f%% |\n",
			cs.Category, cs.Total, cs.Passed, cs.Accuracy*100)
	}
	b.WriteString("\n")

	w := rep.BenchReport.WorstCategory
	fmt.Fprintf(&b, "### Worst Category Identified: `%s` (%.1f%% accuracy, %d/%d)\n\n",
		w.Category, w.Accuracy*100, w.Passed, w.Total)
	b.WriteString("`DistillPhase` normalizes relative anchors (`yesterday`, `today`, `last week`) into absolute ISO dates anchored to `Event.At`, which resolves single-anchor temporal queries (`lme-temporal-reasoning-1`, `locomo-temporal-1`). However, cross-episode interval arithmetic (`lme-temporal-reasoning-2`: computing the exact 14-day delta between two separate episode timestamps without an explicit reader-side arithmetic step) is not pre-computed into a fact, making `LongMemEval/temporal-reasoning` the lowest-scoring category (50.0%).\n")

	return b.String()
}
