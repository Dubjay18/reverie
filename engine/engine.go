// Package engine is the public constructor for a reverie.Memory backed by a
// durable store. It lives outside the root package because the root defines
// the types that the store packages import.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/embed"
	"github.com/Dubjay18/reverie/internal/episodes"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/ontology"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/sqlite"
	"github.com/oklog/ulid/v2"
)

type Engine struct {
	st           store.Store
	now          func() time.Time
	emb          embed.Embedder
	tok          recall.Tokenizer
	embedTimeout time.Duration // bound on the per-query embedding call in Recall
	onto         *ontology.Ontology
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

// WithOntology replaces the built-in ontology (see internal/ontology).
func WithOntology(o *ontology.Ontology) Option { return func(g *Engine) { g.onto = o } }

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
	e := &Engine{st: st, now: time.Now, onto: ontology.Default(), embedTimeout: 2 * time.Second, kick: make(chan struct{}, 1), done: make(chan struct{})}
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
		ev.Payload = redactPayload(ev.Payload)
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
				if err := e.rememberFromEvent(ctx, tx, ev); err != nil {
					return err
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
	if ref.FactID != "" {
		// the events behind a fact: every task it came from, oldest first
		err := e.st.For(s).View(ctx, func(tx store.Tx) error {
			f, err := tx.Facts().Get(ctx, ref.FactID)
			if err != nil {
				return err
			}
			for _, task := range f.EpisodeIDs {
				evs, err := tx.Events().List(ctx, store.EventQuery{TaskID: task, Limit: MaxExpand})
				if err != nil {
					return err
				}
				out = append(out, evs...)
				if len(out) >= MaxExpand {
					out = out[:MaxExpand]
					break
				}
			}
			return nil
		})
		return out, err
	}
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
		vecs, failed, embErr := e.embedItems(ctx, pending) // network calls, outside any transaction
		if vecs == nil {
			return total, embErr // nothing usable
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
		if embErr != nil {
			return total, embErr // partial results are stored; report the outage
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

	got, berr := e.emb.Embed(ctx, texts)
	if berr == nil && len(got) == len(items) {
		copy(vecs, got)
		return vecs, rejected, nil
	}
	if berr == nil {
		berr = errors.New("engine: embedder returned a wrong number of vectors")
	}
	// Only a rejected input is worth retrying alone. Any other failure (a
	// timeout, a 429, a 5xx) means the embedder is struggling, and one request
	// per text would only add load: report it and try again on the next tick.
	if !errors.Is(berr, embed.ErrRejected) {
		return nil, nil, berr
	}
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
			// keep what already succeeded; the rest stay pending
			return vecs, rejected, serr
		}
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

// rememberFromEvent turns the user's own "remember ..." into a fact in the
// same transaction as the event, with no model call. Only a Feedback event
// with user_input origin counts: tool output and agent text cannot mint one.
//
// The payload is {"remember": "free text"}, optionally with "entity",
// "relation" and "value" to state a triple. Free text is filed as a note about
// the user, because turning a sentence into a triple needs a model and the
// write path has none.
func (e *Engine) rememberFromEvent(ctx context.Context, tx store.Tx, ev reverie.Event) error {
	if ev.Kind != reverie.Feedback || ev.Origin != reverie.OriginUser {
		return nil
	}
	var p struct {
		Remember string `json:"remember"`
		Entity   string `json:"entity"`
		Relation string `json:"relation"`
		Value    string `json:"value"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil || strings.TrimSpace(p.Remember) == "" {
		return nil
	}
	in := reverie.FactInput{Entity: p.Entity, Relation: p.Relation, Value: p.Value, Source: reverie.SourceUser, TaskID: ev.TaskID, ValidAt: ev.At}
	if in.Relation == "" || in.Value == "" || !e.onto.Has(strings.ToLower(strings.TrimSpace(in.Relation))) {
		in.Entity, in.Relation, in.Value = "", "", p.Remember
	}
	_, err := e.assertInput(ctx, tx, in, facts.NewRunID("remember"))
	return err
}

func (e *Engine) assertInput(ctx context.Context, tx store.Tx, in reverie.FactInput, runID string) (facts.Resolution, error) {
	f := reverie.Fact{
		ID: ulid.Make().String(), Entity: in.Entity, Relation: in.Relation, Value: in.Value,
		Source: in.Source, ValidFrom: in.ValidAt, EpisodeIDs: []string{in.TaskID},
	}
	if f.Entity == "" {
		f.Entity = "user"
	}
	if f.Relation == "" {
		f.Relation = "note"
	}
	if in.TaskID == "" {
		f.EpisodeIDs = []string{"manual"}
	}
	return facts.Remember(ctx, tx, e.onto, f, e.now(), runID)
}

// ErrRejected is returned by Remember when a more trusted fact already holds
// the place the new one would take.
var ErrRejected = errors.New("engine: not stored: a more trusted fact already holds that")

// Remember states a fact now. The caller chooses in.Source; an adapter that
// relays a user's own words passes SourceUser, one acting for an agent passes
// SourceAgent. A free-form value (no relation) is filed as a note about "user".
// The returned Fact is the one stored, or the one it merged into.
func (e *Engine) Remember(ctx context.Context, s reverie.Scope, in reverie.FactInput) (reverie.Fact, error) {
	// No default: whoever calls decides how far the fact is trusted, and
	// silently choosing "the user said it" would make it unbeatable.
	if in.Source != reverie.SourceUser && in.Source != reverie.SourceAgent {
		return reverie.Fact{}, fmt.Errorf("engine: Remember needs Source user_stated or agent_stated, got %q", in.Source)
	}
	if in.TaskID == "" {
		in.TaskID = "manual:" + e.now().UTC().Format("2006-01-02")
	}
	in.Value = guard.Redact(in.Value)
	var out reverie.Fact
	var rejected error
	err := e.st.For(s).Tx(ctx, func(tx store.Tx) error {
		// the provenance event: what was said, when, by whom
		payload, _ := json.Marshal(map[string]string{"remember": in.Value, "entity": in.Entity, "relation": in.Relation, "source": string(in.Source)})
		origin := reverie.OriginAgent
		if in.Source == reverie.SourceUser {
			origin = reverie.OriginUser
		}
		ev := reverie.NewEvent(s, in.TaskID, reverie.Feedback, origin, payload)
		ev.At = e.now().UTC()
		if _, err := tx.Events().Append(ctx, ev); err != nil {
			return err
		}
		if _, err := episodes.Apply(ctx, tx, ev); err != nil && !errors.Is(err, episodes.ErrTaskClosed) {
			return err
		}
		in.ValidAt = firstNonZero(in.ValidAt, ev.At)
		res, err := e.assertInput(ctx, tx, in, facts.NewRunID("remember"))
		if err != nil {
			return err
		}
		switch res.Op {
		case facts.OpReject:
			// Commit, so the attempt stays in the log and the journal; report
			// the refusal to the caller afterwards.
			rejected = fmt.Errorf("%w (%s)", ErrRejected, res.Reason)
			return nil
		case facts.OpMerge:
			out = res.Updates[0]
		default:
			out = res.Fact
		}
		return nil
	})
	if err == nil {
		err = rejected
	}
	return out, err
}

func firstNonZero(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}

// Forget tombstones one fact by id and returns the run id for Revert. by is
// the caller's trust: pass reverie.SourceUser for a person at the CLI and
// reverie.SourceAgent for an agent's tool call.
func (e *Engine) Forget(ctx context.Context, s reverie.Scope, factID string, by reverie.FactSource) (string, error) {
	run := facts.NewRunID("forget")
	err := e.st.For(s).Tx(ctx, func(tx store.Tx) error {
		return facts.Forget(ctx, tx, factID, by, e.now(), run)
	})
	if err != nil {
		return "", err
	}
	return run, nil
}

// Pin protects a fact from being changed or expired by a dream.
func (e *Engine) Pin(ctx context.Context, s reverie.Scope, factID string, pinned bool) error {
	return e.st.For(s).Tx(ctx, func(tx store.Tx) error {
		return facts.SetPinned(ctx, tx, factID, pinned, e.now(), facts.NewRunID("pin"))
	})
}

// Revert undoes everything journaled under runID and returns how many
// changes it undid.
func (e *Engine) Revert(ctx context.Context, s reverie.Scope, runID string) (n int, err error) {
	err = e.st.For(s).Tx(ctx, func(tx store.Tx) (err error) {
		n, err = facts.RevertRun(ctx, tx, runID, e.now())
		return err
	})
	return n, err
}

// Facts lists what is believed, newest and most confident first, optionally
// filtered. asOf, when non-zero, returns what was true at that time instead.
func (e *Engine) Facts(ctx context.Context, s reverie.Scope, q store.FactQuery, asOf time.Time) (out []reverie.Fact, err error) {
	err = e.st.For(s).View(ctx, func(tx store.Tx) (err error) {
		if asOf.IsZero() {
			out, err = tx.Facts().Query(ctx, q)
		} else {
			out, err = tx.Facts().AsOf(ctx, q, asOf)
		}
		return err
	})
	return out, err
}

// Journal lists the newest journal entries, newest first: every change to
// long-term memory with the run id to pass to Revert.
func (e *Engine) Journal(ctx context.Context, s reverie.Scope, limit int) (out []store.JournalEntry, err error) {
	err = e.st.For(s).View(ctx, func(tx store.Tx) (err error) {
		out, err = tx.Journal().Recent(ctx, limit)
		return err
	})
	return out, err
}

// redactPayload keeps secrets out of the log, the search index and Expand
// results. Redaction rewrites text, so the result is checked again: if it is
// no longer valid JSON or has outgrown the size cap, the payload is replaced
// by a marker rather than stored broken.
func redactPayload(p json.RawMessage) json.RawMessage {
	out := guard.RedactBytes(p)
	if len(out) == len(p) && string(out) == string(p) {
		return p
	}
	if !json.Valid(out) || len(out) > reverie.MaxPayload {
		return json.RawMessage(`{"redacted":"payload replaced: redaction could not keep it valid"}`)
	}
	return out
}

// ScrubReport counts what Scrub changed.
type ScrubReport struct{ Events, Episodes int }

// Scrub redacts secrets from data stored before redaction existed, or before
// a rule was added: event payloads, episode titles and gists, and the search
// text built from them. It is safe to run repeatedly.
func (e *Engine) Scrub(ctx context.Context) (ScrubReport, error) {
	var rep ScrubReport
	scopes, err := e.st.Scopes(ctx)
	if err != nil {
		return rep, err
	}
	for _, sc := range scopes {
		err := e.st.For(sc).Tx(ctx, func(tx store.Tx) error {
			dirty := map[string]bool{} // tasks whose events changed
			var after ulid.ULID
			for {
				page, err := tx.Events().List(ctx, store.EventQuery{After: after, Limit: store.MaxLimit})
				if err != nil {
					return err
				}
				for _, ev := range page {
					after = ev.ID
					if clean := redactPayload(ev.Payload); string(clean) != string(ev.Payload) {
						if err := tx.Events().UpdatePayload(ctx, ev.ID, clean); err != nil {
							return err
						}
						dirty[ev.TaskID] = true
						rep.Events++
					}
				}
				if len(page) < store.MaxLimit {
					break
				}
			}
			var eps []store.Episode
			open, err := tx.Episodes().Open(ctx)
			if err != nil {
				return err
			}
			closed, err := tx.Episodes().Recent(ctx, store.MaxLimit)
			if err != nil {
				return err
			}
			eps = append(open, closed...)
			for _, ep := range eps {
				changed := false
				for _, f := range []*string{&ep.Title, &ep.Goal, &ep.LastError} {
					if c := guard.Redact(*f); c != *f {
						*f, changed = c, true
					}
				}
				if changed {
					if err := tx.Episodes().Save(ctx, ep); err != nil {
						return err
					}
					rep.Episodes++
				}
				if (changed || dirty[ep.TaskID]) && ep.Closed {
					if err := episodes.Index(ctx, tx, ep); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}
