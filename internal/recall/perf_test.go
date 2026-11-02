package recall_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/sqlite"
)

// TestRecallLatencyAt10K measures p50/p95 of Recall over 10,000 embedded
// episodes (1536-d, the OpenAI size). It is a measurement, not a gate, and
// only runs with REVERIE_PERF=1 because seeding takes a while.
func TestRecallLatencyAt10K(t *testing.T) {
	if os.Getenv("REVERIE_PERF") == "" {
		t.Skip("set REVERIE_PERF=1 to run")
	}
	const n, dims = 10000, 1536
	ctx := context.Background()
	st, err := sqlite.Open(t.TempDir()+"/perf.db", sqlite.Config{EmbeddingModel: "fixture", Dimensions: dims})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := rand.New(rand.NewSource(7))
	words := []string{"deploy", "gateway", "timeout", "lint", "migration", "cache", "retry", "auth", "billing", "queue"}
	scoped := st.For(sc)

	t0 := time.Now()
	for lo := 0; lo < n; lo += 500 {
		err := scoped.Tx(ctx, func(tx store.Tx) error {
			for i := lo; i < lo+500; i++ {
				task := fmt.Sprintf("task-%05d", i)
				text := fmt.Sprintf("%s %s %s code ERR_%05d", words[r.Intn(10)], words[r.Intn(10)], words[r.Intn(10)], i)
				vec := make([]float32, dims)
				for j := range vec {
					vec[j] = r.Float32() - 0.5
				}
				at := time.Now().Add(-time.Duration(i) * time.Minute)
				if err := tx.Episodes().Save(ctx, store.Episode{TaskID: task, Title: task, FirstEvent: reverie.NewID(), LastEvent: reverie.NewID(),
					StartedAt: at, EndedAt: at, Closed: true, Outcome: store.Success, Importance: r.Float64(), Tier: store.Hot, Consolidated: i > 10}); err != nil {
					return err
				}
				if err := tx.Index().Upsert(ctx, store.Item{Kind: "episode", RefID: task, Text: text, Embedding: vec}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("seeded %d episodes in %v", n, time.Since(t0))

	q := make([]float32, dims)
	for j := range q {
		q[j] = r.Float32() - 0.5
	}
	var lat []time.Duration
	for i := 0; i < 40; i++ {
		start := time.Now()
		pack, err := recall.Run(ctx, scoped, recall.Query{Task: fmt.Sprintf("%s %s ERR_%05d", words[i%10], words[(i+3)%10], i*7), Vector: q, Budget: 1500})
		if err != nil || len(pack.Episodes) == 0 {
			t.Fatalf("recall: %v %+v", err, pack)
		}
		lat = append(lat, time.Since(start))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("recall over %d embedded episodes: p50=%v p95=%v max=%v", n, lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1])
}

// TestLoad10KEpisodes5KFactsAndConflictScan measures M5 (T5.5):
//   - Recall p50/p95/max over 10,000 embedded episodes + 5,000 facts (FTS + 1536-d vector + 2-hop entity expansion)
//   - Conflict-scan latency (Live(entity, relation) lookups over 5,000 facts during a dream reconcile pass)
//
// Run with: REVERIE_PERF=1 go test -run TestLoad10KEpisodes5KFactsAndConflictScan -v ./internal/recall
func TestLoad10KEpisodes5KFactsAndConflictScan(t *testing.T) {
	if os.Getenv("REVERIE_PERF") == "" {
		t.Skip("set REVERIE_PERF=1 to run")
	}
	const (
		nEpisodes = 10000
		nFacts    = 5000
		dims      = 1536
	)
	ctx := context.Background()
	st, err := sqlite.Open(t.TempDir()+"/perf_m5.db", sqlite.Config{EmbeddingModel: "fixture", Dimensions: dims})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := rand.New(rand.NewSource(42))
	words := []string{"deploy", "gateway", "timeout", "lint", "migration", "cache", "retry", "auth", "billing", "queue"}
	relations := []string{"uses", "prefers", "works_on", "depends_on", "lives_in"}
	scoped := st.For(sc)

	t0 := time.Now()
	for lo := 0; lo < nEpisodes; lo += 500 {
		err := scoped.Tx(ctx, func(tx store.Tx) error {
			for i := lo; i < lo+500; i++ {
				task := fmt.Sprintf("task-%05d", i)
				text := fmt.Sprintf("%s %s %s code ERR_%05d", words[r.Intn(10)], words[r.Intn(10)], words[r.Intn(10)], i)
				vec := make([]float32, dims)
				for j := range vec {
					vec[j] = r.Float32() - 0.5
				}
				at := time.Now().Add(-time.Duration(i) * time.Minute)
				if err := tx.Episodes().Save(ctx, store.Episode{
					TaskID: task, Title: task, FirstEvent: reverie.NewID(), LastEvent: reverie.NewID(),
					StartedAt: at, EndedAt: at, Closed: true, Outcome: store.Success,
					Importance: r.Float64(), Tier: store.Warm, Consolidated: i > 10,
				}); err != nil {
					return err
				}
				if err := tx.Index().Upsert(ctx, store.Item{Kind: "episode", RefID: task, Text: text, Embedding: vec}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	t1 := time.Now()
	for lo := 0; lo < nFacts; lo += 500 {
		err := scoped.Tx(ctx, func(tx store.Tx) error {
			for i := lo; i < lo+500; i++ {
				fid := fmt.Sprintf("fact-%05d", i)
				ent := fmt.Sprintf("svc_%03d", i%250)
				rel := relations[i%len(relations)]
				val := fmt.Sprintf("%s_%04d", words[i%len(words)], i)
				at := time.Now().Add(-time.Duration(i) * time.Minute)
				f := reverie.Fact{
					ID:         fid,
					Entity:     ent,
					Relation:   rel,
					Value:      val,
					Confidence: 0.85,
					Source:     reverie.SourceDistilled,
					Status:     reverie.FactActive,
					ValidFrom:  at,
					RecordedAt: at,
					EpisodeIDs: []string{fmt.Sprintf("task-%05d", i)},
				}
				if err := tx.Facts().Save(ctx, f); err != nil {
					return err
				}
				if err := tx.Index().Upsert(ctx, store.Item{
					Kind:  "fact",
					RefID: fid,
					Text:  ent + " " + rel + " " + val,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("seeded %d episodes (%v) and %d facts (%v)", nEpisodes, t1.Sub(t0), nFacts, time.Since(t1))

	// 1. Measure Recall p50 / p95 / max across 25 queries over 10K episodes + 5K facts.
	qVec := make([]float32, dims)
	for j := range qVec {
		qVec[j] = r.Float32() - 0.5
	}
	var recallLat []time.Duration
	for i := 0; i < 25; i++ {
		start := time.Now()
		qText := fmt.Sprintf("svc_%03d %s ERR_%05d", i%250, words[i%10], i*7)
		pack, err := recall.Run(ctx, scoped, recall.Query{Task: qText, Vector: qVec, Budget: 1500})
		if err != nil || (len(pack.Episodes) == 0 && len(pack.Facts) == 0) {
			t.Fatalf("recall: %v %+v", err, pack)
		}
		recallLat = append(recallLat, time.Since(start))
	}
	sort.Slice(recallLat, func(i, j int) bool { return recallLat[i] < recallLat[j] })
	p50 := recallLat[len(recallLat)/2]
	p95 := recallLat[len(recallLat)*95/100]
	maxLat := recallLat[len(recallLat)-1]
	t.Logf("recall over %d episodes + %d facts: p50=%v p95=%v max=%v", nEpisodes, nFacts, p50, p95, maxLat)

	// 2. Measure conflict-scan time across all 250 entities x 5 relations (1,250 Live lookups over 5,000 facts).
	scanStart := time.Now()
	totalLive := 0
	err = scoped.View(ctx, func(tx store.Tx) error {
		for eIdx := 0; eIdx < 250; eIdx++ {
			ent := fmt.Sprintf("svc_%03d", eIdx)
			for _, rel := range relations {
				live, err := tx.Facts().Live(ctx, ent, rel)
				if err != nil {
					return err
				}
				totalLive += len(live)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scanDur := time.Since(scanStart)
	t.Logf("conflict scan over %d facts (1,250 Live lookups): total_live=%d duration=%v (per_lookup=%v)",
		nFacts, totalLive, scanDur, scanDur/1250)
}
