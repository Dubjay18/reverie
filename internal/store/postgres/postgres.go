// Package postgres is the server-backed store.Store implementation using
// PostgreSQL, pgvector (when available), and tsvector for full-text search.
package postgres

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.ParseInLocation(timeLayout, s, time.UTC) }

// Config describes the embedder this database is used with. The zero value
// means none: search is full-text only and no model is pinned.
type Config struct {
	EmbeddingModel string
	Dimensions     int
}

type Store struct {
	pool *pgxpool.Pool
	dims int
}

var _ store.Store = (*Store)(nil)

func Open(ctx context.Context, dsn string, cfg Config) (*Store, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pcfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, dims: cfg.Dimensions}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.checkMeta(ctx, cfg); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

func (s *Store) For(sc reverie.Scope) store.Scoped { return &scoped{s: s, sc: sc} }

func (s *Store) Scopes(ctx context.Context) ([]reverie.Scope, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT tenant, agent, project, usr FROM events ORDER BY 1, 2, 3, 4`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reverie.Scope
	for rows.Next() {
		var sc reverie.Scope
		if err := rows.Scan(&sc.Tenant, &sc.Agent, &sc.Project, &sc.User); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

type txCtxKey struct{}

type scoped struct {
	s  *Store
	sc reverie.Scope
}

func (s *scoped) Tx(ctx context.Context, fn func(store.Tx) error) error {
	return s.run(ctx, false, fn)
}

func (s *scoped) View(ctx context.Context, fn func(store.Tx) error) error {
	return s.run(ctx, true, fn)
}

func (s *scoped) run(ctx context.Context, readOnly bool, fn func(store.Tx) error) error {
	if err := s.sc.Validate(); err != nil {
		return err
	}
	tx, err := s.s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(&txn{tx: tx, sc: s.sc, dims: s.s.dims, readOnly: readOnly}); err != nil {
		return err
	}
	if readOnly {
		return tx.Rollback(ctx)
	}
	return tx.Commit(ctx)
}

type txn struct {
	tx       pgx.Tx
	sc       reverie.Scope
	dims     int
	readOnly bool
}

var errReadOnly = errors.New("postgres: write on read-only view")

func (t *txn) Events() store.EventRepo     { return eventRepo{t} }
func (t *txn) Episodes() store.EpisodeRepo { return episodeRepo{t} }
func (t *txn) Index() store.IndexRepo      { return indexRepo{t} }
func (t *txn) Facts() store.FactRepo       { return factRepo{t} }
func (t *txn) Journal() store.JournalRepo  { return journalRepo{t} }
func (t *txn) Dreams() store.DreamRepo     { return dreamRepo{t} }

func (s *Store) migrate(ctx context.Context) error {
	for i, q := range []string{
		`CREATE TABLE IF NOT EXISTS reverie_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS events (
    id         TEXT NOT NULL,
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    origin     TEXT NOT NULL,
    at         TEXT NOT NULL,
    payload    BYTEA,
    token_cost INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, agent, project, usr, id)
)`,
		`CREATE INDEX IF NOT EXISTS events_scope_task ON events (tenant, agent, project, usr, task_id, id)`,
		`CREATE TABLE IF NOT EXISTS episodes (
    tenant       TEXT NOT NULL,
    agent        TEXT NOT NULL,
    project      TEXT NOT NULL,
    usr          TEXT NOT NULL,
    task_id      TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    first_event  TEXT NOT NULL,
    last_event   TEXT NOT NULL,
    started_at   TEXT NOT NULL,
    ended_at     TEXT NOT NULL,
    closed       INTEGER NOT NULL DEFAULT 0,
    outcome      TEXT NOT NULL DEFAULT '',
    events       INTEGER NOT NULL DEFAULT 0,
    tool_calls   INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0,
    feedback     INTEGER NOT NULL DEFAULT 0,
    implicit     INTEGER NOT NULL DEFAULT 0,
    importance   DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    tier         TEXT NOT NULL DEFAULT 'hot',
    consolidated INTEGER NOT NULL DEFAULT 0,
    goal         TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant, agent, project, usr, task_id)
)`,
		`CREATE INDEX IF NOT EXISTS episodes_scope_closed ON episodes (tenant, agent, project, usr, closed, ended_at)`,
		`CREATE TABLE IF NOT EXISTS items (
    id             BIGSERIAL,
    tenant         TEXT NOT NULL,
    agent          TEXT NOT NULL,
    project        TEXT NOT NULL,
    usr            TEXT NOT NULL,
    kind           TEXT NOT NULL,
    ref_id         TEXT NOT NULL,
    text           TEXT NOT NULL,
    embedding      BYTEA,
    embed_failures INTEGER NOT NULL DEFAULT 0,
    tsv            tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED,
    PRIMARY KEY (tenant, agent, project, usr, kind, ref_id)
)`,
		`CREATE INDEX IF NOT EXISTS items_tsv_idx ON items USING gin (tsv)`,
		`CREATE TABLE IF NOT EXISTS facts (
    tenant        TEXT NOT NULL,
    agent         TEXT NOT NULL,
    project       TEXT NOT NULL,
    usr           TEXT NOT NULL,
    id            TEXT NOT NULL,
    entity        TEXT NOT NULL,
    entity_type   TEXT NOT NULL DEFAULT '',
    relation      TEXT NOT NULL,
    value         TEXT NOT NULL,
    confidence    DOUBLE PRECISION NOT NULL,
    source        TEXT NOT NULL,
    status        TEXT NOT NULL,
    pinned        INTEGER NOT NULL DEFAULT 0,
    valid_from    TEXT NOT NULL,
    valid_to      TEXT NOT NULL DEFAULT '',
    recorded_at   TEXT NOT NULL,
    superseded_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant, agent, project, usr, id)
)`,
		`CREATE INDEX IF NOT EXISTS facts_key ON facts (tenant, agent, project, usr, entity, relation, status)`,
		`CREATE TABLE IF NOT EXISTS fact_sources (
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    fact_id    TEXT NOT NULL,
    episode_id TEXT NOT NULL,
    PRIMARY KEY (tenant, agent, project, usr, fact_id, episode_id)
)`,
		`CREATE TABLE IF NOT EXISTS journal (
    id              BIGSERIAL PRIMARY KEY,
    tenant          TEXT NOT NULL,
    agent           TEXT NOT NULL,
    project         TEXT NOT NULL,
    usr             TEXT NOT NULL,
    run_id          TEXT NOT NULL DEFAULT '',
    phase           TEXT NOT NULL DEFAULT '',
    op              TEXT NOT NULL,
    target_kind     TEXT NOT NULL,
    target_id       TEXT NOT NULL,
    "before"        BYTEA,
    "after"         BYTEA,
    rationale       TEXT NOT NULL DEFAULT '',
    source_episodes TEXT NOT NULL DEFAULT '[]',
    tokens          INTEGER NOT NULL DEFAULT 0,
    at              TEXT NOT NULL,
    reverted        INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE INDEX IF NOT EXISTS journal_scope_run ON journal (tenant, agent, project, usr, run_id, id)`,
		`CREATE TABLE IF NOT EXISTS dream_runs (
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    id         TEXT NOT NULL,
    trigger    TEXT NOT NULL,
    status     TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at   TEXT NOT NULL DEFAULT '',
    budget     INTEGER NOT NULL DEFAULT 0,
    spent      INTEGER NOT NULL DEFAULT 0,
    report     BYTEA,
    PRIMARY KEY (tenant, agent, project, usr, id)
)`,
		`CREATE TABLE IF NOT EXISTS dream_leases (
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    run_id     TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY (tenant, agent, project, usr)
)`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("postgres: migrate #%d: %w", i, err)
		}
	}
	return nil
}

func (s *Store) checkMeta(ctx context.Context, cfg Config) error {
	if cfg.Dimensions == 0 && cfg.EmbeddingModel == "" {
		return nil
	}
	want := map[string]string{
		"embedding_model": cfg.EmbeddingModel,
		"embedding_dims":  fmt.Sprint(cfg.Dimensions),
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for k, v := range want {
		var have string
		err := tx.QueryRow(ctx, `SELECT value FROM reverie_meta WHERE key = $1`, k).Scan(&have)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `INSERT INTO reverie_meta (key, value) VALUES ($1, $2)`, k, v); err != nil {
				return err
			}
		case err != nil:
			return err
		case have != v:
			return fmt.Errorf("%w: %s is %q, opened with %q", store.ErrEmbedderMismatch, k, have, v)
		}
	}
	return tx.Commit(ctx)
}

// --- EventRepo ---

type eventRepo struct{ t *txn }

func (r eventRepo) Append(ctx context.Context, ev reverie.Event) (bool, error) {
	if r.t.readOnly {
		return false, errReadOnly
	}
	if ev.Scope != r.t.sc {
		return false, store.ErrScopeMismatch
	}
	if err := ev.Validate(); err != nil {
		return false, err
	}
	sc := r.t.sc
	tag, err := r.t.tx.Exec(ctx, `INSERT INTO events
		(id, tenant, agent, project, usr, task_id, kind, origin, at, payload, token_cost)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant, agent, project, usr, id) DO NOTHING`,
		ev.ID.String(), sc.Tenant, sc.Agent, sc.Project, sc.User, ev.TaskID,
		string(ev.Kind), string(ev.Origin), fmtTime(ev.At), []byte(ev.Payload), ev.TokenCost)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r eventRepo) List(ctx context.Context, q store.EventQuery) ([]reverie.Event, error) {
	limit := limitOf(q.Limit)
	sc := r.t.sc
	query := `SELECT id, task_id, kind, origin, at, payload, token_cost FROM events
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4`
	args := []any{sc.Tenant, sc.Agent, sc.Project, sc.User}
	idx := 5
	if q.TaskID != "" {
		query += fmt.Sprintf(` AND task_id = $%d`, idx)
		args = append(args, q.TaskID)
		idx++
	}
	if q.After != (ulid.ULID{}) {
		query += fmt.Sprintf(` AND id > $%d`, idx)
		args = append(args, q.After.String())
		idx++
	}
	if q.From != (ulid.ULID{}) {
		query += fmt.Sprintf(` AND id >= $%d`, idx)
		args = append(args, q.From.String())
		idx++
	}
	if q.To != (ulid.ULID{}) {
		query += fmt.Sprintf(` AND id <= $%d`, idx)
		args = append(args, q.To.String())
		idx++
	}
	query += fmt.Sprintf(` ORDER BY id LIMIT $%d`, idx)
	args = append(args, limit)

	rows, err := r.t.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reverie.Event
	for rows.Next() {
		var (
			id, kind, origin, at string
			ev                   reverie.Event
			payload              []byte
		)
		if err := rows.Scan(&id, &ev.TaskID, &kind, &origin, &at, &payload, &ev.TokenCost); err != nil {
			return nil, err
		}
		if ev.ID, err = ulid.ParseStrict(id); err != nil {
			return nil, err
		}
		if ev.At, err = parseTime(at); err != nil {
			return nil, err
		}
		ev.Scope, ev.Kind, ev.Origin = sc, reverie.EventKind(kind), reverie.Origin(origin)
		if len(payload) > 0 {
			ev.Payload = json.RawMessage(append([]byte(nil), payload...))
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (r eventRepo) UpdatePayload(ctx context.Context, id ulid.ULID, payload json.RawMessage) error {
	sc := r.t.sc
	tag, err := r.t.tx.Exec(ctx, `UPDATE events SET payload = $1
		WHERE tenant = $2 AND agent = $3 AND project = $4 AND usr = $5 AND id = $6`,
		[]byte(payload), sc.Tenant, sc.Agent, sc.Project, sc.User, id.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// --- EpisodeRepo ---

type episodeRepo struct{ t *txn }

const episodeCols = `task_id, title, first_event, last_event, started_at, ended_at, closed, outcome,
	events, tool_calls, errors, feedback, implicit, importance, tier, consolidated, goal, last_error`

func (r episodeRepo) Get(ctx context.Context, taskID string) (store.Episode, error) {
	sc := r.t.sc
	row := r.t.tx.QueryRow(ctx, `SELECT `+episodeCols+` FROM episodes
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND task_id = $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, taskID)
	ep, err := scanEpisode(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Episode{}, store.ErrNotFound
	}
	return ep, err
}

