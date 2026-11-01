package memstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/Dubjay18/reverie"
)

// Store is an in-memory reverie.Memory. Events are lost when the process exits.
type Store struct {
	mu     sync.RWMutex
	events []reverie.Event
}

var _ reverie.Memory = (*Store)(nil)

func New() *Store { return &Store{} }

func (m *Store) Record(ctx context.Context, events ...reverie.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, events...)
	return nil
}

func (m *Store) Expand(ctx context.Context, s reverie.Scope, ref reverie.Ref) ([]reverie.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []reverie.Event
	for _, e := range m.events {
		if e.Scope == s && e.TaskID == ref.TaskID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *Store) Recall(ctx context.Context, s reverie.Scope, task string, budget int) (reverie.ContextPack, error) {
	if err := ctx.Err(); err != nil {
		return reverie.ContextPack{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	pack := reverie.ContextPack{Budget: budget}
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
		pack.Episodes = append(pack.Episodes, reverie.PackItem{
			ID: e.ID.String(), Kind: "episode", Text: text, Tokens: tokens,
		})
	}
	return pack, nil
}
