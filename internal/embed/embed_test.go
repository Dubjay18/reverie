package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func vec(first float32) []float32 {
	v := make([]float32, defaultDims)
	v[0] = first
	return v
}

func TestOpenAIEmbedReordersByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing auth header")
		}
		// Return out of order to prove we honour "index".
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": vec(2)},
			{"index": 0, "embedding": vec(1)},
		}})
	}))
	defer srv.Close()

	o := &OpenAI{APIKey: "k", BaseURL: srv.URL}
	got, err := o.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0][0] != 1 || got[1][0] != 2 {
		t.Fatalf("vectors not in input order: %v %v", got[0][0], got[1][0])
	}
}

func TestOpenAIEmbedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	o := &OpenAI{APIKey: "k", BaseURL: srv.URL}
	_, err := o.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
}
