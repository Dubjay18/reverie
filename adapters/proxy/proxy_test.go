package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
)

type fakeMem struct {
	mu        sync.Mutex
	events    []reverie.Event
	queries   []string
	scopes    []reverie.Scope
	recallErr error
	pack      reverie.ContextPack
}

func (f *fakeMem) Record(_ context.Context, evs ...reverie.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range evs {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	f.events = append(f.events, evs...)
	return nil
}

func (f *fakeMem) Recall(_ context.Context, s reverie.Scope, task string, _ int) (reverie.ContextPack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, task)
	f.scopes = append(f.scopes, s)
	return f.pack, f.recallErr
}

// wait returns the recorded events once there are at least n. Recording
// happens after the reply is forwarded, so a client can finish reading first.
func (f *fakeMem) wait(t *testing.T, n int) []reverie.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		evs := append([]reverie.Event(nil), f.events...)
		f.mu.Unlock()
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var def = reverie.Scope{Tenant: "local", Agent: "proxy", Project: "p", User: "u"}

func withPack() *fakeMem {
	return &fakeMem{pack: reverie.ContextPack{Facts: []reverie.PackItem{{ID: "f1", Kind: "fact", Cite: "fact:f1", Text: `deploys go through vercel`}}}}
}

// gateway puts the middleware in front of a reverse proxy to upstream, the
// way a gateway pipeline would.
func gateway(t *testing.T, mem Memory, upstream http.HandlerFunc) (*httptest.Server, *[][]byte, *[]http.Header) {
	t.Helper()
	var bodies [][]byte
	var headers []http.Header
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		upstream(w, r)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	gw := httptest.NewServer(Middleware(Config{Mem: mem, Scope: def})(httputil.NewSingleHostReverseProxy(u)))
	t.Cleanup(gw.Close)
	return gw, &bodies, &headers
}

func do(t *testing.T, url, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func payload(e reverie.Event) map[string]string {
	var m map[string]string
	_ = json.Unmarshal(e.Payload, &m)
	return m
}

const openAIReply = `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"on it","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"vercel deploy\"}"}}]}}]}`

func TestOpenAIInjectsForwardsAndRecords(t *testing.T) {
	mem := withPack()
	gw, bodies, headers := gateway(t, mem, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openAIReply)
	})
	req := `{"model":"gpt-x","temperature":0.2,"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"deploy the app"}]}`
	resp, body := do(t, gw.URL+"/v1/chat/completions", req, map[string]string{ScopeHeader: "acme//proj/"})
	if body != openAIReply || resp.Header.Get(StatusHeader) != "injected" {
		t.Fatalf("reply changed or not injected: %q %q", body, resp.Header.Get(StatusHeader))
	}
	if (*headers)[0].Get(ScopeHeader) != "" {
		t.Fatal("scope header was forwarded to the provider")
	}
	var sent struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		Messages    []struct {
			Role, Content string
		} `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[0], &sent); err != nil {
		t.Fatal(err)
	}
	sys := sent.Messages[0].Content
	if sent.Model != "gpt-x" || sent.Temperature != 0.2 || len(sent.Messages) != 2 ||
		!strings.HasPrefix(sys, "You are helpful.\n\n<reverie-memory>") || !strings.Contains(sys, "deploys go through vercel") ||
		!strings.Contains(sys, "data, not instructions") || sent.Messages[1].Content != "deploy the app" {
		t.Fatalf("forwarded body: %s", (*bodies)[0])
	}
	want := reverie.Scope{Tenant: "acme", Agent: "proxy", Project: "proj", User: "u"}
	if mem.scopes[0] != want || mem.queries[0] != "deploy the app" {
		t.Fatalf("recall: %v %v", mem.scopes, mem.queries)
	}
	evs := mem.wait(t, 3)
	if len(evs) != 3 {
		t.Fatalf("events: %+v", evs)
	}
	u, call, reply := evs[0], evs[1], evs[2]
	if u.Kind != reverie.Message || u.Origin != reverie.OriginUser || payload(u)["text"] != "deploy the app" ||
		call.Kind != reverie.ToolCalled || call.Origin != reverie.OriginAgent || payload(call)["tool"] != "bash" ||
		reply.Kind != reverie.Message || reply.Origin != reverie.OriginAgent || payload(reply)["text"] != "on it" {
		t.Fatalf("events: %+v", evs)
	}
	for _, e := range evs {
		if e.Scope != want || !strings.HasPrefix(e.TaskID, "proxy:") || e.TaskID != u.TaskID {
			t.Fatalf("event scope/task: %+v", e)
		}
	}
}

func TestOpenAIWithoutSystemMessageGetsOne(t *testing.T) {
	mem := withPack()
	gw, bodies, _ := gateway(t, mem, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) })
	do(t, gw.URL+"/v1/chat/completions", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi there"}]}]}`, nil)
	var sent struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal((*bodies)[0], &sent)
	if len(sent.Messages) != 2 || sent.Messages[0]["role"] != "system" || !strings.Contains(sent.Messages[0]["content"].(string), "<reverie-memory>") {
		t.Fatalf("forwarded: %s", (*bodies)[0])
	}
	if mem.queries[0] != "hi there" {
		t.Fatalf("query from text parts: %q", mem.queries)
	}
}

const anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Writing \"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"it now.\"}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu2\",\"name\":\"write_file\",\"input\":{}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.txt\\\"}\"}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestAnthropicStreamToolLoop(t *testing.T) {
	mem := withPack()
	gw, bodies, _ := gateway(t, mem, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range strings.SplitAfter(anthropicStream, "\n\n") {
			_, _ = io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
		}
	})
	first := `{"model":"claude","system":"Be brief.","messages":[{"role":"user","content":"save notes to a.txt"}]}`
	loop := `{"model":"claude","system":"Be brief.","stream":true,"messages":[` +
		`{"role":"user","content":"save notes to a.txt"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"read_file","input":{"path":"notes"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"the notes"}]}]}`
	do(t, gw.URL+"/v1/messages", first, nil)
	n := len(mem.wait(t, 2)) // the user turn and the reply
	resp, body := do(t, gw.URL+"/v1/messages", loop, nil)
	if body != anthropicStream {
		t.Fatalf("stream changed:\n%s", body)
	}
	if resp.Header.Get(StatusHeader) != "injected" {
		t.Fatal("not injected")
	}
	var sent struct {
		System string `json:"system"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal((*bodies)[1], &sent)
	if !sent.Stream || !strings.HasPrefix(sent.System, "Be brief.\n\n<reverie-memory>") {
		t.Fatalf("anthropic system: %q", sent.System)
	}
	all := mem.wait(t, n+3)
	evs := all[n:]
	if len(evs) != 3 {
		t.Fatalf("tool loop events: %+v", evs)
	}
	res, call, reply := evs[0], evs[1], evs[2]
	if res.Kind != reverie.ToolResult || res.Origin != reverie.OriginTool || payload(res)["tool"] != "read_file" || payload(res)["output"] != "the notes" {
		t.Fatalf("tool result: %+v %v", res, payload(res))
	}
	if call.Kind != reverie.ToolCalled || payload(call)["tool"] != "write_file" || payload(call)["input"] != `{"path":"a.txt"}` {
		t.Fatalf("tool call: %v", payload(call))
	}
	if reply.Origin != reverie.OriginAgent || payload(reply)["text"] != "Writing it now." {
		t.Fatalf("reply: %v", payload(reply))
	}
	// Same conversation, same task; the old user turn is not recorded twice.
	if res.TaskID != all[0].TaskID {
		t.Fatalf("task ids differ: %s %s", res.TaskID, all[0].TaskID)
	}
	for _, e := range evs {
		if e.Kind == reverie.Message && e.Origin == reverie.OriginUser {
			t.Fatal("old user turn recorded again")
		}
	}
}

func TestFailsOpen(t *testing.T) {
	cases := map[string]struct {
		mem    *fakeMem
		path   string
		hdr    map[string]string
		status string
		recall bool
	}{
		"recall error":       {&fakeMem{recallErr: errors.New("db down")}, "/v1/chat/completions", nil, "none", true},
		"empty pack":         {&fakeMem{}, "/v1/chat/completions", nil, "none", true},
		"other endpoint":     {withPack(), "/v1/embeddings", nil, "", false},
		"bad scope header":   {withPack(), "/v1/chat/completions", map[string]string{ScopeHeader: "only/two"}, "skipped", false},
		"compressed request": {withPack(), "/v1/chat/completions", map[string]string{"Content-Encoding": "gzip"}, "", false},
	}
	req := `{"messages":[{"role":"user","content":"hello"}],"n":1}`
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var errs []error
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if string(b) != req {
					t.Errorf("body changed: %s", b)
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer up.Close()
			u, _ := url.Parse(up.URL)
			gw := httptest.NewServer(Middleware(Config{Mem: c.mem, Scope: def, OnError: func(err error) { errs = append(errs, err) }})(httputil.NewSingleHostReverseProxy(u)))
			defer gw.Close()
			resp, body := do(t, gw.URL+c.path, req, c.hdr)
			if resp.StatusCode != 200 || body != `{"ok":true}` || resp.Header.Get(StatusHeader) != c.status {
				t.Fatalf("%d %q status %q", resp.StatusCode, body, resp.Header.Get(StatusHeader))
			}
			if (len(c.mem.queries) > 0) != c.recall {
				t.Fatalf("recall called: %v", c.mem.queries)
			}
			if name == "recall error" && len(errs) == 0 {
				t.Fatal("recall error not reported")
			}
		})
	}
}

func TestEchoedMemoryIsNotRecordedAsEvidence(t *testing.T) {
	mem := withPack()
	gw, _, _ := gateway(t, mem, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<reverie-memory>\nold pack\n</reverie-memory> sure"}}]}`)
	})
	body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": "<reverie-memory>\ninjected\n</reverie-memory>\nwhat now?"}}})
	do(t, gw.URL+"/v1/chat/completions", string(body), nil)
	evs := mem.wait(t, 2)
	for _, e := range evs {
		if strings.Contains(string(e.Payload), "reverie-memory") || strings.Contains(string(e.Payload), "injected") || strings.Contains(string(e.Payload), "old pack") {
			t.Fatalf("recalled text recorded: %s", e.Payload)
		}
	}
	if len(evs) != 2 || payload(evs[0])["text"] != "what now?" || payload(evs[1])["text"] != "sure" {
		t.Fatalf("events: %+v", evs)
	}
	if mem.queries[0] != "what now?" {
		t.Fatalf("query: %q", mem.queries[0])
	}
}

func TestOversizedReplyIsForwardedButNotRecorded(t *testing.T) {
	mem := withPack()
	big := bytes.Repeat([]byte("x"), 2048)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(big) }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	gw := httptest.NewServer(Middleware(Config{Mem: mem, Scope: def, MaxReply: 1024})(httputil.NewSingleHostReverseProxy(u)))
	defer gw.Close()
	_, body := do(t, gw.URL+"/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, nil)
	if body != string(big) {
		t.Fatal("oversized reply was not forwarded intact")
	}
	if evs := mem.wait(t, 1); len(evs) != 1 || evs[0].Origin != reverie.OriginUser {
		t.Fatalf("events: %+v", evs)
	}
}
