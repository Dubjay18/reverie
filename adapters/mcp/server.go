// Package mcp serves Reverie over the Model Context Protocol on stdio
// (newline-delimited JSON-RPC 2.0). It is hand-written to avoid a dependency:
// the surface is initialize, ping, tools/list, tools/call, resources/list and
// resources/read.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
)

// PackURI is the resource a client can preload: the pack for the current
// scope with no task, i.e. what is pinned, contested and recent.
const PackURI = "memory://pack/current"

// SkillReporter is the optional part of Mem that takes skill outcomes. When
// Mem implements it, the memory_skill_outcome tool is offered.
type SkillReporter interface {
	SkillOutcome(ctx context.Context, s reverie.Scope, ref string, success bool) (store.Skill, skills.Transition, error)
}

var supported = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const (
	defaultBudget = 1500
	minBudget     = 100
	maxBudget     = 8000
	maxExpandText = 20000
	maxRecordText = 8 << 10
	maxLine       = 4 << 20
)

type Server struct {
	Mem     reverie.Memory
	Scope   reverie.Scope
	Version string

	mu sync.Mutex // serialises writes to the client
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve handles requests from r until it closes or ctx ends.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, tooLong, err := readLine(br)
		if err != nil && line == "" && !tooLong {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if tooLong {
			s.write(w, response{ID: json.RawMessage("null"), Error: &rpcError{-32600, "request too large"}})
			if err != nil {
				return eofOK(err)
			}
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if err != nil {
				return eofOK(err)
			}
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.write(w, response{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" { // a notification: no reply
			continue
		}
		s.write(w, s.handle(ctx, req))
		if err != nil {
			return eofOK(err)
		}
	}
}

// readLine returns the next newline-terminated line. A line longer than
// maxLine is discarded up to its newline and reported as tooLong, so one
// oversized request cannot end the session.
func readLine(br *bufio.Reader) (line string, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if !tooLong {
			buf = append(buf, chunk...)
			if len(buf) > maxLine {
				buf, tooLong = nil, true
			}
		}
		switch {
		case rerr == nil:
			return string(buf), tooLong, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		default:
			return string(buf), tooLong, rerr
		}
	}
}

func (s *Server) write(w io.Writer, resp response) {
	resp.JSONRPC = "2.0"
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Write(append(b, '\n'))
}

func (s *Server) handle(ctx context.Context, req request) response {
	resp := response{ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := supported[0]
		for _, v := range supported {
			if v == p.ProtocolVersion {
				ver = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": "reverie", "version": s.Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		list := tools
		if _, ok := s.Mem.(SkillReporter); ok {
			list = append(append([]map[string]any(nil), tools...), skillOutcomeTool)
		}
		resp.Result = map[string]any{"tools": list}
	case "resources/list":
		resp.Result = map[string]any{"resources": []map[string]any{{
			"uri":         PackURI,
			"name":        "Current memory pack",
			"description": "What Reverie would recall for this scope with no task: pinned and contested facts, recent episodes and active skills.",
			"mimeType":    "text/markdown",
		}}}
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			return resp
		}
		if p.URI != PackURI {
			resp.Error = &rpcError{-32002, "resource not found: " + p.URI}
			return resp
		}
		pack, err := s.Mem.Recall(ctx, s.Scope, "", defaultBudget)
		if err != nil {
			resp.Error = &rpcError{-32603, err.Error()}
			return resp
		}
		text := pack.Render()
		if text == "" {
			text = "No relevant memory yet."
		}
		resp.Result = map[string]any{"contents": []map[string]any{{"uri": PackURI, "mimeType": "text/markdown", "text": text}}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			return resp
		}
		text, err := s.call(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			resp.Error = &rpcError{-32602, err.Error()}
			return resp
		}
		res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		if err != nil {
			res["content"] = []map[string]any{{"type": "text", "text": err.Error()}}
			res["isError"] = true
		}
		resp.Result = res
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp
}

var errUnknownTool = errors.New("unknown tool")

var tools = []map[string]any{
	{
		"name":        "memory_recall",
		"description": "Recall what earlier sessions learned that is relevant to a task. Returns cited items; pass a citation's ref to memory_expand for the raw events.",
		"inputSchema": schema(map[string]any{
			"task":   prop("string", "What you are about to do, in a sentence. Empty returns recent activity."),
			"budget": prop("integer", fmt.Sprintf("Token budget for the answer (default %d, max %d).", defaultBudget, maxBudget)),
		}),
	},
	{
		"name":        "memory_record",
		"description": "Record a decision, a correction you were given, or a note so future sessions can recall it. Everything you record is stored as written by the agent, including feedback; it is not treated as the user's own words.",
		"inputSchema": schema(map[string]any{
			"kind":    map[string]any{"type": "string", "enum": []string{"decision", "feedback", "message"}, "description": "decision: a choice and its reason. feedback: a correction you were given, as you understood it. message: any other note."},
			"text":    prop("string", fmt.Sprintf("What to remember (max %d bytes).", maxRecordText)),
			"task_id": prop("string", "Task this belongs to. Defaults to \"mcp\"."),
		}, "kind", "text"),
	},
	{
		"name":        "memory_remember",
		"description": "Remember a fact for future sessions. Give a sentence in text; optionally also entity, relation and value to state it as structured data (for example entity=jay, relation=lives_in, value=London). It is stored as something you were told to remember, not as the user's own words, and a conflicting statement the user made themselves will win over it.",
		"inputSchema": schema(map[string]any{
			"text":     prop("string", fmt.Sprintf("What to remember (max %d bytes).", maxRecordText)),
			"entity":   prop("string", "Optional. Who or what the fact is about."),
			"relation": prop("string", "Optional. A relation such as prefers, uses, lives_in, decision, lesson. Unknown relations are rejected."),
			"value":    prop("string", "Optional. The value of the relation."),
		}, "text"),
	},
	{
		"name":        "memory_forget",
		"description": "Forget one fact by its ref from a memory_recall citation (fact:<id>). The fact stops being recalled; the change is journaled and the user can undo it. You cannot forget a fact the user stated themselves, and pinned facts cannot be forgotten this way.",
		"inputSchema": schema(map[string]any{
			"ref": prop("string", "The fact ref, e.g. fact:01J..."),
		}, "ref"),
	},
	{
		"name":        "memory_expand",
		"description": "Return the raw events behind a recalled item. Use the ref shown in brackets: ep:deploy#01J...A..01J...Z for an episode, or fact:<id> for the events a fact came from.",
		"inputSchema": schema(map[string]any{
			"ref": prop("string", "The ref from a memory_recall citation."),
		}, "ref"),
	},
}

var skillOutcomeTool = map[string]any{
	"name":        "memory_skill_outcome",
	"description": "Report whether a recalled skill worked, using its ref (skill:<id>@v<n>). A proposed method becomes a skill after enough successful uses; a skill that keeps failing is withdrawn.",
	"inputSchema": schema(map[string]any{
		"ref":     prop("string", "The skill ref from a memory_recall citation, e.g. skill:deploy@v2."),
		"success": prop("boolean", "true if following the skill worked."),
	}, "ref", "success"),
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "memory_recall":
		var a struct {
			Task   string `json:"task"`
			Budget int    `json:"budget"`
		}
		if err := decode(args, &a); err != nil {
			return "", err
		}
		budget := a.Budget
		if budget == 0 {
			budget = defaultBudget
		}
		budget = min(max(budget, minBudget), maxBudget)
		pack, err := s.Mem.Recall(ctx, s.Scope, a.Task, budget)
		if err != nil {
			return "", err
		}
		if out := pack.Render(); out != "" {
			return out, nil
		}
		return "No relevant memory yet.", nil

	case "memory_record":
		var a struct {
			Kind   string `json:"kind"`
			Text   string `json:"text"`
			TaskID string `json:"task_id"`
		}
		if err := decode(args, &a); err != nil {
			return "", err
		}
		switch a.Kind {
		case "decision", "feedback", "message":
		default:
			return "", fmt.Errorf("kind must be decision, feedback or message, got %q", a.Kind)
		}
		if strings.TrimSpace(a.Text) == "" {
			return "", errors.New("text is required")
		}
		if len(a.Text) > maxRecordText {
			return "", fmt.Errorf("text is %d bytes; the limit is %d", len(a.Text), maxRecordText)
		}
		if a.TaskID == "" {
			a.TaskID = "mcp"
		}
		payload, _ := json.Marshal(map[string]string{"text": a.Text})
		ev := reverie.NewEvent(s.Scope, a.TaskID, reverie.EventKind(a.Kind), reverie.OriginAgent, payload)
		if err := s.Mem.Record(ctx, ev); err != nil {
			return "", err
		}
		return "recorded " + ev.ID.String(), nil

	case "memory_remember":
		var a struct {
			Text, Entity, Relation, Value string
		}
		if err := decode(args, &a); err != nil {
			return "", err
		}
		if strings.TrimSpace(a.Text) == "" && strings.TrimSpace(a.Value) == "" {
			return "", errors.New("text is required")
		}
		if len(a.Text) > maxRecordText || len(a.Value) > maxRecordText {
			return "", fmt.Errorf("text is too long; the limit is %d bytes", maxRecordText)
		}
		in := reverie.FactInput{Entity: a.Entity, Relation: a.Relation, Value: a.Value, Source: reverie.SourceAgent, TaskID: "mcp"}
		if in.Relation == "" || in.Value == "" {
			in.Entity, in.Relation, in.Value = "", "", a.Text // a plain sentence is filed as a note
		}
		f, err := s.Mem.Remember(ctx, s.Scope, in)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("remembered fact:%s (%s %s: %s)", f.ID, f.Entity, f.Relation, f.Value), nil

	case "memory_forget":
		var a struct {
			Ref string `json:"ref"`
		}
		if err := decode(args, &a); err != nil {
			return "", err
		}
		ref, err := reverie.ParseRef(a.Ref)
		if err != nil || ref.FactID == "" {
			return "", errors.New("ref must be a fact ref like fact:01J...")
		}
		run, err := s.Mem.Forget(ctx, s.Scope, ref.FactID, reverie.SourceAgent)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("forgot fact:%s (undo with: reverie revert %s)", ref.FactID, run), nil

	case "memory_expand":
		var a struct {
			Ref string `json:"ref"`
		}
		if err := decode(args, &a); err != nil {
			return "", err
		}
		ref, err := reverie.ParseRef(a.Ref)
		if err != nil {
			return "", err
		}
		events, err := s.Mem.Expand(ctx, s.Scope, ref)
		if err != nil {
			return "", err
		}
		return formatEvents(events), nil

	case "memory_skill_outcome":
		rep, ok := s.Mem.(SkillReporter)
		if !ok {
			break
		}
		var a struct {
			Ref     string `json:"ref"`
			Success *bool  `json:"success"`
		}
		if err := decode(args, &a); err != nil {
			return "", err
		}
		if a.Success == nil {
			return "", errors.New("success is required")
		}
		sk, tr, err := rep.SkillOutcome(ctx, s.Scope, a.Ref, *a.Success)
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("recorded for %s: %d/%d successful, rate %.2f, %s", skills.Ref(sk), sk.Successes, sk.Uses, skills.Rate(sk), sk.Status)
		switch tr {
		case skills.Promoted:
			msg += " (promoted to an active skill)"
		case skills.Deprecate:
			msg += " (deprecated: it fails too often)"
		}
		return msg, nil
	}
	return "", fmt.Errorf("%w: %s", errUnknownTool, name)
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func formatEvents(events []reverie.Event) string {
	if len(events) == 0 {
		return "No events for that ref."
	}
	var b strings.Builder
	for i, e := range events {
		line := fmt.Sprintf("%s %s %s %s %s\n", e.ID, e.At.UTC().Format("2006-01-02T15:04:05Z"), e.Kind, e.Origin, e.Payload)
		if len(line) > maxExpandText {
			// Keep an oversized event visible, shortened, instead of hiding it.
			line = strings.ToValidUTF8(line[:maxExpandText-80], "") + fmt.Sprintf("…[event cut: %d bytes in all]\n", len(line))
		}
		if b.Len()+len(line) > maxExpandText {
			fmt.Fprintf(&b, "… %d more events; narrow the ref to a range (ep:<task>#<from>..<to>).\n", len(events)-i)
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// eofOK is nil for a normal end of input and the error for a real read failure.
func eofOK(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