func (r episodeRepo) Save(ctx context.Context, ep store.Episode) error {
	sc := r.t.sc
	_, err := r.t.tx.Exec(ctx, `INSERT INTO episodes
		(tenant, agent, project, usr, `+episodeCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		ON CONFLICT (tenant, agent, project, usr, task_id) DO UPDATE SET
			title = EXCLUDED.title, first_event = EXCLUDED.first_event, last_event = EXCLUDED.last_event,
			started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at, closed = EXCLUDED.closed,
			outcome = EXCLUDED.outcome, events = EXCLUDED.events, tool_calls = EXCLUDED.tool_calls,
			errors = EXCLUDED.errors, feedback = EXCLUDED.feedback, implicit = EXCLUDED.implicit,
			importance = EXCLUDED.importance, tier = EXCLUDED.tier, consolidated = EXCLUDED.consolidated,
			goal = EXCLUDED.goal, last_error = EXCLUDED.last_error`,
		sc.Tenant, sc.Agent, sc.Project, sc.User,
		ep.TaskID, ep.Title, ep.FirstEvent.String(), ep.LastEvent.String(),
		fmtTime(ep.StartedAt), fmtTime(ep.EndedAt), b2i(ep.Closed), string(ep.Outcome),
		ep.Events, ep.ToolCalls, ep.Errors, ep.Feedback, b2i(ep.Implicit),
		ep.Importance, string(ep.Tier), b2i(ep.Consolidated), ep.Goal, ep.LastError)
	return err
}

func (r episodeRepo) Open(ctx context.Context) ([]store.Episode, error) {
	return r.list(ctx, `closed = 0 ORDER BY ended_at`, 0)
}

func (r episodeRepo) Recent(ctx context.Context, limit int) ([]store.Episode, error) {
	return r.list(ctx, `closed = 1 ORDER BY ended_at DESC, task_id LIMIT $5`, limitOf(limit))
}

func (r episodeRepo) Unconsolidated(ctx context.Context, limit int) ([]store.Episode, error) {
	return r.list(ctx, `closed = 1 AND consolidated = 0 ORDER BY importance DESC, ended_at DESC, task_id LIMIT $5`, limitOf(limit))
}

func (r episodeRepo) list(ctx context.Context, tail string, limit int) ([]store.Episode, error) {
	sc := r.t.sc
	args := []any{sc.Tenant, sc.Agent, sc.Project, sc.User}
	if limit > 0 {
		args = append(args, limit)
	}
	rows, err := r.t.tx.Query(ctx, `SELECT `+episodeCols+` FROM episodes
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanEpisode(s scanner) (store.Episode, error) {
	var (
		ep                          store.Episode
		first, last, started, ended string
		closed, implicit, consol    int
		outcome, tier               string
	)
	if err := s.Scan(&ep.TaskID, &ep.Title, &first, &last, &started, &ended, &closed, &outcome,
		&ep.Events, &ep.ToolCalls, &ep.Errors, &ep.Feedback, &implicit, &ep.Importance, &tier, &consol, &ep.Goal, &ep.LastError); err != nil {
		return store.Episode{}, err
	}
	var err error
	if ep.FirstEvent, err = ulid.ParseStrict(first); err != nil {
		return ep, err
	}
	if ep.LastEvent, err = ulid.ParseStrict(last); err != nil {
		return ep, err
	}
	if ep.StartedAt, err = parseTime(started); err != nil {
		return ep, err
	}
	if ep.EndedAt, err = parseTime(ended); err != nil {
		return ep, err
	}
	ep.Closed, ep.Implicit, ep.Consolidated = closed != 0, implicit != 0, consol != 0
	ep.Outcome, ep.Tier = store.Outcome(outcome), store.Tier(tier)
	return ep, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- IndexRepo ---

type indexRepo struct{ t *txn }

func (r indexRepo) Upsert(ctx context.Context, it store.Item) error {
	sc := r.t.sc
	if _, err := r.t.tx.Exec(ctx, `INSERT INTO items (tenant, agent, project, usr, kind, ref_id, text, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)
		ON CONFLICT (tenant, agent, project, usr, kind, ref_id) DO UPDATE SET text = EXCLUDED.text, embedding = NULL, embed_failures = 0`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, it.Kind, it.RefID, it.Text); err != nil {
		return err
	}
	if len(it.Embedding) > 0 {
		_, err := r.SetEmbedding(ctx, it.Kind, it.RefID, it.Text, it.Embedding)
		return err
	}
	return nil
}

func (r indexRepo) Delete(ctx context.Context, kind, refID string) error {
	sc := r.t.sc
	_, err := r.t.tx.Exec(ctx, `DELETE FROM items WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND kind = $5 AND ref_id = $6`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, kind, refID)
	return err
}

func (r indexRepo) MarkEmbedFailed(ctx context.Context, kind, refID string) error {
	sc := r.t.sc
	_, err := r.t.tx.Exec(ctx, `UPDATE items SET embed_failures = embed_failures + 1
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND kind = $5 AND ref_id = $6`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, kind, refID)
	return err
}

func (r indexRepo) SetEmbedding(ctx context.Context, kind, refID, text string, vec []float32) (bool, error) {
	if r.t.dims > 0 && len(vec) != r.t.dims {
		return false, fmt.Errorf("postgres: vector has %d dimensions, database is pinned to %d", len(vec), r.t.dims)
	}
	sc := r.t.sc
	tag, err := r.t.tx.Exec(ctx, `UPDATE items SET embedding = $1
		WHERE tenant = $2 AND agent = $3 AND project = $4 AND usr = $5 AND kind = $6 AND ref_id = $7 AND text = $8`,
		encodeVec(normalized(vec)), sc.Tenant, sc.Agent, sc.Project, sc.User, kind, refID, text)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r indexRepo) Pending(ctx context.Context, limit int) ([]store.Item, error) {
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT kind, ref_id, text FROM items
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND embedding IS NULL AND embed_failures < $5
		ORDER BY id LIMIT $6`, sc.Tenant, sc.Agent, sc.Project, sc.User, store.MaxEmbedFailures, limitOf(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Item
	for rows.Next() {
		var it store.Item
		if err := rows.Scan(&it.Kind, &it.RefID, &it.Text); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

func tsQuery(q string) string {
	words := wordRE.FindAllString(q, 64)
	if len(words) == 0 {
		return ""
	}
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	return strings.Join(words, " | ")
}

func (r indexRepo) SearchText(ctx context.Context, query string, limit int) ([]store.Hit, error) {
	tsq := tsQuery(query)
	if tsq == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT kind, ref_id FROM items
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND tsv @@ to_tsquery('simple', $5)
		ORDER BY ts_rank_cd(tsv, to_tsquery('simple', $5)) DESC, ref_id LIMIT $6`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, tsq, min(limit, store.MaxLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Hit
	for rows.Next() {
		var h store.Hit
		if err := rows.Scan(&h.Kind, &h.RefID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (r indexRepo) SearchVector(ctx context.Context, vec []float32, limit int) ([]store.Hit, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	q := normalized(vec)
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT kind, ref_id, embedding FROM items
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND embedding IS NOT NULL`,
		sc.Tenant, sc.Agent, sc.Project, sc.User)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		h store.Hit
		s float32
	}
	var all []scored
	for rows.Next() {
		var (
			h    store.Hit
			blob []byte
		)
		if err := rows.Scan(&h.Kind, &h.RefID, &blob); err != nil {
			return nil, err
		}
		if len(blob) != 4*len(q) {
			continue
		}
		all = append(all, scored{h, dotBlob(q, blob)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		return all[i].h.RefID < all[j].h.RefID
	})
	limit = min(limit, len(all))
	out := make([]store.Hit, limit)
	for i := range out {
		out[i] = all[i].h
	}
	return out, nil
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func dotBlob(q []float32, b []byte) (s float32) {
	for i, f := range q {
		s += f * math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return s
}

func normalized(v []float32) []float32 {
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(n))
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = f * inv
	}
	return out
}

// --- FactRepo ---

type factRepo struct{ t *txn }

const factCols = `id, entity, entity_type, relation, value, confidence, source, status, pinned, valid_from, valid_to, recorded_at, superseded_by`
const liveClause = `status IN ('active', 'contested')`

func (r factRepo) Save(ctx context.Context, f reverie.Fact) error {
	sc := r.t.sc
	validTo := ""
	if !f.ValidTo.IsZero() {
		validTo = fmtTime(f.ValidTo)
	}
	if _, err := r.t.tx.Exec(ctx, `INSERT INTO facts
		(tenant, agent, project, usr, `+factCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (tenant, agent, project, usr, id) DO UPDATE SET
			entity = EXCLUDED.entity, entity_type = EXCLUDED.entity_type, relation = EXCLUDED.relation,
			value = EXCLUDED.value, confidence = EXCLUDED.confidence, source = EXCLUDED.source,
			status = EXCLUDED.status, pinned = EXCLUDED.pinned, valid_from = EXCLUDED.valid_from,
			valid_to = EXCLUDED.valid_to, recorded_at = EXCLUDED.recorded_at, superseded_by = EXCLUDED.superseded_by`,
		sc.Tenant, sc.Agent, sc.Project, sc.User,
		f.ID, f.Entity, f.EntityType, f.Relation, f.Value, f.Confidence,
		string(f.Source), string(f.Status), b2i(f.Pinned),
		fmtTime(f.ValidFrom), validTo, fmtTime(f.RecordedAt), f.SupersededBy); err != nil {
		return err
	}
	if _, err := r.t.tx.Exec(ctx, `DELETE FROM fact_sources
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND fact_id = $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, f.ID); err != nil {
		return err
	}
	for _, ep := range f.EpisodeIDs {
		if _, err := r.t.tx.Exec(ctx, `INSERT INTO fact_sources
			(tenant, agent, project, usr, fact_id, episode_id) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING`,
			sc.Tenant, sc.Agent, sc.Project, sc.User, f.ID, ep); err != nil {
			return err
		}
	}
	return nil
}

func (r factRepo) Get(ctx context.Context, id string) (reverie.Fact, error) {
	fs, err := r.query(ctx, `id = $5`, []any{id}, ``, 1)
	if err != nil {
		return reverie.Fact{}, err
	}
	if len(fs) == 0 {
		return reverie.Fact{}, store.ErrNotFound
	}
	return fs[0], nil
}

func (r factRepo) Live(ctx context.Context, entity, relation string) ([]reverie.Fact, error) {
	return r.query(ctx, `entity = $5 AND relation = $6 AND `+liveClause,
		[]any{strings.ToLower(strings.TrimSpace(entity)), strings.ToLower(strings.TrimSpace(relation))},
		`ORDER BY valid_from, id`, store.MaxLimit)
}

func (r factRepo) Query(ctx context.Context, q store.FactQuery) ([]reverie.Fact, error) {
	where, args := filters(q, 5)
	return r.query(ctx, liveClause+where, args, `ORDER BY confidence DESC, recorded_at DESC, id`, limitOf(q.Limit))
}

func (r factRepo) AsOf(ctx context.Context, q store.FactQuery, t time.Time) ([]reverie.Fact, error) {
	where, args := filters(q, 7)
	at := fmtTime(t)
	return r.query(ctx, `status != 'tombstoned' AND valid_from <= $5 AND (valid_to = '' OR valid_to > $6)`+where,
		append([]any{at, at}, args...), `ORDER BY valid_from, id`, limitOf(q.Limit))
}

func (r factRepo) Related(ctx context.Context, names []string, limit int) ([]reverie.Fact, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > 20 {
		names = names[:20]
	}
	var m1, m2 []string
	args := make([]any, 0, 2*len(names))
	lowered := make([]any, len(names))
	for i, n := range names {
		lowered[i] = strings.ToLower(strings.TrimSpace(n))
		m1 = append(m1, fmt.Sprintf("$%d", 5+i))
		m2 = append(m2, fmt.Sprintf("$%d", 5+len(names)+i))
	}
	args = append(args, lowered...)
	args = append(args, lowered...)
	return r.query(ctx, liveClause+` AND (entity IN (`+strings.Join(m1, ", ")+`) OR lower(value) IN (`+strings.Join(m2, ", ")+`))`, args,
		`ORDER BY confidence DESC, recorded_at DESC, id`, limitOf(limit))
}

func filters(q store.FactQuery, startIdx int) (string, []any) {
	var where string
	var args []any
	idx := startIdx
	if q.Entity != "" {
		where += fmt.Sprintf(` AND entity = $%d`, idx)
		args = append(args, strings.ToLower(strings.TrimSpace(q.Entity)))
		idx++
	}
	if q.Relation != "" {
		where += fmt.Sprintf(` AND relation = $%d`, idx)
		args = append(args, strings.ToLower(strings.TrimSpace(q.Relation)))
		idx++
	}
	if q.OnlyContested {
		where += ` AND status = 'contested'`
	}
	if q.OnlyPinned {
		where += ` AND pinned = 1`
	}
	return where, args
}

func limitOf(n int) int {
	if n <= 0 {
		return store.DefaultLimit
	}
	return min(n, store.MaxLimit)
}

func (r factRepo) query(ctx context.Context, where string, args []any, order string, limit int) ([]reverie.Fact, error) {
	sc := r.t.sc
	full := append([]any{sc.Tenant, sc.Agent, sc.Project, sc.User}, args...)
	limitPlaceholder := fmt.Sprintf("$%d", len(full)+1)
	full = append(full, limit)
	rows, err := r.t.tx.Query(ctx, `SELECT `+factCols+` FROM facts
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND `+where+` `+order+` LIMIT `+limitPlaceholder, full...)
	if err != nil {
		return nil, err
	}
	var out []reverie.Fact
	for rows.Next() {
		var (
			f                           reverie.Fact
			source, status, vf, vt, rec string
			pinned                      int
		)
		if err := rows.Scan(&f.ID, &f.Entity, &f.EntityType, &f.Relation, &f.Value, &f.Confidence, &source, &status,
			&pinned, &vf, &vt, &rec, &f.SupersededBy); err != nil {
			rows.Close()
			return nil, err
		}
		f.Source, f.Status, f.Pinned = reverie.FactSource(source), reverie.FactStatus(status), pinned != 0
		if f.ValidFrom, err = parseTime(vf); err != nil {
			rows.Close()
			return nil, err
		}
		if vt != "" {
			if f.ValidTo, err = parseTime(vt); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if f.RecordedAt, err = parseTime(rec); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		eps, err := r.sources(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].EpisodeIDs = eps
	}
	return out, nil
}

func (r factRepo) sources(ctx context.Context, factID string) ([]string, error) {
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT episode_id FROM fact_sources
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND fact_id = $5 ORDER BY episode_id`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, factID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- JournalRepo ---

type journalRepo struct{ t *txn }

const journalCols = `id, run_id, phase, op, target_kind, target_id, "before", "after", rationale, source_episodes, tokens, at, reverted`

func (r journalRepo) Append(ctx context.Context, e store.JournalEntry) (int64, error) {
	sc := r.t.sc
	eps, _ := json.Marshal(e.SourceEpisodes)
	if e.SourceEpisodes == nil {
		eps = []byte("[]")
	}
	var id int64
	err := r.t.tx.QueryRow(ctx, `INSERT INTO journal
		(tenant, agent, project, usr, run_id, phase, op, target_kind, target_id, "before", "after", rationale, source_episodes, tokens, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, e.RunID, e.Phase, e.Op, e.TargetKind, e.TargetID,
		[]byte(e.Before), []byte(e.After), e.Rationale, string(eps), e.Tokens, fmtTime(e.At)).Scan(&id)
	return id, err
}

func (r journalRepo) Get(ctx context.Context, id int64) (store.JournalEntry, error) {
	sc := r.t.sc
	row := r.t.tx.QueryRow(ctx, `SELECT `+journalCols+` FROM journal
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND id = $5`, sc.Tenant, sc.Agent, sc.Project, sc.User, id)
	e, err := scanJournal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.JournalEntry{}, store.ErrNotFound
	}
	return e, err
}

func (r journalRepo) ByRun(ctx context.Context, runID string) ([]store.JournalEntry, error) {
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT `+journalCols+` FROM journal
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND run_id = $5 ORDER BY id LIMIT $6`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, runID, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JournalEntry
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r journalRepo) Recent(ctx context.Context, limit int) ([]store.JournalEntry, error) {
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT `+journalCols+` FROM journal
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 ORDER BY id DESC LIMIT $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, limitOf(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JournalEntry
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r journalRepo) MarkReverted(ctx context.Context, id int64) error {
	sc := r.t.sc
	tag, err := r.t.tx.Exec(ctx, `UPDATE journal SET reverted = 1
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND id = $5`, sc.Tenant, sc.Agent, sc.Project, sc.User, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanJournal(s scanner) (store.JournalEntry, error) {
	var (
		e        store.JournalEntry
		before   []byte
		after    []byte
		eps, at  string
		reverted int
	)
	if err := s.Scan(&e.ID, &e.RunID, &e.Phase, &e.Op, &e.TargetKind, &e.TargetID, &before, &after, &e.Rationale, &eps, &e.Tokens, &at, &reverted); err != nil {
		return e, err
	}
	if len(before) > 0 {
		e.Before = json.RawMessage(append([]byte(nil), before...))
	}
	if len(after) > 0 {
		e.After = json.RawMessage(append([]byte(nil), after...))
	}
	_ = json.Unmarshal([]byte(eps), &e.SourceEpisodes)
	var err error
	if e.At, err = parseTime(at); err != nil {
		return e, err
	}
	e.Reverted = reverted != 0
	return e, nil
}

// --- DreamRepo ---

type dreamRepo struct{ t *txn }

func (r dreamRepo) Acquire(ctx context.Context, runID string, ttl time.Duration, now time.Time) (bool, error) {
	sc := r.t.sc
	var holder, exp string
	err := r.t.tx.QueryRow(ctx, `SELECT run_id, expires_at FROM dream_leases
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4`,
		sc.Tenant, sc.Agent, sc.Project, sc.User).Scan(&holder, &exp)
	switch {
	case err == nil:
		expAt, err := parseTime(exp)
		if err != nil {
			return false, err
		}
		if holder != runID && expAt.After(now) {
			return false, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return false, err
	}
	if _, err := r.t.tx.Exec(ctx, `INSERT INTO dream_leases (tenant, agent, project, usr, run_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant, agent, project, usr) DO UPDATE SET run_id = EXCLUDED.run_id, expires_at = EXCLUDED.expires_at`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, runID, fmtTime(now.Add(ttl))); err != nil {
		return false, err
	}
	_, err = r.t.tx.Exec(ctx, `UPDATE dream_runs SET status = $1, ended_at = $2
		WHERE tenant = $3 AND agent = $4 AND project = $5 AND usr = $6 AND status = $7 AND id != $8`,
		store.RunAborted, fmtTime(now), sc.Tenant, sc.Agent, sc.Project, sc.User, store.RunRunning, runID)
	return err == nil, err
}

func (r dreamRepo) Renew(ctx context.Context, runID string, ttl time.Duration, now time.Time) (bool, error) {
	sc := r.t.sc
	tag, err := r.t.tx.Exec(ctx, `UPDATE dream_leases SET expires_at = $1
		WHERE tenant = $2 AND agent = $3 AND project = $4 AND usr = $5 AND run_id = $6`,
		fmtTime(now.Add(ttl)), sc.Tenant, sc.Agent, sc.Project, sc.User, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r dreamRepo) Release(ctx context.Context, runID string) error {
	sc := r.t.sc
	_, err := r.t.tx.Exec(ctx, `DELETE FROM dream_leases
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND run_id = $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, runID)
	return err
}

func (r dreamRepo) StartRun(ctx context.Context, d store.DreamRun) error {
	sc := r.t.sc
	_, err := r.t.tx.Exec(ctx, `INSERT INTO dream_runs
		(tenant, agent, project, usr, id, trigger, status, started_at, ended_at, budget, spent, report)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '', $9, $10, $11)`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, d.ID, d.Trigger, d.Status, fmtTime(d.StartedAt), d.Budget, d.Spent, []byte(d.Report))
	return err
}

func (r dreamRepo) FinishRun(ctx context.Context, d store.DreamRun) error {
	sc := r.t.sc
	tag, err := r.t.tx.Exec(ctx, `UPDATE dream_runs
		SET status = $1, ended_at = $2, spent = $3, report = $4
		WHERE tenant = $5 AND agent = $6 AND project = $7 AND usr = $8 AND id = $9`,
		d.Status, fmtTime(d.EndedAt), d.Spent, []byte(d.Report),
		sc.Tenant, sc.Agent, sc.Project, sc.User, d.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r dreamRepo) GetRun(ctx context.Context, id string) (store.DreamRun, error) {
	sc := r.t.sc
	row := r.t.tx.QueryRow(ctx, `SELECT id, trigger, status, started_at, ended_at, budget, spent, report
		FROM dream_runs WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND id = $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, id)
	d, err := scanDreamRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.DreamRun{}, store.ErrNotFound
	}
	return d, err
}

func (r dreamRepo) Runs(ctx context.Context, limit int) ([]store.DreamRun, error) {
	if limit <= 0 {
		limit = 50
	}
	sc := r.t.sc
	rows, err := r.t.tx.Query(ctx, `SELECT id, trigger, status, started_at, ended_at, budget, spent, report
		FROM dream_runs WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4
		ORDER BY started_at DESC, id DESC LIMIT $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, min(limit, store.MaxLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.DreamRun
	for rows.Next() {
		d, err := scanDreamRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDreamRun(s scanner) (store.DreamRun, error) {
	var (
		d              store.DreamRun
		started, ended string
		report         []byte
	)
	if err := s.Scan(&d.ID, &d.Trigger, &d.Status, &started, &ended, &d.Budget, &d.Spent, &report); err != nil {
		return store.DreamRun{}, err
	}
	var err error
	if d.StartedAt, err = parseTime(started); err != nil {
		return d, err
	}
	if ended != "" {
		if d.EndedAt, err = parseTime(ended); err != nil {
			return d, err
		}
	}
	if len(report) > 0 {
		d.Report = append(d.Report[:0], report...)
	}
	return d, nil
}
