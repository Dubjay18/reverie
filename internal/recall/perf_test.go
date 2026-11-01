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
