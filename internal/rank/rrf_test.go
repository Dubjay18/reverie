package rank

import (
	"math"
	"testing"
)

func TestRRF(t *testing.T) {
	got := RRF([]string{"a", "b", "c"}, []string{"c", "d", "a"})

	// a: 1/61 + 1/63, c: 1/63 + 1/61 -> tied, broken by ID; then b (1/62) and d (1/62) tied.
	want := []string{"a", "c", "b", "d"}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("rank %d: got %q, want %q (%v)", i, got[i].ID, id, got)
		}
	}
	if w := 1.0/61 + 1.0/63; math.Abs(got[0].Score-w) > 1e-12 {
		t.Errorf("score for a = %v, want %v", got[0].Score, w)
	}
}

func TestRRFAgreementBeatsSingleTopRank(t *testing.T) {
	// "both" is second in each list; "solo" is first in only one.
	got := RRF([]string{"solo", "both"}, []string{"other", "both"})
	if got[0].ID != "both" {
		t.Fatalf("want both first, got %v", got)
	}
}

func TestRRFEmpty(t *testing.T) {
	if got := RRF(); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}
