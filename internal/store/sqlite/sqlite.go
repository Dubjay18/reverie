// Package sqlite is the default, embedded store.Store. Pure Go, no cgo.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Config describes the embedder this database is used with. The zero value
// means none: search is full-text only and no model is pinned.
type Config struct {
	EmbeddingModel string
	Dimensions     int
}

// Store keeps two pools: db serialises writers, rdb serves read-only views
// concurrently (WAL lets readers run beside the writer).
type Store struct {
	db, rdb *sql.DB
	dims    int // pinned embedding dimension, 0 when none
}

var _ store.Store = (*Store)(nil)

func Open(path string, cfg Config) (*Store, error) {
	// _txlock=immediate takes the write lock at BEGIN, so a second process
	// waits on busy_timeout instead of failing mid-transaction on upgrade.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; this avoids "database is locked"
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := checkMeta(db, cfg); err != nil {
		db.Close()
		return nil, err
	}
	rdb, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(4)
	return &Store{db: db, rdb: rdb, dims: cfg.Dimensions}, nil
}

func (s *Store) Close() error { return errors.Join(s.rdb.Close(), s.db.Close()) }

func (s *Store) For(sc reverie.Scope) store.Scoped { return &scoped{s: s, sc: sc} }

func (s *Store) Scopes(ctx context.Context) ([]reverie.Scope, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT tenant, agent, project, usr FROM events ORDER BY 1, 2, 3, 4`)
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

type scoped struct {
	s  *Store
	sc reverie.Scope
}

func (s *scoped) Tx(ctx context.Context, fn func(store.Tx) error) error {
	return s.run(ctx, s.s.db, fn)
}

func (s *scoped) View(ctx context.Context, fn func(store.Tx) error) error {
	return s.run(ctx, s.s.rdb, fn)
}

func (s *scoped) run(ctx context.Context, db *sql.DB, fn func(store.Tx) error) error {
	if err := s.sc.Validate(); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	if err := fn(&txn{tx: tx, sc: s.sc, dims: s.s.dims}); err != nil {
		return err
	}
	return tx.Commit()
}

type txn struct {
	tx   *sql.Tx
	sc   reverie.Scope
	dims int
}

func (t *txn) Events() store.EventRepo     { return eventRepo{t} }
func (t *txn) Episodes() store.EpisodeRepo { return episodeRepo{t} }
func (t *txn) Index() store.IndexRepo      { return indexRepo{t} }
func (t *txn) Facts() store.FactRepo       { return factRepo{t} }
func (t *txn) Journal() store.JournalRepo  { return journalRepo{t} }

// checkMeta pins the embedding model and dimension on first use and refuses a
// different one afterwards: mixing embedders silently corrupts similarity.
func checkMeta(db *sql.DB, cfg Config) error {
	if cfg.Dimensions == 0 && cfg.EmbeddingModel == "" {
		return nil
	}
	want := map[string]string{
		"embedding_model": cfg.EmbeddingModel,
		"embedding_dims":  fmt.Sprint(cfg.Dimensions),
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range want {
		var have string
		err := tx.QueryRow(`SELECT value FROM reverie_meta WHERE key = ?`, k).Scan(&have)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.Exec(`INSERT INTO reverie_meta (key, value) VALUES (?, ?)`, k, v); err != nil {
				return err
			}
		case err != nil:
			return err
		case have != v:
			return fmt.Errorf("%w: %s is %q, opened with %q", store.ErrEmbedderMismatch, k, have, v)
		}
	}
	return tx.Commit()
}
