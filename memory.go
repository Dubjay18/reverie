package reverie

import (
	"context"
	"fmt"
	"strings"
)

// Memory is the contract agents program against.
type Memory interface {
	Record(ctx context.Context, events ...Event) error
	// Recall returns a pack whose rendered form fits budget tokens.
	Recall(ctx context.Context, s Scope, task string, budget int) (ContextPack, error)
	// Expand returns the raw events behind a pack item, oldest first. A
	// result at the engine's cap means "narrow the range and ask again".
	Expand(ctx context.Context, s Scope, ref Ref) ([]Event, error)
	// Remember states a fact now, without waiting for a dream. in.Source
	// says how far to trust it; adapters pass SourceUser only for words the
	// user typed themselves.
	Remember(ctx context.Context, s Scope, in FactInput) (Fact, error)
	// Forget tombstones one fact by id (the "fact:<id>" in a pack citation) and
	// returns the run id that Revert takes to undo it. by is who is asking: a
	// caller cannot forget a fact from a source more trusted than itself, so an
	// agent cannot erase what the user said.
	Forget(ctx context.Context, s Scope, factID string, by FactSource) (runID string, err error)
}

type ContextPack struct {
	Skills   []PackItem
	Facts    []PackItem
	Episodes []PackItem
	Insights []PackItem
	Tokens   int // tokens of Render(), as counted by the engine's tokenizer
	Budget   int
	Dropped  int
}

type PackItem struct {
	ID     string     // so the agent can cite or flag it
	Kind   string     // skill | fact | episode | insight
	Cite   string     // "ep:<task>#<from>..<to> @<time>"; the first word is a valid Ref
	Text   string     // already rendered, ready for the prompt
	Source FactSource // set for fact items so callers do not inspect Text
	Status FactStatus // set for fact items so callers do not inspect Text
	Score  float64    // why it ranked here
	Tokens int
	Hot    bool // recent and not yet consolidated
}

// Render returns a compact, labelled text block for use in a prompt. Sections
// run from most durable to most immediate, because models attend best to the
// start and end of a long context: pinned facts first, recent episodes and
// skills last, nearest the task.
func (p ContextPack) Render() string {
	var b strings.Builder
	section := func(title string, items []PackItem) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "- [%s] %s\n", it.label(), it.Text)
		}
	}
	section("Facts", p.Facts)
	section("Insights", p.Insights)
	section("Episodes", p.Episodes)
	section("Skills", p.Skills)
	return b.String()
}

func (it PackItem) label() string {
	if it.Cite != "" {
		return it.Cite
	}
	return it.ID
}

// SectionTitle and RenderLine let the assembler cost items exactly as Render prints them.
func SectionTitle(kind string) string {
	switch kind {
	case "fact":
		return "Facts"
	case "insight":
		return "Insights"
	case "skill":
		return "Skills"
	}
	return "Episodes"
}

func (it PackItem) RenderLine() string { return fmt.Sprintf("- [%s] %s\n", it.label(), it.Text) }
