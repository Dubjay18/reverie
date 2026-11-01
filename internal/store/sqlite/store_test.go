package sqlite

import (
	"errors"
	"path/filepath"
	"testing"

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
