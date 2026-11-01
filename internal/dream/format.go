package dream

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

// FormatSummary formats the one-line summary printed by `reverie dream`.
func FormatSummary(rep Report) string {
	return fmt.Sprintf("dream %s: consolidated %d episode(s), learned %d fact(s), superseded %d, merged %d, contested %d, rejected %d (tokens %d/%d)",
		rep.RunID, rep.Episodes, rep.Learned, rep.Superseded, rep.Merged, rep.Contested, rep.Rejected, rep.TokensSpent, rep.TokensBudget)
}

// FormatDiff renders a human-readable diff of a dream run and its journal entries.
func FormatDiff(run store.DreamRun, entries []store.JournalEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run:     %s\n", run.ID)
	fmt.Fprintf(&b, "trigger: %s\n", run.Trigger)
	fmt.Fprintf(&b, "status:  %s\n", run.Status)
	fmt.Fprintf(&b, "started: %s\n", run.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
	if !run.EndedAt.IsZero() {
		fmt.Fprintf(&b, "ended:   %s\n", run.EndedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	fmt.Fprintf(&b, "tokens:  %d / %d\n", run.Spent, run.Budget)
	if len(run.Report) > 0 {
		var rep Report
		if json.Unmarshal(run.Report, &rep) == nil {
			fmt.Fprintf(&b, "summary: learned %d facts, superseded %d, merged %d, contested %d, rejected %d (episodes %d)\n",
				rep.Learned, rep.Superseded, rep.Merged, rep.Contested, rep.Rejected, rep.Episodes)
		}
	}
	if len(entries) == 0 {
		b.WriteString("changes: (none)\n")
		return b.String()
	}
	b.WriteString("changes:\n")
	for _, e := range entries {
		if e.TargetKind == "episode" {
			rev := ""
			if e.Reverted {
				rev = " [reverted]"
			}
			fmt.Fprintf(&b, "  ~ [%s] episode:%s%s\n", e.Op, e.TargetID, rev)
			continue
		}
		var before, after reverie.Fact
		_ = json.Unmarshal(e.Before, &before)
		_ = json.Unmarshal(e.After, &after)
		rev := ""
		if e.Reverted {
			rev = " [reverted]"
		}
		switch {
		case before.ID == "" && after.ID != "":
			fmt.Fprintf(&b, "  + [%s] fact:%s %s %s = %q (%s, %.2f)%s\n",
				e.Op, after.ID, after.Entity, after.Relation, after.Value, after.Source, after.Confidence, rev)
		case before.ID != "" && after.ID != "":
			fmt.Fprintf(&b, "  ~ [%s] fact:%s %s %s: %q (%s) -> %q (%s)%s\n",
				e.Op, after.ID, after.Entity, after.Relation, before.Value, before.Status, after.Value, after.Status, rev)
		default:
			fmt.Fprintf(&b, "  - [%s] %s:%s%s\n", e.Op, e.TargetKind, e.TargetID, rev)
		}
	}
	return b.String()
}
