package recall

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
)

// wordTok counts whitespace-separated words: deliberately unlike Approx and
// not additive over concatenation, to prove the budget guarantee does not
// depend on the tokenizer being well behaved.
type wordTok struct{}

func (wordTok) Count(s string) int { return len(strings.Fields(s)) }

func randomCands(r *rand.Rand) map[string][]candidate {
	out := map[string][]candidate{}
	for _, kind := range []string{"episode", "fact", "skill", "insight"} {
		n := r.Intn(12)
		if kind != "episode" && r.Intn(3) > 0 {
			n = 0
		}
		for i := 0; i < n; i++ {
			words := 1 + r.Intn(60)
			if r.Intn(20) == 0 {
				words = 4000 // one huge summary
			}
			text := strings.TrimSpace(strings.Repeat("lorem ", words))
			out[kind] = append(out[kind], candidate{
				item:  reverie.PackItem{ID: fmt.Sprint(kind, i), Kind: kind, Cite: fmt.Sprintf("ep:%s%d", kind, i), Text: text, Hot: r.Intn(3) == 0},
				score: r.Float64(),
				at:    time.Unix(int64(r.Intn(1e6)), 0),
			})
		}
	}
	return out
}

func TestPackNeverExceedsBudget(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		budget := []int{0, 1, 10, 50, 200, 1000, 5000}[r.Intn(7)]
		for _, tok := range []Tokenizer{Approx{}, wordTok{}} {
			pack := assemble(randomCands(r), budget, tok)
			if got := tok.Count(pack.Render()); got > budget || pack.Tokens != got {
				t.Fatalf("case %d budget %d %T: render costs %d, pack.Tokens=%d", i, budget, tok, got, pack.Tokens)
			}
		}
	}
}

func TestSmallBudgetStillReturnsSomethingUseful(t *testing.T) {
	cands := map[string][]candidate{"episode": {{
		item:  reverie.PackItem{ID: "t", Kind: "episode", Cite: "ep:t", Text: strings.Repeat("deploy failed with a very long stack trace ", 200)},
		score: 1,
	}}}
	pack := assemble(cands, 200, Approx{})
	if len(pack.Episodes) != 1 || !strings.Contains(pack.Episodes[0].Text, "expand") {
		t.Fatalf("a single oversize item should be truncated with a marker, got %+v", pack.Episodes)
	}
	if pack.Tokens > 200 {
		t.Fatalf("over budget: %d", pack.Tokens)
	}
}

func TestHotItemsComeLastAndWarmByScore(t *testing.T) {
	mk := func(id string, score float64, hot bool, at int64) candidate {
		return candidate{item: reverie.PackItem{ID: id, Kind: "episode", Cite: "ep:" + id, Text: id, Hot: hot}, score: score, at: time.Unix(at, 0)}
	}
	pack := assemble(map[string][]candidate{"episode": {
		mk("hot-old", 0.9, true, 1), mk("warm-low", 0.1, false, 5), mk("hot-new", 0.2, true, 2), mk("warm-high", 0.8, false, 3),
	}}, 1000, Approx{})
	var got []string
	for _, it := range pack.Episodes {
		got = append(got, it.ID)
	}
	want := []string{"warm-high", "warm-low", "hot-old", "hot-new"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestUnusedLayerBudgetFlowsToEpisodes(t *testing.T) {
	var cs []candidate
	for i := 0; i < 20; i++ {
		cs = append(cs, candidate{item: reverie.PackItem{ID: fmt.Sprint(i), Kind: "episode", Cite: fmt.Sprint("ep:", i), Text: strings.Repeat("word ", 30)}, score: float64(20 - i)})
	}
	pack := assemble(map[string][]candidate{"episode": cs}, 400, Approx{})
	if pack.Tokens < 360 { // only episodes exist, so nearly the whole budget should be used
		t.Fatalf("budget wasted: %d of 400 used", pack.Tokens)
	}
}
