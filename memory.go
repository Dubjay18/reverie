package reverie

import (
	"context"
	"fmt"
	"strings"
)

// Memory is the contract agents program against.
type Memory interface {
	Record(ctx context.Context, events ...Event) error
	Recall(ctx context.Context, s Scope, task string, budget int) (ContextPack, error)
}

type ContextPack struct {
	Skills   []PackItem
	Facts    []PackItem
	Episodes []PackItem
	Insights []PackItem
	Tokens   int
	Budget   int
	Dropped  int
}

type PackItem struct {
	ID     string  // so the agent can cite or flag it
	Kind   string  // skill | fact | episode | insight
	Text   string  // already rendered, ready for the prompt
	Score  float64 // why it ranked here
	Tokens int
}

// Render returns a compact, labelled text block for use in a prompt.
func (p ContextPack) Render() string {
	var b strings.Builder
	section := func(title string, items []PackItem) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "- [%s] %s\n", it.ID, it.Text)
		}
	}
	section("Skills", p.Skills)
	section("Facts", p.Facts)
	section("Episodes", p.Episodes)
	section("Insights", p.Insights)
	return b.String()
}
