package postgres

import (
	"context"
	"hash/fnv"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/Dubjay18/reverie"
)

// bagEmbedder hashes words into buckets, so texts sharing words are close.
type bagEmbedder struct{}

func (bagEmbedder) Dimensions() int { return 1536 }

func (bagEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 1536)
		for _, w := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			h.Write([]byte(w))
			v[h.Sum32()%1536]++
		}
		var norm float64
		for _, x := range v {
			norm += float64(x * x)
		}
		if norm > 0 {
			for j := range v {
				v[j] /= float32(math.Sqrt(norm))
			}
		}
		out[i] = v
	}
	return out, nil
}

func TestSearch(t *testing.T) {
	dsn := os.Getenv("REVERIE_TEST_DSN")
	if dsn == "" {
		t.Skip("REVERIE_TEST_DSN not set (needs Postgres with pgvector)")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn, bagEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := reverie.Scope{Tenant: "t-" + reverie.NewID().String(), Agent: "a", Project: "p", User: "u"}
	other := sc
	other.Tenant += "-other"

	err = s.Put(ctx,
		Item{ID: "1", Scope: sc, Kind: "fact", Text: "deploys run through the staging cluster first"},
		Item{ID: "2", Scope: sc, Kind: "fact", Text: "the user prefers tabs over spaces"},
		Item{ID: "3", Scope: sc, Kind: "episode", Text: "rolled back a failed staging deploy"},
		Item{ID: "x", Scope: other, Kind: "fact", Text: "staging deploy secrets live in vault"},
	)
	if err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search(ctx, sc, "staging deploy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range hits {
		if h.ID == "x" {
			t.Fatal("leaked an item from another scope")
		}
	}
	if top := hits[0].ID; top != "1" && top != "3" {
		t.Errorf("top hit = %q, want a staging/deploy item", top)
	}

	// Upsert replaces in place.
	if err := s.Put(ctx, Item{ID: "2", Scope: sc, Kind: "fact", Text: "the user prefers spaces"}); err != nil {
		t.Fatal(err)
	}
	hits, err = s.Search(ctx, sc, "tabs", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "2" && strings.Contains(h.Text, "tabs") {
			t.Error("upsert did not replace text")
		}
	}
}
