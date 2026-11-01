package http

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
)

// JournalReader is the optional part of Mem behind the journal page.
type JournalReader interface {
	Journal(ctx context.Context, s reverie.Scope, limit int) ([]store.JournalEntry, error)
	DreamRuns(ctx context.Context, s reverie.Scope, limit int) ([]store.DreamRun, error)
}

// SkillReporter is the optional part of Mem that takes skill outcomes.
type SkillReporter interface {
	SkillOutcome(ctx context.Context, s reverie.Scope, ref string, success bool) (store.Skill, skills.Transition, error)
}

const (
	defaultJournalLimit = 100
	maxJournalLimit     = 1000
	maxJournalDiff      = 2000 // bytes of before/after shown per entry
)

func (s *Server) queryScope(r *http.Request) (reverie.Scope, error) {
	q := r.URL.Query()
	return s.resolveScope(reverie.Scope{Tenant: q.Get("tenant"), Agent: q.Get("agent"), Project: q.Get("project"), User: q.Get("user")})
}

func journalLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return defaultJournalLimit
	}
	return min(n, maxJournalLimit)
}

func (s *Server) journalData(r *http.Request) (reverie.Scope, []store.DreamRun, []store.JournalEntry, int, error) {
	jr, ok := s.Mem.(JournalReader)
	if !ok {
		return reverie.Scope{}, nil, nil, http.StatusNotImplemented, errors.New("journal not available")
	}
	sc, err := s.queryScope(r)
	if err != nil {
		return sc, nil, nil, http.StatusBadRequest, err
	}
	limit := journalLimit(r)
	entries, err := jr.Journal(r.Context(), sc, limit)
	if err != nil {
		return sc, nil, nil, http.StatusInternalServerError, err
	}
	runs, err := jr.DreamRuns(r.Context(), sc, limit)
	if err != nil {
		return sc, nil, nil, http.StatusInternalServerError, err
	}
	return sc, runs, entries, http.StatusOK, nil
}

// handleJournalJSON serves GET /v1/journal: dream runs and journal entries
// for a scope, newest first.
func (s *Server) handleJournalJSON(w http.ResponseWriter, r *http.Request) {
	_, runs, entries, code, err := s.journalData(r)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "entries": entries})
}

type pageRun struct {
	ID, Trigger, Status, Started, Report string
	Spent, Budget                        int
}

type pageEntry struct {
	ID                                 int64
	At, RunID, Phase, Op, Kind, Target string
	Rationale, Before, After, Episodes string
	Reverted                           bool
}

// handleJournalPage serves GET /journal: a read-only HTML view of what
// dreams and people changed, with the run id to pass to revert. It has no
// forms and runs no scripts; every value is escaped by html/template.
func (s *Server) handleJournalPage(w http.ResponseWriter, r *http.Request) {
	sc, runs, entries, code, err := s.journalData(r)
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	if err != nil {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	data := struct {
		Scope   reverie.Scope
		Runs    []pageRun
		Entries []pageEntry
	}{Scope: sc}
	for _, run := range runs {
		data.Runs = append(data.Runs, pageRun{ID: run.ID, Trigger: run.Trigger, Status: run.Status, Started: stamp(run.StartedAt),
			Spent: run.Spent, Budget: run.Budget, Report: clipText(string(run.Report), 400)})
	}
	for _, e := range entries {
		eps, _ := json.Marshal(e.SourceEpisodes)
		data.Entries = append(data.Entries, pageEntry{
			ID: e.ID, At: stamp(e.At), RunID: e.RunID, Phase: e.Phase, Op: e.Op, Kind: e.TargetKind, Target: e.TargetID,
			Rationale: e.Rationale, Before: clipText(string(e.Before), maxJournalDiff), After: clipText(string(e.After), maxJournalDiff),
			Episodes: string(eps), Reverted: e.Reverted,
		})
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	_ = journalTmpl.Execute(w, data) // headers are sent; a write error has nowhere to go
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // do not split a UTF-8 rune
		cut--
	}
	return s[:cut] + "…"
}

var journalTmpl = template.Must(template.New("journal").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Reverie journal</title>
<style>
body{font:14px/1.4 system-ui,sans-serif;margin:2rem;color:#1d1d1f}
table{border-collapse:collapse;width:100%;margin-bottom:2rem}
th,td{border-bottom:1px solid #ddd;padding:.35rem .5rem;text-align:left;vertical-align:top}
th{background:#f5f5f7}
code,pre{font:12px ui-monospace,monospace;white-space:pre-wrap;word-break:break-all;margin:0}
.rev{color:#888;text-decoration:line-through}
.muted{color:#666}
</style></head><body>
<h1>Reverie journal</h1>
<p class="muted">Read-only. Scope {{.Scope.Tenant}} / {{.Scope.Agent}} / {{.Scope.Project}} / {{.Scope.User}}. Undo a run with <code>reverie revert &lt;run id&gt;</code>.</p>
<h2>Dream runs</h2>
{{if .Runs}}<table><tr><th>Run</th><th>Trigger</th><th>Status</th><th>Started</th><th>Tokens</th><th>Report</th></tr>
{{range .Runs}}<tr><td><code>{{.ID}}</code></td><td>{{.Trigger}}</td><td>{{.Status}}</td><td>{{.Started}}</td><td>{{.Spent}}/{{.Budget}}</td><td><code>{{.Report}}</code></td></tr>
{{end}}</table>{{else}}<p class="muted">No dream runs yet.</p>{{end}}
<h2>Changes</h2>
{{if .Entries}}<table><tr><th>#</th><th>At</th><th>Run</th><th>Phase</th><th>Change</th><th>Why</th><th>Before → after</th></tr>
{{range .Entries}}<tr{{if .Reverted}} class="rev"{{end}}><td>{{.ID}}</td><td>{{.At}}</td><td><code>{{.RunID}}</code></td><td>{{.Phase}}</td>
<td>{{.Op}} {{.Kind}} <code>{{.Target}}</code>{{if .Reverted}} (reverted){{end}}</td><td>{{.Rationale}}<br><span class="muted">episodes {{.Episodes}}</span></td>
<td><details><summary>diff</summary><pre>{{.Before}}</pre><pre>{{.After}}</pre></details></td></tr>
{{end}}</table>{{else}}<p class="muted">No changes yet.</p>{{end}}
</body></html>
`))

// handleSkillOutcome serves POST /v1/skills/outcome.
func (s *Server) handleSkillOutcome(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.Mem.(SkillReporter)
	if !ok {
		writeErr(w, http.StatusNotImplemented, errors.New("skills not available"))
		return
	}
	var req struct {
		Scope   reverie.Scope `json:"scope"`
		Ref     string        `json:"ref"`
		Success *bool         `json:"success"`
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
	if req.Success == nil {
		writeErr(w, http.StatusBadRequest, errors.New("success is required"))
		return
	}
	sk, tr, err := rep.SkillOutcome(r.Context(), sc, req.Ref, *req.Success)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": sk, "rate": skills.Rate(sk), "transition": string(tr)})
}
