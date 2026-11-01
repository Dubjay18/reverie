// Package llm defines the language-model contract used by dreaming phases,
// a deterministic Fake for golden tests, and an OpenAI-compatible HTTP client.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Request is a single structured-output prompt.
type Request struct {
	System string
	Prompt string
}

// Response carries the raw JSON returned by the model and the token cost.
type Response struct {
	JSON         json.RawMessage
	InputTokens  int
	OutputTokens int
	Tokens       int // InputTokens + OutputTokens
}

// LLM produces structured JSON for a prompt and reports token usage.
type LLM interface {
	CompleteJSON(ctx context.Context, req Request) (Response, error)
}

// Rule matches a prompt substring and returns a canned JSON payload.
type Rule struct {
	Match  string // empty matches anything
	Output any    // marshaled to JSON (or json.RawMessage / string directly)
	Tokens int    // default 100 when <= 0
	Err    error
}

// Fake is a deterministic, rule-driven LLM for golden tests.
type Fake struct {
	mu    sync.Mutex
	Rules []Rule
	Calls []Request
}

func (f *Fake) CompleteJSON(_ context.Context, req Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, req)
	haystack := req.System + "\n" + req.Prompt
	for _, r := range f.Rules {
		if r.Match != "" && !strings.Contains(haystack, r.Match) {
			continue
		}
		if r.Err != nil {
			return Response{}, r.Err
		}
		raw, err := toJSON(r.Output)
		if err != nil {
			return Response{}, err
		}
		tok := r.Tokens
		if tok <= 0 {
			tok = 100
		}
		return Response{JSON: raw, InputTokens: tok / 2, OutputTokens: tok - tok/2, Tokens: tok}, nil
	}
	return Response{JSON: json.RawMessage(`{"facts":[]}`), InputTokens: 10, OutputTokens: 10, Tokens: 20}, nil
}

func toJSON(v any) (json.RawMessage, error) {
	switch x := v.(type) {
	case nil:
		return json.RawMessage(`{"facts":[]}`), nil
	case json.RawMessage:
		return x, nil
	case string:
		return json.RawMessage(x), nil
	case []byte:
		return json.RawMessage(x), nil
	default:
		return json.Marshal(v)
	}
}

// OpenAI calls an OpenAI-compatible chat completions endpoint with JSON mode
// and reports usage tokens from the response.
type OpenAI struct {
	APIKey  string
	Model   string       // default gpt-4o-mini
	BaseURL string       // default https://api.openai.com/v1
	Client  *http.Client // default http.DefaultClient
}

const defaultChatModel = "gpt-4o-mini"

func (o *OpenAI) CompleteJSON(ctx context.Context, req Request) (Response, error) {
	if o.APIKey == "" {
		return Response{}, errors.New("llm: missing API key")
	}
	model, base, client := o.Model, o.BaseURL, o.Client
	if model == "" {
		model = defaultChatModel
	}
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if client == nil {
		client = http.DefaultClient
	}

	msgs := []map[string]string{}
	if req.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": req.Prompt})

	body, err := json.Marshal(map[string]any{
		"model":           model,
		"messages":        msgs,
		"temperature":     0,
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+o.APIKey)
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(hreq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("llm: openai returned %s: %s", resp.Status, raw)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, errors.New("llm: empty choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if !json.Valid([]byte(content)) {
		return Response{}, fmt.Errorf("llm: model returned non-JSON content: %q", content)
	}
	total := out.Usage.TotalTokens
	if total == 0 {
		total = out.Usage.PromptTokens + out.Usage.CompletionTokens
	}
	return Response{
		JSON:         json.RawMessage(content),
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
		Tokens:       total,
	}, nil
}
