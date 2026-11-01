// Package postgres stores memory items in Postgres with pgvector for
// semantic search and tsvector for full-text search.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/embed"
	"github.com/Dubjay18/reverie/internal/rank"
)

// candidates is how many rows each retriever contributes to fusion.
const candidates = 50

type Item struct {
	ID    string
	Scope reverie.Scope
	Kind  string // skill | fact | episode | insight
	Text  string
}

type Hit struct {
	Item
	Score float64
}

type Store struct {
	pool *pgxpool.Pool
	emb  embed.Embedder
}

// Open connects, ensures the schema exists, and registers the vector type.
// The schema is sized to emb.Dimensions(); changing embedders needs a re-embed.
func Open(ctx context.Context, dsn string, emb embed.Embedder) (*Store, error) {
	// The extension must exist before pgx can look up the vector type on connect.
	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: create vector extension: %w", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error { return pgxvec.RegisterTypes(ctx, c) }
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, emb: emb}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS memory_items (
	id         text PRIMARY KEY,
	tenant     text NOT NULL,
	agent      text NOT NULL,
	project    text NOT NULL,
	user_id    text NOT NULL,
	kind       text NOT NULL,
	text       text NOT NULL,
	embedding  vector(%d) NOT NULL,
	tsv        tsvector GENERATED ALWAYS AS (to_tsvector('english', text)) STORED,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS memory_items_scope_idx ON memory_items (tenant, agent, project, user_id);
CREATE INDEX IF NOT EXISTS memory_items_tsv_idx ON memory_items USING gin (tsv);
CREATE INDEX IF NOT EXISTS memory_items_embedding_idx ON memory_items USING hnsw (embedding vector_cosine_ops);
`, s.emb.Dimensions()))
	return err
}

// Put embeds and upserts items by ID.
func (s *Store) Put(ctx context.Context, items ...Item) error {
	if len(items) == 0 {
		return nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.Text
	}
	vecs, err := s.emb.Embed(ctx, texts)
	if err != nil {
		return err
	}

	var b pgx.Batch
	for i, it := range items {
		b.Queue(`
INSERT INTO memory_items (id, tenant, agent, project, user_id, kind, text, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET kind = EXCLUDED.kind, text = EXCLUDED.text, embedding = EXCLUDED.embedding`,
			it.ID, it.Scope.Tenant, it.Scope.Agent, it.Scope.Project, it.Scope.User,
			it.Kind, it.Text, pgvector.NewVector(vecs[i]))
	}
	return s.pool.SendBatch(ctx, &b).Close()
}

// Search returns the top k items in scope for query, ranking by reciprocal
// rank fusion of vector similarity and full-text match.
func (s *Store) Search(ctx context.Context, sc reverie.Scope, query string, k int) ([]Hit, error) {
	vecs, err := s.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	const scope = `tenant = $1 AND agent = $2 AND project = $3 AND user_id = $4`
	byVec, err := s.query(ctx, sc, `SELECT id, kind, text FROM memory_items WHERE `+scope+`
		ORDER BY embedding <=> $5 LIMIT $6`, pgvector.NewVector(vecs[0]), candidates)
	if err != nil {
		return nil, err
	}
	byText, err := s.query(ctx, sc, `SELECT id, kind, text FROM memory_items
		WHERE `+scope+` AND tsv @@ websearch_to_tsquery('english', $5)
		ORDER BY ts_rank_cd(tsv, websearch_to_tsquery('english', $5)) DESC LIMIT $6`, query, candidates)
	if err != nil {
		return nil, err
	}

	items := map[string]Item{}
	ids := func(list []Item) []string {
		out := make([]string, len(list))
		for i, it := range list {
			items[it.ID], out[i] = it, it.ID
		}
		return out
	}
	fused := rank.RRF(ids(byVec), ids(byText))
	if len(fused) > k {
		fused = fused[:k]
	}
	hits := make([]Hit, len(fused))
	for i, f := range fused {
		hits[i] = Hit{Item: items[f.ID], Score: f.Score}
	}
	return hits, nil
}

func (s *Store) query(ctx context.Context, sc reverie.Scope, sql string, arg any, limit int) ([]Item, error) {
	rows, err := s.pool.Query(ctx, sql, sc.Tenant, sc.Agent, sc.Project, sc.User, arg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it := Item{Scope: sc}
		if err := rows.Scan(&it.ID, &it.Kind, &it.Text); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
