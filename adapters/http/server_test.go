package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/adapters/hook"
	revhttp "github.com/Dubjay18/reverie/adapters/http"
	"github.com/Dubjay18/reverie/adapters/mcp"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/store"
)

func TestOpenAPIAndFullRoundTrip(t *testing.T) {
	fake := &llm.Fake{
		Rules: []llm.Rule{
			{
				Match: "",
				Output: map[string]any{
					"facts": []map[string]any{
						{
							"entity":     "staging",
							"relation":   "uses",
							"value":      "pg_advisory_lock for migrations",
							"confidence": 0.85,
						},
					},
				},
				Tokens: 90,
			},
		},
	}

	eng, err := engine.Open(filepath.Join(t.TempDir(), "http.db"), engine.WithLLM(fake))
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer eng.Close()

	defScope := reverie.Scope{Tenant: "local", Agent: "http-agent", Project: "proj", User: "jay"}
	srv := httptest.NewServer(&revhttp.Server{Mem: eng, DefaultScope: defScope})
	defer srv.Close()

	// 1. Verify /openapi.json serves valid OpenAPI 3.1 JSON.
	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.json status=%d", resp.StatusCode)
	}
	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("unexpected openapi version: %v", spec["openapi"])
	}

	// 2. Record an episode, remember a fact, dream, recall, expand, forget, revert.
	post := func(path string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		r, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status=%d body=%s", path, r.StatusCode, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("POST %s decode: %v (%s)", path, err, raw)
		}
		return out
	}

	recOut := post("/v1/record", map[string]any{
		"events": []map[string]any{
			{"task_id": "ep-1", "kind": "message", "origin": "user_input", "text": "Fix migration lock contention"},
			{"task_id": "ep-1", "kind": "task.ended", "origin": "system", "payload": map[string]any{"outcome": "failure", "summary": "lock timeout during concurrent deploy"}},
		},
	})
	if ids, _ := recOut["ids"].([]any); len(ids) != 2 {
		t.Fatalf("expected 2 recorded ids, got %v", recOut)
	}

	remOut := post("/v1/remember", map[string]any{
		"entity":   "jay",
		"relation": "prefers",
		"value":    "zero-downtime migrations",
		"text":     "Jay prefers zero-downtime migrations",
	})
	factObj, _ := remOut["fact"].(map[string]any)
	factID, _ := factObj["ID"].(string)
	if factID == "" {
		t.Fatalf("expected fact ID from /v1/remember, got %v", remOut)
	}

	dreamOut := post("/v1/dream", map[string]any{"trigger": "manual", "budget": 2000})
	if learned, _ := dreamOut["learned"].(float64); learned < 1 {
		t.Fatalf("expected dream to learn >=1 fact/lesson, got %v", dreamOut)
	}

	recallOut := post("/v1/recall", map[string]any{"task": "migrations for jay", "budget": 1200})
	rendered, _ := recallOut["rendered"].(string)
	if !strings.Contains(rendered, "zero-downtime migrations") {
		t.Fatalf("expected recalled pack to contain remembered fact, got:\n%s", rendered)
	}

	expOut := post("/v1/expand", map[string]any{"ref": "ep:ep-1"})
	if evs, _ := expOut["events"].([]any); len(evs) != 2 {
		t.Fatalf("expected 2 expanded events, got %v", expOut)
	}

	// Forget the agent-remembered fact and then revert the forget run.
	fgtOut := post("/v1/forget", map[string]any{"ref": "fact:" + factID})
	runID, _ := fgtOut["run_id"].(string)
	if runID == "" {
		t.Fatalf("expected run_id from /v1/forget, got %v", fgtOut)
	}

	revOut := post("/v1/revert", map[string]any{"run_id": runID})
	if rev, _ := revOut["reverted"].(float64); rev < 1 {
		t.Fatalf("expected reverted >= 1, got %v", revOut)
	}
}

