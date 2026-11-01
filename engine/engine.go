// Package engine is the public constructor for a reverie.Memory backed by a
// durable store. It lives outside the root package because the root defines
// the types that the store packages import.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/embed"
	"github.com/Dubjay18/reverie/internal/episodes"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/sqlite"
)

type Engine struct {
	st           store.Store
	now          func() time.Time
	emb          embed.Embedder
	tok          recall.Tokenizer
	embedTimeout time.Duration // bound on the per-query embedding call in Recall
	kick         chan struct{}
	stop         context.CancelFunc
	done         chan struct{}
}

type Option func(*Engine)

// WithEmbedder enables vector search. Episodes are embedded in the
// background after they close; until then they are found by full-text search.
func WithEmbedder(e embed.Embedder) Option { return func(g *Engine) { g.emb = e } }

// WithTokenizer replaces the approximate token counter used for budgets.
func WithTokenizer(t recall.Tokenizer) Option { return func(g *Engine) { g.tok = t } }

// Open returns a Memory backed by a SQLite file.
func Open(path string, opts ...Option) (*Engine, error) {
	probe := &Engine{}
	for _, o := range opts {
		o(probe)
	}
	cfg := sqlite.Config{}
	if probe.emb != nil {
		cfg.Dimensions = probe.emb.Dimensions()
		cfg.EmbeddingModel = "unknown"
		if n, ok := probe.emb.(interface{ ModelName() string }); ok {
			cfg.EmbeddingModel = n.ModelName()
		}
	}
	st, err := sqlite.Open(path, cfg)
	if err != nil {
		return nil, err
	}
	return New(st, opts...), nil
}

// New wraps an existing store. The engine takes ownership: Close closes it.
func New(st store.Store, opts ...Option) *Engine {
	e := &Engine{st: st, now: time.Now, embedTimeout: 2 * time.Second, kick: make(chan struct{}, 1), done: make(chan struct{})}
	for _, o := range opts {
		o(e)
	}
	if e.emb != nil {
		var ctx context.Context
		ctx, e.stop = context.WithCancel(context.Background())
		go e.embedLoop(ctx)
	} else {
		close(e.done)
	}
	return e
}

func (e *Engine) Close() error {
	if e.stop != nil {
		e.stop()
	}
	<-e.done
	return e.st.Close()
}

// Store exposes the store for tools that read it directly (CLI tail).
func (e *Engine) Store() store.Store { return e.st }

// MaxFutureSkew bounds how far ahead of the engine's clock an event may be
// stamped. A far-future time would keep its episode "recently active" forever.
const MaxFutureSkew = 5 * time.Minute

var ErrFutureEvent = errors.New("engine: event timestamp is too far in the future")

// Record validates every event first, so a bad event writes nothing, then
// writes each scope's events in one transaction. A repeated event ID is
// ignored. An event for a task that already ended is kept in the log (raw
// events are the truth) but does not change the closed episode; this is the
// normal case for async hooks. If a later scope fails, earlier scopes stay
// committed and the error says how many.
func (e *Engine) Record(ctx context.Context, events ...reverie.Event) error {
	limit := e.now().Add(MaxFutureSkew)
	for i, ev := range events {
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		if ev.At.After(limit) {
			return fmt.Errorf("event %d: %w", i, ErrFutureEvent)
		}
	}
	var order []reverie.Scope
	groups := map[reverie.Scope][]reverie.Event{}
	for _, ev := range events {
		// Secrets never reach the log, the search index or an Expand result.
		ev.Payload = guard.RedactBytes(ev.Payload)
		if _, ok := groups[ev.Scope]; !ok {
			order = append(order, ev.Scope)
		}
		groups[ev.Scope] = append(groups[ev.Scope], ev)
	}
	closedAny := false
	defer func() {
		if closedAny {
			e.wake()
		}
	}()
	for done, sc := range order {
		batch := groups[sc]
		err := e.st.For(sc).Tx(ctx, func(tx store.Tx) error {
			for _, ev := range batch {
				inserted, err := tx.Events().Append(ctx, ev)
				if err != nil {
					return err
				}
				if !inserted {
					continue
				}
				ep, err := episodes.Apply(ctx, tx, ev)
				if err != nil {
					if errors.Is(err, episodes.ErrTaskClosed) {
						continue
					}
					return err
				}
				if ep.Closed {
					if err := episodes.Index(ctx, tx, ep); err != nil {
						return err
					}
					closedAny = true
				}
			}
			return nil
		})
		if err != nil {
			if done > 0 {
				return fmt.Errorf("scope %d of %d failed, %d earlier scope(s) already committed: %w", done+1, len(order), done, err)
			}
			return err
		}
	}
	return nil
}

