// Package telemetry provides OpenTelemetry-compatible spans, token metering,
// and attribute mapping for Reverie operations (Recall, Expand, Dream phases,
// and LLM/embedder calls).
//
// Per Architecture §10, all attribute names pass through a single mapping
// function (Attr) so switching between reverie.* and official OTel GenAI memory
// conventions happens in one place.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dubjay18/reverie/internal/embed"
	"github.com/Dubjay18/reverie/internal/llm"
)

// attrTable maps logical attribute keys to canonical attribute names.
// Every span and metric attribute emitted by Reverie goes through Attr().
var attrTable = map[string]string{
	"operation":     "reverie.operation",
	"tenant":        "reverie.scope.tenant",
	"agent":         "reverie.scope.agent",
	"project":       "reverie.scope.project",
	"user":          "reverie.scope.user",
	"task":          "reverie.recall.task",
	"budget":        "reverie.budget.tokens",
	"tokens":        "reverie.pack.tokens",
	"dropped":       "reverie.pack.dropped",
	"ref":           "reverie.expand.ref",
	"events":        "reverie.expand.events",
	"trigger":       "reverie.dream.trigger",
	"run_id":        "reverie.dream.run_id",
	"phase":         "reverie.dream.phase",
	"episodes":      "reverie.dream.episodes",
	"learned":       "reverie.dream.learned",
	"merged":        "reverie.dream.merged",
	"superseded":    "reverie.dream.superseded",
	"contested":     "reverie.dream.contested",
	"forgotten":     "reverie.dream.forgotten",
	"rejected":      "reverie.dream.rejected",
	"tokens_spent":  "reverie.dream.tokens_spent",
	"input_tokens":  "reverie.usage.input_tokens",
	"output_tokens": "reverie.usage.output_tokens",
	"total_tokens":  "reverie.usage.total_tokens",
	"texts":         "reverie.embed.texts",
	"dimensions":    "reverie.embed.dimensions",
}

// Attr is the single mapping function for all telemetry attribute names.
func Attr(logical string) string {
	logical = strings.TrimSpace(logical)
	if mapped, ok := attrTable[logical]; ok {
		return mapped
	}
	if strings.HasPrefix(logical, "reverie.") {
		return logical
	}
	return "reverie." + logical
}