// TestParityClaudeCodeMCPHookAndTSAgentHTTP runs the exact same two-session
// scenario through (A) Claude Code (hook.Handle + mcp.Server) and (B) a
// TypeScript agent running adapters/http/client.ts over HTTP, verifying parity.
func TestParityClaudeCodeMCPHookAndTSAgentHTTP(t *testing.T) {
	newModel := func() *llm.Fake {
		return &llm.Fake{
			Rules: []llm.Rule{
				{
					Match: "",
					Output: map[string]any{
						"facts": []map[string]any{
							{
								"entity":     "staging",
								"relation":   "uses",
								"value":      "pg_advisory_lock before migrations",
								"confidence": 0.9,
							},
						},
					},
					Tokens: 80,
				},
			},
		}
	}

	// --- Surface A: Claude Code (hook + MCP stdio) ---
	engMCP, err := engine.Open(filepath.Join(t.TempDir(), "mcp.db"), engine.WithLLM(newModel()))
	if err != nil {
		t.Fatalf("open mcp db: %v", err)
	}
	defer engMCP.Close()

	ctx := context.Background()
	mcpScope := reverie.Scope{Tenant: "local", Agent: "claude-code", Project: "/workspace/demo", User: "jay"}
	hookDef := hook.Defaults{Tenant: mcpScope.Tenant, Agent: mcpScope.Agent, Project: mcpScope.Project, User: mcpScope.User}

	// Hook records prompt and stop; MCP records agent decision and remembers preference.
	promptPayload := `{"session_id":"sess-1","cwd":"/workspace/demo","hook_event_name":"UserPromptSubmit","prompt":"Run database migrations on staging"}`
	if err := hook.Handle(ctx, engMCP, hookDef, strings.NewReader(promptPayload), io.Discard); err != nil {
		t.Fatalf("hook UserPromptSubmit: %v", err)
	}

	mcpSrv := &mcp.Server{Mem: engMCP, Scope: mcpScope, Version: "0.1"}
	callMCP := func(tool string, args map[string]any) string {
		t.Helper()
		reqLine, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      tool,
				"arguments": args,
			},
		})
		var out bytes.Buffer
		if err := mcpSrv.Serve(ctx, bytes.NewReader(append(reqLine, '\n')), &out); err != nil {
			t.Fatalf("mcp %s: %v", tool, err)
		}
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.Result.IsError || len(resp.Result.Content) == 0 {
			t.Fatalf("mcp %s unexpected response: %s", tool, out.String())
		}
		return resp.Result.Content[0].Text
	}

	callMCP("memory_record", map[string]any{
		"task_id": "sess-1",
		"kind":    "decision",
		"text":    "Always acquire pg_advisory_lock before running migrations",
	})
	// Close episode as failure and run dream.
	endEv := reverie.NewEvent(mcpScope, "sess-1", reverie.TaskEnded, reverie.OriginSystem,
		json.RawMessage(`{"outcome":"failure","summary":"Migration failed with lock timeout when concurrent worker ran"}`))
	if err := engMCP.Record(ctx, endEv); err != nil {
		t.Fatalf("Record TaskEnded: %v", err)
	}
	callMCP("memory_remember", map[string]any{
		"entity":   "jay",
		"relation": "prefers",
		"value":    "PostgreSQL 16",
		"text":     "Jay prefers PostgreSQL 16",
	})
	mcpDream, err := engMCP.Dream(ctx, mcpScope, "manual", 4000)
	if err != nil {
		t.Fatalf("mcp Dream: %v", err)
	}
	mcpRecalled := callMCP("memory_recall", map[string]any{
		"task":   "Run database migrations for jay on staging",
		"budget": 1200,
	})
	mcpExpanded := callMCP("memory_expand", map[string]any{
		"ref": "ep:sess-1",
	})

	// --- Surface B: TypeScript Agent over HTTP (adapters/http/client.ts) ---
	engHTTP, err := engine.Open(filepath.Join(t.TempDir(), "ts-http.db"), engine.WithLLM(newModel()))
	if err != nil {
		t.Fatalf("open http db: %v", err)
	}
	defer engHTTP.Close()

	httpSrv := httptest.NewServer(&revhttp.Server{Mem: engHTTP})
	defer httpSrv.Close()

	cmd := exec.CommandContext(ctx, "node", "--experimental-strip-types", "client.ts", httpSrv.URL)
	rawTS, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node client.ts failed: %v\n%s", err, rawTS)
	}
	var tsOut struct {
		FactID        string `json:"fact_id"`
		DreamLearned  int    `json:"dream_learned"`
		Rendered      string `json:"rendered"`
		ExpandedCount int    `json:"expanded_count"`
	}
	lines := strings.Split(strings.TrimSpace(string(rawTS)), "\n")
	lastLine := lines[len(lines)-1]
	if err := json.Unmarshal([]byte(lastLine), &tsOut); err != nil {
		t.Fatalf("decode client.ts output %q: %v", lastLine, err)
	}

	// Verify both surfaces learned the same number of facts in Dream, recalled the
	// user preference + distilled fact + failure lesson, and expanded the 3 raw events.
	if mcpDream.Learned != tsOut.DreamLearned || tsOut.DreamLearned < 2 {
		t.Fatalf("dream learned mismatch: mcp=%d ts=%d", mcpDream.Learned, tsOut.DreamLearned)
	}
	for _, needle := range []string{"PostgreSQL 16", "pg_advisory_lock", "task sess-1 (failure): Run database migrations on staging"} {
		if !strings.Contains(mcpRecalled, needle) {
			t.Errorf("Claude Code MCP recall missing %q:\n%s", needle, mcpRecalled)
		}
		if !strings.Contains(tsOut.Rendered, needle) {
			t.Errorf("TS HTTP recall missing %q:\n%s", needle, tsOut.Rendered)
		}
	}
	mcpEventLines := strings.Split(strings.TrimSpace(mcpExpanded), "\n")
	if len(mcpEventLines) != 3 || tsOut.ExpandedCount != 3 {
		t.Fatalf("expanded event count mismatch: mcp=%d ts=%d", len(mcpEventLines), tsOut.ExpandedCount)
	}
}

