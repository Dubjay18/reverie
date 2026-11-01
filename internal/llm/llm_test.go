package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestFakeMatchesRulesInOrder(t *testing.T) {
	f := &Fake{Rules: []Rule{
		{Match: "London", Output: map[string]any{"city": "London"}, Tokens: 42},
		{Match: "", Output: `{"city":"default"}`, Tokens: 10},
	}}
	r1, err := f.CompleteJSON(context.Background(), Request{Prompt: "moved to London"})
	if err != nil || r1.Tokens != 42 || string(r1.JSON) != `{"city":"London"}` {
		t.Fatalf("got %+v %v", r1, err)
	}
	r2, err := f.CompleteJSON(context.Background(), Request{Prompt: "other"})
	if err != nil || r2.Tokens != 10 || string(r2.JSON) != `{"city":"default"}` {
		t.Fatalf("got %+v %v", r2, err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("want 2 recorded calls, got %d", len(f.Calls))
	}
}

func TestOpenAIClientStructuredOutputAndMetering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad auth header: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": `{"facts":[{"entity":"jay","relation":"uses","value":"Go"}]}`}},
			},
			"usage": map[string]int{"prompt_tokens": 30, "completion_tokens": 15, "total_tokens": 45},
		})
	}))
	defer srv.Close()

	client := &OpenAI{APIKey: "test-key", BaseURL: srv.URL, Client: srv.Client()}
	resp, err := client.CompleteJSON(context.Background(), Request{System: "extract JSON", Prompt: "I use Go"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InputTokens != 30 || resp.OutputTokens != 15 || resp.Tokens != 45 {
		t.Fatalf("unexpected token metering: %+v", resp)
	}
	if !json.Valid(resp.JSON) {
		t.Fatalf("invalid json: %s", resp.JSON)
	}
}

func TestOpenAILiveSmoke(t *testing.T) {
	key := os.Getenv("REVERIE_TEST_OPENAI_KEY")
	if key == "" {
		t.Skip("skipping live LLM test: REVERIE_TEST_OPENAI_KEY not set")
	}
	client := &OpenAI{APIKey: key}
	resp, err := client.CompleteJSON(context.Background(), Request{
		System: `Return a JSON object {"ok": true}.`,
		Prompt: "ping",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Tokens <= 0 || !json.Valid(resp.JSON) {
		t.Fatalf("bad live response: tokens=%d json=%s", resp.Tokens, resp.JSON)
	}
}
