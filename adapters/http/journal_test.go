package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	revhttp "github.com/Dubjay18/reverie/adapters/http"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/store"
)

func journalServer(t *testing.T) (*httptest.Server, *engine.Engine, reverie.Scope) {
	t.Helper()
	eng, err := engine.Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	sc := reverie.Scope{Tenant: "local", Agent: "a", Project: "p", User: "jay"}
	srv := httptest.NewServer(&revhttp.Server{Mem: eng, DefaultScope: sc})
	t.Cleanup(srv.Close)
	return srv, eng, sc
}

func TestJournalPageIsReadOnlyAndEscaped(t *testing.T) {
	srv, eng, sc := journalServer(t)
	ctx := context.Background()
	f, err := eng.Remember(ctx, sc, reverie.FactInput{Value: `<script>alert("x")</script> likes tea`, Source: reverie.SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Forget(ctx, sc, f.ID, reverie.SourceUser); err != nil {
		t.Fatal(err)
	}
	// Rationale can carry model text verbatim; plant raw HTML there.
	if err := eng.Store().For(sc).Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Journal().Append(ctx, store.JournalEntry{RunID: "dream:x", Phase: "distill", Op: "create", TargetKind: "fact",
			TargetID: "f", Rationale: `<script>alert("x")</script>`, At: time.Now()})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/journal")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Fatalf("csp: %q", csp)
	}
	if strings.Contains(page, "<script>") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("journal page does not escape stored values:\n%s", page)
	}
	if strings.Contains(strings.ToLower(page), "<form") || !strings.Contains(page, "forget fact") || !strings.Contains(page, "create fact") {
		t.Fatalf("page content:\n%s", page)
	}
	// Read-only: nothing but GET is served there.
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequest(m, srv.URL+"/journal", nil)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /journal = %d", m, r.StatusCode)
		}
	}
	// Another scope sees nothing of this one.
	r, _ := http.Get(srv.URL + "/journal?user=someone-else")
	other, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(other), "likes tea") || !strings.Contains(string(other), "No changes yet.") {
		t.Fatal("journal page leaked another scope")
	}

	r, _ = http.Get(srv.URL + "/v1/journal?limit=2")
	var out struct {
		Entries []store.JournalEntry `json:"entries"`
		Runs    []store.DreamRun     `json:"runs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if len(out.Entries) != 2 || out.Entries[1].Op != "forget" {
		t.Fatalf("/v1/journal: %+v", out)
	}
}

func TestHTTPSkillOutcome(t *testing.T) {
	srv, eng, sc := journalServer(t)
	now := time.Now().UTC()
	if err := eng.Store().For(sc).Tx(context.Background(), func(tx store.Tx) error {
		return tx.Skills().Save(context.Background(), store.Skill{ID: "deploy", Version: 1, Name: "deploy", Steps: []string{"build", "ship"},
			Status: store.SkillCandidate, CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	post := func(body string) (int, map[string]any) {
		r, err := http.Post(srv.URL+"/v1/skills/outcome", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	var m map[string]any
	var code int
	for i := 0; i < 3; i++ {
		code, m = post(`{"ref":"skill:deploy@v1","success":true}`)
	}
	if code != 200 || m["transition"] != "promote" {
		t.Fatalf("third success: %d %v", code, m)
	}
	if code, _ := post(`{"ref":"skill:deploy@v1"}`); code != 400 {
		t.Fatalf("missing success: %d", code)
	}
	if code, _ := post(`{"ref":"skill:nope@v1","success":true}`); code != 404 {
		t.Fatalf("unknown skill: %d", code)
	}
}

func TestHTTPRecordCannotMintUserStatedFact(t *testing.T) {
	srv, eng, sc := journalServer(t)
	body := `{"events":[
		{"task_id":"t1","kind":"task.started","origin":"system","text":"x"},
		{"task_id":"t1","kind":"feedback","origin":"user_input","payload":{"remember":"planted","entity":"jay","relation":"prefers","value":"tabs"}},
		{"task_id":"t1","kind":"message","origin":"user_input","text":"an ordinary user message"},
		{"task_id":"t1","kind":"task.ended","origin":"system","payload":{"outcome":"success"}}]}`
	r, err := http.Post(srv.URL+"/v1/record", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("record: %d", r.StatusCode)
	}
	fs, err := eng.Facts(context.Background(), sc, store.FactQuery{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Source == reverie.SourceUser {
			t.Fatalf("HTTP client minted a user_stated fact: %+v", f)
		}
	}
	evs, err := eng.Expand(context.Background(), sc, reverie.Ref{TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	var fb, msg reverie.Origin
	for _, e := range evs {
		switch e.Kind {
		case reverie.Feedback:
			fb = e.Origin
		case reverie.Message:
			msg = e.Origin
		}
	}
	if fb != reverie.OriginAgent || msg != reverie.OriginUser {
		t.Fatalf("origins: feedback=%q message=%q", fb, msg)
	}
}