func TestHTTPRememberAndForgetEnforceAgentSource(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(filepath.Join(t.TempDir(), "http-trust.db"))
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer eng.Close()

	defScope := reverie.Scope{Tenant: "local", Agent: "http-agent", Project: "proj", User: "jay"}
	srv := httptest.NewServer(&revhttp.Server{Mem: eng, DefaultScope: defScope})
	defer srv.Close()

	// Trigger A: POST /v1/remember with source="user_stated" must ignore the
	// client-supplied source and store SourceAgent (never SourceUser).
	remBody, _ := json.Marshal(map[string]any{
		"entity":   "jay",
		"relation": "note",
		"value":    "minted as user by a TS agent",
		"source":   "user_stated",
	})
	resp, err := http.Post(srv.URL+"/v1/remember", "application/json", bytes.NewReader(remBody))
	if err != nil {
		t.Fatalf("POST /v1/remember: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/remember status=%d body=%s", resp.StatusCode, raw)
	}
	var remOut struct {
		Fact reverie.Fact `json:"fact"`
	}
	if err := json.Unmarshal(raw, &remOut); err != nil {
		t.Fatalf("decode remember response: %v", err)
	}
	if remOut.Fact.Source == reverie.SourceUser || remOut.Fact.Source != reverie.SourceAgent {
		t.Fatalf("Trigger A: expected stored Source=%q, got %q (confidence=%v)", reverie.SourceAgent, remOut.Fact.Source, remOut.Fact.Confidence)
	}

	// Trigger B: Engine.Remember with SourceUser, then POST /v1/forget with
	// source="user_stated" must fail and leave the user fact live.
	userFact, err := eng.Remember(ctx, defScope, reverie.FactInput{
		Entity:   "jay",
		Relation: "note",
		Value:    "keep this",
		Source:   reverie.SourceUser,
	})
	if err != nil {
		t.Fatalf("Engine.Remember SourceUser: %v", err)
	}
	fgtBody, _ := json.Marshal(map[string]any{
		"ref":    "fact:" + userFact.ID,
		"source": "user_stated",
	})
	fgtResp, err := http.Post(srv.URL+"/v1/forget", "application/json", bytes.NewReader(fgtBody))
	if err != nil {
		t.Fatalf("POST /v1/forget: %v", err)
	}
	fgtRaw, _ := io.ReadAll(fgtResp.Body)
	fgtResp.Body.Close()
	if fgtResp.StatusCode == http.StatusOK {
		t.Fatalf("Trigger B: expected POST /v1/forget on user_stated fact to fail, got 200: %s", fgtRaw)
	}

	liveFacts, err := eng.Facts(ctx, defScope, store.FactQuery{Entity: "jay", Relation: "note", Limit: 20}, time.Time{})
	if err != nil {
		t.Fatalf("eng.Facts: %v", err)
	}
	stillLive := false
	for _, f := range liveFacts {
		if f.ID == userFact.ID && f.Live() {
			stillLive = true
		}
	}
	if !stillLive {
		t.Fatalf("Trigger B: expected user fact %s to remain live after rejected HTTP forget; got %+v", userFact.ID, liveFacts)
	}
}
