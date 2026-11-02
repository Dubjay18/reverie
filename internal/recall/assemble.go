package recall

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dubjay18/reverie"
)

// candidate is an item that may enter the pack.
type candidate struct {
	item  reverie.PackItem
	score float64
	at    time.Time // when the thing happened; orders the hot tail
}

// layer shares of the budget, in spill order: what a layer leaves unused
// flows to the next one.
var layers = []struct {
	kind  string
	share float64
}{
	{"fact", 0.25},
	{"episode", 0.40},
	{"skill", 0.15},
	{"insight", 0.10},
}

const (
	minTruncate     = 24 // do not truncate into fewer tokens than this
	truncatedMarker = " …[truncated; expand the ref for the rest]"
)

// assemble packs candidates into budget tokens. The guarantee: the returned
// pack's Render() costs at most budget tokens under tok, however tok counts.
func assemble(byKind map[string][]candidate, budget int, tok Tokenizer) reverie.ContextPack {
	pack := reverie.ContextPack{Budget: budget}
	if budget <= 0 {
		for _, cs := range byKind {
			pack.Dropped += len(cs)
		}
		return pack
	}

	used := 0
	opened := map[string]bool{}
	chosen := map[string][]candidate{}
	var overflow []candidate

	cost := func(kind string, it reverie.PackItem) int {
		c := tok.Count(it.RenderLine())
		if !opened[kind] {
			c += tok.Count("## " + reverie.SectionTitle(kind) + "\n")
		}
		return c
	}
	take := func(c candidate) bool {
		kind := c.item.Kind
		n := cost(kind, c.item)
		left := budget - used
		if n > left {
			t, ok := truncate(c.item, left, kind, opened, tok)
			if !ok {
				return false
			}
			c.item = t
			n = cost(kind, c.item)
		}
		c.item.Tokens = n
		opened[kind] = true
		used += n
		chosen[kind] = append(chosen[kind], c)
		return true
	}

	carry := 0.0
	for _, l := range layers {
		share := l.share*float64(budget) + carry
		spent := 0
		cs := byKind[l.kind]
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
		for _, c := range cs {
			before := used
			if float64(spent)+float64(cost(l.kind, c.item)) > share || !take(c) {
				overflow = append(overflow, c)
				continue
			}
			spent += used - before
		}
		carry = max(share-float64(spent), 0)
	}

	// Second pass: budget a layer's share did not claim goes to the best
	// leftovers, so a pack is not left half empty because one layer was thin.
	sort.SliceStable(overflow, func(i, j int) bool { return overflow[i].score > overflow[j].score })
	for _, c := range overflow {
		if !take(c) {
			pack.Dropped++
		}
	}

	place(&pack, chosen)
	// Tokenizers need not be additive across items, so check the real render
	// and shed the weakest items until it fits.
	for {
		pack.Tokens = tok.Count(pack.Render())
		if pack.Tokens <= budget {
			return pack
		}
		if !dropWeakest(&pack) {
			return pack
		}
		pack.Dropped++
	}
}

// place orders each section for attention: within episodes, relevance-ranked
// warm items first and the newest hot items last, nearest the task.
func place(pack *reverie.ContextPack, chosen map[string][]candidate) {
	order := func(cs []candidate) []reverie.PackItem {
		sort.SliceStable(cs, func(i, j int) bool {
			a, b := cs[i], cs[j]
			if a.item.Hot != b.item.Hot {
				return !a.item.Hot
			}
			if a.item.Hot {
				return a.at.Before(b.at)
			}
			return a.score > b.score
		})
		out := make([]reverie.PackItem, len(cs))
		for i, c := range cs {
			c.item.Score = c.score
			out[i] = c.item
		}
		return out
	}
	pack.Facts = order(chosen["fact"])
	pack.Insights = order(chosen["insight"])
	pack.Episodes = order(chosen["episode"])
	pack.Skills = order(chosen["skill"])
}

func dropWeakest(p *reverie.ContextPack) bool {
	sections := []*[]reverie.PackItem{&p.Insights, &p.Skills, &p.Episodes, &p.Facts}
	var (
		target *[]reverie.PackItem
		idx    = -1
		worst  float64
	)
	for _, s := range sections {
		for i, it := range *s {
			if idx < 0 || it.Score < worst {
				target, idx, worst = s, i, it.Score
			}
		}
	}
	if idx < 0 {
		return false
	}
	*target = append((*target)[:idx], (*target)[idx+1:]...)
	return true
}

// truncate shortens the item's text until it fits in left tokens.
func truncate(it reverie.PackItem, left int, kind string, opened map[string]bool, tok Tokenizer) (reverie.PackItem, bool) {
	if left < minTruncate {
		return it, false
	}
	fits := func(text string) bool {
		c := it
		c.Text = text
		n := tok.Count(c.RenderLine())
		if !opened[kind] {
			n += tok.Count("## " + reverie.SectionTitle(kind) + "\n")
		}
		return n <= left
	}
	runes := []rune(it.Text)
	lo, hi := 0, len(runes) // largest prefix that fits
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(cutAtWord(runes[:mid]) + truncatedMarker) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	text := cutAtWord(runes[:lo]) + truncatedMarker
	if lo == 0 || !fits(text) {
		return it, false
	}
	it.Text = text
	return it, true
}

func cutAtWord(r []rune) string {
	s := string(r)
	if i := strings.LastIndexAny(s, " \n\t"); i > len(s)/2 && utf8.ValidString(s) {
		return s[:i]
	}
	return s
}
