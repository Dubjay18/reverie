// Package embed turns text into vectors.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Embedder maps texts to fixed-length vectors. Vectors from different
// models are not comparable, so a store must stick to one Embedder.
type Embedder interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions is the length of every returned vector.
	Dimensions() int
}

// OpenAI embeds via the OpenAI embeddings API.
type OpenAI struct {
	APIKey  string
	Model   string       // default text-embedding-3-small
	BaseURL string       // default https://api.openai.com/v1
	Client  *http.Client // default http.DefaultClient
}

const (
	defaultModel = "text-embedding-3-small"
	defaultDims  = 1536
)

func (o *OpenAI) Dimensions() int { return defaultDims }

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	model, base, client := o.Model, o.BaseURL, o.Client
	if model == "" {
		model = defaultModel
	}
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if client == nil {
		client = http.DefaultClient
	}

	body, err := json.Marshal(map[string]any{"model": model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: openai returned %s: %s", resp.Status, raw)
	}

	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) || len(d.Embedding) != o.Dimensions() {
			return nil, fmt.Errorf("embed: bad vector (index %d, %d dims)", d.Index, len(d.Embedding))
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