// SpanRecord is an immutable completed span exported to a Collector.
type SpanRecord struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id,omitempty"`
	Name         string         `json:"name"`
	StartTime    time.Time      `json:"start_time"`
	EndTime      time.Time      `json:"end_time"`
	DurationNs   int64          `json:"duration_ns"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	Status       string         `json:"status"` // "OK" or "ERROR"
	Error        string         `json:"error,omitempty"`
}

// TokenSample records a single metered LLM or Embedder call.
type TokenSample struct {
	Kind         string    `json:"kind"` // "llm" or "embed"
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	At           time.Time `json:"at"`
}

// TokenSnapshot summarizes cumulative token usage across LLM and Embedder calls.
type TokenSnapshot struct {
	LLMCalls        int64 `json:"llm_calls"`
	LLMInputTokens  int64 `json:"llm_input_tokens"`
	LLMOutputTokens int64 `json:"llm_output_tokens"`
	LLMTotalTokens  int64 `json:"llm_total_tokens"`
	EmbedCalls      int64 `json:"embed_calls"`
	EmbedTokens     int64 `json:"embed_tokens"`
}

// TokenMeter tracks token consumption across LLM and embedding calls.
type TokenMeter struct {
	llmCalls        atomic.Int64
	llmInputTokens  atomic.Int64
	llmOutputTokens atomic.Int64
	llmTotalTokens  atomic.Int64
	embedCalls      atomic.Int64
	embedTokens     atomic.Int64
}

// RecordLLM adds usage from one LLM completion call.
func (m *TokenMeter) RecordLLM(in, out, total int) {
	if total <= 0 {
		total = in + out
	}
	m.llmCalls.Add(1)
	m.llmInputTokens.Add(int64(in))
	m.llmOutputTokens.Add(int64(out))
	m.llmTotalTokens.Add(int64(total))
}

// RecordEmbed adds usage from one Embedder call.
func (m *TokenMeter) RecordEmbed(tokens int) {
	m.embedCalls.Add(1)
	m.embedTokens.Add(int64(tokens))
}

// Snapshot returns an atomic snapshot of metered tokens.
func (m *TokenMeter) Snapshot() TokenSnapshot {
	return TokenSnapshot{
		LLMCalls:        m.llmCalls.Load(),
		LLMInputTokens:  m.llmInputTokens.Load(),
		LLMOutputTokens: m.llmOutputTokens.Load(),
		LLMTotalTokens:  m.llmTotalTokens.Load(),
		EmbedCalls:      m.embedCalls.Load(),
		EmbedTokens:     m.embedTokens.Load(),
	}
}

// Collector receives finished spans and token usage samples.
type Collector interface {
	ExportSpan(rec SpanRecord)
	ExportTokens(sample TokenSample)
}

// LocalCollector stores spans and token samples in memory and optionally
// forwards them as OTLP-style JSON payloads to an HTTP collector endpoint
// (e.g. a local OTel collector or test server).
type LocalCollector struct {
	mu       sync.Mutex
	spans    []SpanRecord
	samples  []TokenSample
	Endpoint string
	Client   *http.Client
}

// NewLocalCollector creates an in-memory collector; if endpoint is non-empty,
// every span and token sample is also POSTed to endpoint+"/v1/traces" and
// endpoint+"/v1/metrics".
func NewLocalCollector(endpoint string) *LocalCollector {
	return &LocalCollector{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Client:   &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *LocalCollector) ExportSpan(rec SpanRecord) {
	c.mu.Lock()
	c.spans = append(c.spans, rec)
	ep := c.Endpoint
	client := c.Client
	c.mu.Unlock()

	if ep != "" && client != nil {
		body, err := json.Marshal(map[string]any{
			"resourceSpans": []map[string]any{
				{
					"service": "reverie",
					"spans":   []SpanRecord{rec},
				},
			},
		})
		if err == nil {
			req, err := http.NewRequest(http.MethodPost, ep+"/v1/traces", bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}
		}
	}
}

func (c *LocalCollector) ExportTokens(s TokenSample) {
	c.mu.Lock()
	c.samples = append(c.samples, s)
	ep := c.Endpoint
	client := c.Client
	c.mu.Unlock()

	if ep != "" && client != nil {
		body, err := json.Marshal(map[string]any{
			"resourceMetrics": []map[string]any{
				{
					"service": "reverie",
					"metrics": []TokenSample{s},
				},
			},
		})
		if err == nil {
			req, err := http.NewRequest(http.MethodPost, ep+"/v1/metrics", bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}
		}
	}
}

// Spans returns a copy of all collected spans.
func (c *LocalCollector) Spans() []SpanRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SpanRecord, len(c.spans))
	copy(out, c.spans)
	return out
}

// Samples returns a copy of all collected token samples.
func (c *LocalCollector) Samples() []TokenSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TokenSample, len(c.samples))
	copy(out, c.samples)
	return out
}

// Tracer creates spans and owns a TokenMeter.
type Tracer struct {
	collector Collector
	meter     TokenMeter
}

// NewTracer returns a Tracer backed by col (or a default LocalCollector if nil).
func NewTracer(col Collector) *Tracer {
	if col == nil {
		col = NewLocalCollector("")
	}
	return &Tracer{collector: col}
}

// Meter returns the tracer's cumulative token meter.
func (t *Tracer) Meter() *TokenMeter {
	return &t.meter
}

// Collector returns the underlying Collector.
func (t *Tracer) Collector() Collector {
	return t.collector
}

type spanCtxKey struct{}

type spanParent struct {
	traceID string
	spanID  string
}

// Span is an in-flight trace span.
type Span struct {
	tracer       *Tracer
	traceID      string
	spanID       string
	parentSpanID string
	name         string
	start        time.Time
	mu           sync.Mutex
	attrs        map[string]any
}

// Start begins a new span, linking to any active parent span in ctx.
// All initial attribute keys are normalized through Attr().
func (t *Tracer) Start(ctx context.Context, name string, logicalAttrs map[string]any) (context.Context, *Span) {
	if t == nil {
		t = NewTracer(nil)
	}
	traceID := randomHex(16)
	var parentID string
	if p, ok := ctx.Value(spanCtxKey{}).(spanParent); ok && p.traceID != "" {
		traceID = p.traceID
		parentID = p.spanID
	}
	spanID := randomHex(8)
	attrs := make(map[string]any, len(logicalAttrs))
	for k, v := range logicalAttrs {
		attrs[Attr(k)] = v
	}
	sp := &Span{
		tracer:       t,
		traceID:      traceID,
		spanID:       spanID,
		parentSpanID: parentID,
		name:         name,
		start:        time.Now().UTC(),
		attrs:        attrs,
	}
	return context.WithValue(ctx, spanCtxKey{}, spanParent{traceID: traceID, spanID: spanID}), sp
}

// SetAttr sets a span attribute, mapping the logical key via Attr().
func (s *Span) SetAttr(logicalKey string, val any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attrs == nil {
		s.attrs = map[string]any{}
	}
	s.attrs[Attr(logicalKey)] = val
}

// End completes the span and exports it to the tracer's collector.
func (s *Span) End(err error) {
	if s == nil || s.tracer == nil {
		return
	}
	end := time.Now().UTC()
	s.mu.Lock()
	attrsCopy := make(map[string]any, len(s.attrs))
	for k, v := range s.attrs {
		attrsCopy[k] = v
	}
	s.mu.Unlock()

	rec := SpanRecord{
		TraceID:      s.traceID,
		SpanID:       s.spanID,
		ParentSpanID: s.parentSpanID,
		Name:         s.name,
		StartTime:    s.start,
		EndTime:      end,
		DurationNs:   end.Sub(s.start).Nanoseconds(),
		Attributes:   attrsCopy,
		Status:       "OK",
	}
	if err != nil {
		rec.Status = "ERROR"
		rec.Error = err.Error()
	}
	if s.tracer.collector != nil {
		s.tracer.collector.ExportSpan(rec)
	}
}

// RecordTokens records token usage on both the Tracer's Meter and Collector.
func (t *Tracer) RecordTokens(kind string, in, out, total int) {
	if t == nil {
		return
	}
	if total <= 0 {
		total = in + out
	}
	if kind == "embed" {
		t.meter.RecordEmbed(total)
	} else {
		t.meter.RecordLLM(in, out, total)
	}
	if t.collector != nil {
		t.collector.ExportTokens(TokenSample{
			Kind:         kind,
			InputTokens:  in,
			OutputTokens: out,
			TotalTokens:  total,
			At:           time.Now().UTC(),
		})
	}
}

// WrapLLM returns an llm.LLM that emits a "llm.complete" span and meters tokens
// on every CompleteJSON call.
func WrapLLM(base llm.LLM, tr *Tracer) llm.LLM {
	if base == nil || tr == nil {
		return base
	}
	if _, ok := base.(*meteredLLM); ok {
		return base
	}
	return &meteredLLM{base: base, tr: tr}
}

type meteredLLM struct {
	base llm.LLM
	tr   *Tracer
}

func (m *meteredLLM) CompleteJSON(ctx context.Context, req llm.Request) (llm.Response, error) {
	ctx, sp := m.tr.Start(ctx, "llm.complete", map[string]any{
		"operation": "llm_complete",
	})
	resp, err := m.base.CompleteJSON(ctx, req)
	if err == nil {
		total := resp.Tokens
		if total <= 0 {
			total = resp.InputTokens + resp.OutputTokens
		}
		inTok := resp.InputTokens
		outTok := resp.OutputTokens
		if inTok == 0 && outTok == 0 && total > 0 {
			inTok = total / 2
			outTok = total - inTok
		}
		sp.SetAttr("input_tokens", inTok)
		sp.SetAttr("output_tokens", outTok)
		sp.SetAttr("total_tokens", total)
		m.tr.RecordTokens("llm", inTok, outTok, total)
	}
	sp.End(err)
	return resp, err
}

// WrapEmbedder returns an embed.Embedder that emits an "embed" span and meters
// input tokens on every Embed call.
func WrapEmbedder(base embed.Embedder, tr *Tracer) embed.Embedder {
	if base == nil || tr == nil {
		return base
	}
	if _, ok := base.(*meteredEmbedder); ok {
		return base
	}
	return &meteredEmbedder{base: base, tr: tr}
}

type meteredEmbedder struct {
	base embed.Embedder
	tr   *Tracer
}

func (m *meteredEmbedder) Dimensions() int { return m.base.Dimensions() }

func (m *meteredEmbedder) ModelName() string {
	if n, ok := m.base.(interface{ ModelName() string }); ok {
		return n.ModelName()
	}
	return "unknown"
}

func (m *meteredEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	ctx, sp := m.tr.Start(ctx, "embed", map[string]any{
		"operation":  "embed",
		"texts":      len(texts),
		"dimensions": m.base.Dimensions(),
	})
	vecs, err := m.base.Embed(ctx, texts)
	if err == nil {
		chars := 0
		for _, t := range texts {
			chars += len(t)
		}
		toks := (chars + 3) / 4
		if toks == 0 && len(texts) > 0 {
			toks = len(texts)
		}
		sp.SetAttr("total_tokens", toks)
		m.tr.RecordTokens("embed", toks, 0, toks)
	}
	sp.End(err)
	return vecs, err
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}
