package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := Open(filepath.Join(t.TempDir(), "r.db"), Config{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestEmbedderMismatchFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	open := func(cfg Config) error {
		s, err := Open(path, cfg)
		if err == nil {
			s.Close()
		}
		return err
	}
	if err := open(Config{EmbeddingModel: "text-embedding-3-small", Dimensions: 1536}); err != nil {
		t.Fatal(err)
	}
	if err := open(Config{EmbeddingModel: "text-embedding-3-small", Dimensions: 1536}); err != nil {
		t.Fatalf("same embedder must reopen: %v", err)
	}
	if err := open(Config{}); err != nil {
		t.Fatalf("no embedder must reopen (full-text only): %v", err)
	}
	if err := open(Config{EmbeddingModel: "text-embedding-3-small", Dimensions: 768}); !errors.Is(err, store.ErrEmbedderMismatch) {
		t.Fatalf("dimension change: got %v", err)
	}
	if err := open(Config{EmbeddingModel: "nomic-embed-text", Dimensions: 1536}); !errors.Is(err, store.ErrEmbedderMismatch) {
		t.Fatalf("model change: got %v", err)
	}
}

func TestPinnedDimensionRejectsWrongVector(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "r.db"), Config{EmbeddingModel: "m", Dimensions: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc := reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u"}
	err = s.For(sc).Tx(context.Background(), func(tx store.Tx) error {
		if err := tx.Index().Upsert(context.Background(), store.Item{Kind: "episode", RefID: "x", Text: "t"}); err != nil {
			return err
		}
		_, err := tx.Index().SetEmbedding(context.Background(), "episode", "x", "t", []float32{1, 2})
		return err
	})
	if err == nil {
		t.Fatal("a 2-d vector must be rejected by a 3-d database")
	}
}