// Sweep closes episodes idle for longer than idle as abandoned, in every
// scope, and returns how many it closed.
func (e *Engine) Sweep(ctx context.Context, now time.Time, idle time.Duration) (int, error) {
	scopes, err := e.st.Scopes(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sc := range scopes {
		err := e.st.For(sc).Tx(ctx, func(tx store.Tx) error {
			closed, err := episodes.Sweep(ctx, tx, now, idle)
			total += len(closed)
			for _, ep := range closed {
				if err := episodes.Index(ctx, tx, ep); err != nil {
					return err
				}
			}
			return err
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Recall builds a cited pack that fits budget tokens. Search is full-text,
// plus vector similarity when an embedder is configured; if the embedder
// fails, recall degrades to full-text rather than failing.
func (e *Engine) Recall(ctx context.Context, s reverie.Scope, task string, budget int) (reverie.ContextPack, error) {
	q := recall.Query{Task: task, Budget: budget, Now: e.now(), Tokenizer: e.tok}
	if e.emb != nil && task != "" {
		// A slow or hung embedder must not stall recall (the MCP loop is serial).
		ectx, cancel := context.WithTimeout(ctx, e.embedTimeout)
		vecs, err := e.emb.Embed(ectx, []string{task})
		cancel()
		if err == nil && len(vecs) == 1 {
			q.Vector = vecs[0]
		}
	}
	return recall.Run(ctx, e.st.For(s), q)
}

// MaxExpand caps the events one Expand call returns.
const MaxExpand = 500

func (e *Engine) Expand(ctx context.Context, s reverie.Scope, ref reverie.Ref) ([]reverie.Event, error) {
	var out []reverie.Event
	err := e.st.For(s).View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Events().List(ctx, store.EventQuery{TaskID: ref.TaskID, From: ref.From, To: ref.To, Limit: MaxExpand})
		return err
	})
	return out, err
}

// EmbedPending embeds up to batch waiting items per scope and returns how many
// it stored. The background loop calls it; tests call it directly.
//
// If a batch fails, each text is retried alone. A text the embedder rejects
// (embed.ErrRejected) is counted against store.MaxEmbedFailures and parked
// after that, so one bad text cannot starve the queue. Any other failure is
// treated as an outage: the error is returned and nothing is marked.
func (e *Engine) EmbedPending(ctx context.Context, batch int) (int, error) {
	if e.emb == nil {
		return 0, nil
	}
	scopes, err := e.st.Scopes(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sc := range scopes {
		var pending []store.Item
		if err := e.st.For(sc).View(ctx, func(tx store.Tx) (err error) {
			pending, err = tx.Index().Pending(ctx, batch)
			return err
		}); err != nil {
			return total, err
		}
		if len(pending) == 0 {
			continue
		}
		vecs, failed, err := e.embedItems(ctx, pending) // network calls, outside any transaction
		if err != nil {
			return total, err
		}
		err = e.st.For(sc).Tx(ctx, func(tx store.Tx) error {
			for i, it := range pending {
				if vecs[i] == nil {
					if failed[i] {
						if err := tx.Index().MarkEmbedFailed(ctx, it.Kind, it.RefID); err != nil {
							return err
						}
					}
					continue
				}
				applied, err := tx.Index().SetEmbedding(ctx, it.Kind, it.RefID, it.Text, vecs[i])
				if err != nil {
					return err
				}
				if applied {
					total++
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// embedItems returns one vector per item (nil where embedding failed) and
// which items the embedder rejected. Only a rejection (embed.ErrRejected)
// counts against an item: any other failure is the embedder being down or
// misconfigured and clears up on its own, so it is returned as an error and
// nothing is marked.
func (e *Engine) embedItems(ctx context.Context, items []store.Item) (vecs [][]float32, rejected []bool, err error) {
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.Text
	}
	vecs = make([][]float32, len(items))
	rejected = make([]bool, len(items))

	if got, berr := e.emb.Embed(ctx, texts); berr == nil && len(got) == len(items) {
		copy(vecs, got)
		return vecs, rejected, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var outage error
	for i := range items {
		got, serr := e.emb.Embed(ctx, texts[i:i+1])
		switch {
		case serr == nil && len(got) == 1:
			vecs[i] = got[0]
		case errors.Is(serr, embed.ErrRejected):
			rejected[i] = true
		default:
			if serr == nil {
				serr = errors.New("engine: embedder returned a wrong number of vectors")
			}
			outage = serr
		}
	}
	if outage != nil {
		return nil, nil, outage
	}
	return vecs, rejected, nil
}

func (e *Engine) wake() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) embedLoop(ctx context.Context) {
	defer close(e.done)
	tick := time.NewTicker(30 * time.Second) // retry after failures, pick up backlog
	defer tick.Stop()
	for {
		for {
			n, err := e.EmbedPending(ctx, 32)
			if err != nil || n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
		case <-tick.C:
		}
	}
}
