package reverie

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type IMemory interface {
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

// Memory is an in-memory IMemory. Events are lost when the process exits.
type Memory struct {
	mu     sync.RWMutex
	events []Event
}

func NewMemory() IMemory {
	return &Memory{}
}

func (m *Memory) Record(ctx context.Context, events ...Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, events...)
	return nil
}


func (m *Memory) Recall(ctx context.Context, s Scope, task string, budget int) (ContextPack, error) {
	if err := ctx.Err(); err != nil {
		return ContextPack{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	pack := ContextPack{Budget: budget}
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if e.Scope != s || (task != "" && e.TaskID != task) {
			continue
		}
		text := fmt.Sprintf("%s %s", e.Kind, e.Payload)
		tokens := e.TokenCost
		if tokens == 0 {
			tokens = len(text)/4 + 1
		}
		if pack.Tokens+tokens > budget {
			pack.Dropped++
			continue
		}
		pack.Tokens += tokens
		pack.Episodes = append(pack.Episodes, PackItem{
			ID: e.ID.String(), Kind: "episode", Text: text, Tokens: tokens,
		})
	}
	return pack, nil
}
