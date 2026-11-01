package telemetry_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/telemetry"
)

type fakeEmbedder struct{}

func (fakeEmbedder) Dimensions() int   { return 4 }
func (fakeEmbedder) ModelName() string { return "test-embed-4" }
func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{0.5, 0.5, 0.5, 0.5}
	}
	return out, nil
}

func TestSpansAndTokenMeterVisibleInLocalCollector(t *testing.T) {
	var (
		mu             sync.Mutex
		httpTraces     []telemetry.SpanRecord
		httpTokenDraws []telemetry.TokenSample
	)
	collectorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/traces":
			var payload struct {
				ResourceSpans []struct {
					Spans []telemetry.SpanRecord `json:"spans"`
				} `json:"resourceSpans"`
			}
			if err := json.Unmarshal(raw, &payload); err == nil {
				for _, rs := range payload.ResourceSpans {
					httpTraces = append(httpTraces, rs.Spans...)
				}
			}
		case "/v1/metrics":
			var payload struct {
				ResourceMetrics []struct {
					Metrics []telemetry.TokenSample `json:"metrics"`
				} `json:"resourceMetrics"`
			}
			if err := json.Unmarshal(raw, &payload); err == nil {
				for _, rm := range payload.ResourceMetrics {
					httpTokenDraws = append(httpTokenDraws, rm.Metrics...)
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collectorSrv.Close()

	col := telemetry.NewLocalCollector(collectorSrv.URL)
	tr := telemetry.NewTracer(col)

	fakeModel := &llm.Fake{
		Rules: []llm.Rule{
			{
				Match: "",
				Output: map[string]any{
					"facts": []map[string]any{
						{
							"entity":     "user",
							"relation":   "uses",
							"value":      "Go 1.24",
							"confidence": 0.9,
						},
					},
				},
				Tokens: 120,
			},
		},
	}

	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	eng, err := engine.Open(
		dbPath,
		engine.WithTracer(tr),
		engine.WithLLM(fakeModel),
		engine.WithEmbedder(fakeEmbedder{}),
	)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer eng.Close()

	ctx := context.Background()
	sc := reverie.Scope{Tenant: "t1", Agent: "a1", Project: "p1", User: "u1"}

	ev1 := reverie.NewEvent(sc, "task-tel-1", reverie.Message, reverie.OriginUser, json.RawMessage(`{"text":"We use Go 1.24 in production"}`))
	ev2 := reverie.NewEvent(sc, "task-tel-1", reverie.TaskEnded, reverie.OriginSystem, json.RawMessage(`{"outcome":"success","summary":"confirmed Go 1.24"}`))
	if err := eng.Record(ctx, ev1, ev2); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := eng.EmbedPending(ctx, 16); err != nil {
		t.Fatalf("EmbedPending: %v", err)
	}

	if _, err := eng.Dream(ctx, sc, "manual", 2000); err != nil {
		t.Fatalf("Dream: %v", err)
	}

	pack, err := eng.Recall(ctx, sc, "what Go version do we use", 1000)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(pack.Facts) == 0 {
		t.Fatalf("expected recalled fact, got none")
	}

	evs, err := eng.Expand(ctx, sc, reverie.Ref{TaskID: "task-tel-1"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("expected 2 expanded events, got %d", len(evs))
	}

	spans := col.Spans()
	wantNames := []string{
		"embed",
		"dream.run",
		"dream.phase.replay",
		"dream.phase.distill",
		"dream.phase.reconcile_reflect",
		"dream.phase.forget",
		"llm.complete",
		"recall",
		"expand",
	}
	seen := map[string]telemetry.SpanRecord{}
	for _, s := range spans {
		seen[s.Name] = s
		for k := range s.Attributes {
			if !strings.HasPrefix(k, "reverie.") {
				t.Errorf("span %q has unmapped attribute key %q (must come from telemetry.Attr)", s.Name, k)
			}
		}
	}
	for _, name := range wantNames {
		if _, ok := seen[name]; !ok {
			t.Errorf("missing expected span %q; got spans=%v", name, spans)
		}
	}

	// Verify parent-child linkage on dream phases.
	dreamSpan := seen["dream.run"]
	distillSpan := seen["dream.phase.distill"]
	if distillSpan.TraceID != dreamSpan.TraceID || distillSpan.ParentSpanID != dreamSpan.SpanID {
		t.Errorf("distill phase span not linked to dream.run parent: distill=%+v dream=%+v", distillSpan, dreamSpan)
	}

	// Verify token meter recorded both LLM and embedder tokens.
	snap := tr.Meter().Snapshot()
	if snap.LLMCalls < 1 || snap.LLMTotalTokens != 120 {
		t.Errorf("expected LLMCalls>=1 and LLMTotalTokens=120, got %+v", snap)
	}
	if snap.EmbedCalls < 2 || snap.EmbedTokens <= 0 {
		t.Errorf("expected EmbedCalls>=2 and EmbedTokens>0, got %+v", snap)
	}

	// Verify spans and token metrics arrived at the local HTTP collector.
	mu.Lock()
	gotHTTPTraces := len(httpTraces)
	gotHTTPMetrics := len(httpTokenDraws)
	mu.Unlock()
	if gotHTTPTraces < len(wantNames) {
		t.Errorf("local HTTP collector received %d spans, want at least %d", gotHTTPTraces, len(wantNames))
	}
	if gotHTTPMetrics < 2 {
		t.Errorf("local HTTP collector received %d token metric samples, want >= 2", gotHTTPMetrics)
	}
}
