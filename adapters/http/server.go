// Package http provides the JSON HTTP API and OpenAPI 3.1 document for Reverie.
package http

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/dream"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

//go:embed openapi.json
var openAPISpec []byte

// OpenAPI returns the embedded OpenAPI 3.1 JSON document.
func OpenAPI() []byte {
	out := make([]byte, len(openAPISpec))
	copy(out, openAPISpec)
	return out
}

// Memory is the engine subset served over HTTP.
type Memory interface {
	Record(ctx context.Context, events ...reverie.Event) error
	Recall(ctx context.Context, s reverie.Scope, task string, budget int) (reverie.ContextPack, error)
	Expand(ctx context.Context, s reverie.Scope, ref reverie.Ref) ([]reverie.Event, error)
	Remember(ctx context.Context, s reverie.Scope, in reverie.FactInput) (reverie.Fact, error)
	Forget(ctx context.Context, s reverie.Scope, factID string, by reverie.FactSource) (runID string, err error)
	Dream(ctx context.Context, s reverie.Scope, trigger string, budget int) (dream.Report, error)
	Revert(ctx context.Context, s reverie.Scope, runID string) (int, error)
	Facts(ctx context.Context, s reverie.Scope, q store.FactQuery, asOf time.Time) ([]reverie.Fact, error)
}

// Server serves the Reverie JSON API over HTTP.
type Server struct {
	Mem          Memory
	DefaultScope reverie.Scope
}

const maxBodyBytes = 1 << 20 // 1 MB

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/openapi.json" || r.URL.Path == "/v1/openapi.json"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPISpec)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/record":
		s.handleRecord(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/recall":
		s.handleRecall(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/expand":
		s.handleExpand(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/remember":
		s.handleRemember(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/forget":
		s.handleForget(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/dream":
		s.handleDream(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/revert":
		s.handleRevert(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/facts":
		s.handleFacts(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/journal":
		s.handleJournalJSON(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/journal":
		s.handleJournalPage(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/skills/outcome":
		s.handleSkillOutcome(w, r)
	default:
		writeErr(w, http.StatusNotFound, errors.New("not found"))
	}
}

func (s *Server) resolveScope(in reverie.Scope) (reverie.Scope, error) {
	if in.Tenant == "" {
		in.Tenant = s.DefaultScope.Tenant
	}
	if in.Agent == "" {
		in.Agent = s.DefaultScope.Agent
	}
	if in.Project == "" {
		in.Project = s.DefaultScope.Project
	}
	if in.User == "" {
		in.User = s.DefaultScope.User
	}
	return in, in.Validate()
}

type recordItem struct {
	ID        string          `json:"id"`
	TaskID    string          `json:"task_id"`
	Kind      string          `json:"kind"`
	Origin    string          `json:"origin"`
	At        string          `json:"at"`
	Text      string          `json:"text"`
	Payload   json.RawMessage `json:"payload"`
	TokenCost int             `json:"token_cost"`
}

type recordReq struct {
	Scope   reverie.Scope   `json:"scope"`
	Events  []recordItem    `json:"events"`
	TaskID  string          `json:"task_id"`
	Kind    string          `json:"kind"`
	Origin  string          `json:"origin"`
	At      string          `json:"at"`
	Text    string          `json:"text"`
	Payload json.RawMessage `json:"payload"`
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	var req recordReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	items := req.Events
	if len(items) == 0 && (req.TaskID != "" || req.Kind != "" || req.Text != "" || len(req.Payload) > 0) {
		items = []recordItem{{
			TaskID:  req.TaskID,
			Kind:    req.Kind,
			Origin:  req.Origin,
			At:      req.At,
			Text:    req.Text,
			Payload: req.Payload,
		}}
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("no events provided"))
		return
	}
	events := make([]reverie.Event, 0, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		kind := reverie.EventKind(it.Kind)
		if kind == "" {
			kind = reverie.Message
		}
		origin := reverie.Origin(it.Origin)
		if origin == "" {
			origin = reverie.OriginAgent
		}
		if kind == reverie.Feedback && origin == reverie.OriginUser {
			origin = reverie.OriginAgent
		}
		payload := it.Payload
		if len(payload) == 0 {
			b, _ := json.Marshal(map[string]string{"text": it.Text})
			payload = b
		}
		ev := reverie.NewEvent(sc, it.TaskID, kind, origin, payload)
		if it.ID != "" {
			parsed, err := ulid.Parse(it.ID)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			ev.ID = parsed
		}
		if it.At != "" {
			t, err := time.Parse(time.RFC3339Nano, it.At)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			ev.At = t.UTC()
		}
		ev.TokenCost = it.TokenCost
		events = append(events, ev)
		ids = append(ids, ev.ID.String())
	}
	if err := s.Mem.Record(r.Context(), events...); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope  reverie.Scope `json:"scope"`
		Task   string        `json:"task"`
		Budget int           `json:"budget"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Budget <= 0 {
		req.Budget = 1200
	}
	pack, err := s.Mem.Recall(r.Context(), sc, req.Task, req.Budget)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pack":     pack,
		"rendered": pack.Render(),
	})
}

func (s *Server) handleExpand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope reverie.Scope `json:"scope"`
		Ref   string        `json:"ref"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ref, err := reverie.ParseRef(req.Ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	events, err := s.Mem.Expand(r.Context(), sc, ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleRemember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope    reverie.Scope `json:"scope"`
		Text     string        `json:"text"`
		Entity   string        `json:"entity"`
		Relation string        `json:"relation"`
		Value    string        `json:"value"`
		Source   string        `json:"source"`
		TaskID   string        `json:"task_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// HTTP callers are agent clients: never allow a request to escalate to SourceUser.
	in := reverie.FactInput{
		Entity:   req.Entity,
		Relation: req.Relation,
		Value:    req.Value,
		Source:   reverie.SourceAgent,
		TaskID:   req.TaskID,
	}
	if in.Relation == "" || in.Value == "" {
		in.Entity, in.Relation, in.Value = "", "", req.Text
	}
	f, err := s.Mem.Remember(r.Context(), sc, in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fact": f})
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope  reverie.Scope `json:"scope"`
		Ref    string        `json:"ref"`
		Source string        `json:"source"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	factID := strings.TrimSpace(req.Ref)
	if strings.HasPrefix(factID, "fact:") {
		ref, err := reverie.ParseRef(factID)
		if err != nil || ref.FactID == "" {
			writeErr(w, http.StatusBadRequest, errors.New("invalid fact ref"))
			return
		}
		factID = ref.FactID
	}
	// HTTP callers are agent clients: always pass SourceAgent so a client cannot
	// erase a user_stated fact by claiming source="user_stated".
	runID, err := s.Mem.Forget(r.Context(), sc, factID, reverie.SourceAgent)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID})
}

func (s *Server) handleDream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope   reverie.Scope `json:"scope"`
		Trigger string        `json:"trigger"`
		Budget  int           `json:"budget"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rep, err := s.Mem.Dream(r.Context(), sc, req.Trigger, req.Budget)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope reverie.Scope `json:"scope"`
		RunID string        `json:"run_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.resolveScope(req.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.RunID) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("run_id is required"))
		return
	}
	n, err := s.Mem.Revert(r.Context(), sc, req.RunID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reverted": n})
}

func (s *Server) handleFacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc, err := s.resolveScope(reverie.Scope{
		Tenant:  q.Get("tenant"),
		Agent:   q.Get("agent"),
		Project: q.Get("project"),
		User:    q.Get("user"),
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	limit := 50
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	var asOf time.Time
	if raw := q.Get("asof"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		asOf = t
	}
	facts, err := s.Mem.Facts(r.Context(), sc, store.FactQuery{
		Entity:        q.Get("entity"),
		Relation:      q.Get("relation"),
		OnlyContested: q.Get("contested") == "true",
		Limit:         limit,
	}, asOf)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"facts": facts})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
