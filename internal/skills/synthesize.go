package skills

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/Dubjay18/reverie"
)

const (
	// MinSupport is how many successful episodes must share a tool sequence
	// before it is drafted as a skill.
	MinSupport = 2
	MinSteps   = 2
	MaxSteps   = 20
	// Similar is the normalised LCS similarity at which two tool sequences
	// count as the same procedure.
	Similar = 0.6
)

// Trace is the tool sequence of one successful episode.
type Trace struct {
	EpisodeID string
	Key       string // what the task was about (the dream's cluster key)
	Goal      string
	Tools     []string
}

// Draft is a skill proposed from a cluster of similar traces.
type Draft struct {
	ID         string
	Name       string
	Trigger    string
	Steps      []string
	EpisodeIDs []string
}

var toolNameRE = regexp.MustCompile(`[^a-z0-9_.:-]+`)

// ToolSequence returns the tools an episode called, in order, with repeats
// collapsed. Only agent tool calls count; recalled events are never evidence.
func ToolSequence(evs []reverie.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind != reverie.ToolCalled || ev.Origin == reverie.OriginRecalled {
			continue
		}
		var p struct {
			Tool string `json:"tool"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			continue
		}
		name := toolNameRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(p.Tool)), "_")
		if len(name) > 64 {
			name = name[:64]
		}
		if name == "" || (len(out) > 0 && out[len(out)-1] == name) {
			continue
		}
		out = append(out, name)
		if len(out) == MaxSteps {
			break
		}
	}
	return out
}

// Similarity is 2*LCS(a,b)/(len a + len b): 1 for equal sequences, 0 for
// sequences with nothing in common in order.
func Similarity(a, b []string) float64 {
	if len(a)+len(b) == 0 {
		return 1
	}
	return 2 * float64(len(lcs(a, b))) / float64(len(a)+len(b))
}

func lcs(a, b []string) []string {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var out []string
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}

// Synthesize clusters traces by tool-sequence similarity and drafts one skill
// per cluster backed by at least MinSupport distinct episodes. The steps are
// the subsequence every trace in the cluster shares. Drafts are keyed by what
// the tasks were about, so the same kind of task with a new procedure becomes
// a new version of the same skill. Several drafts may share an ID; they come
// sorted by ID, best supported first. It is deterministic and calls no model.
func Synthesize(traces []Trace) []Draft {
	type cluster struct {
		rep    []string
		traces []Trace
	}
	var cs []*cluster
	for _, tr := range traces {
		if len(tr.Tools) < MinSteps {
			continue
		}
		var home *cluster
		for _, c := range cs {
			if Similarity(tr.Tools, c.rep) >= Similar {
				home = c
				break
			}
		}
		if home == nil {
			home = &cluster{rep: tr.Tools}
			cs = append(cs, home)
		}
		home.traces = append(home.traces, tr)
	}
	var out []Draft
	for _, c := range cs {
		eps := map[string]bool{}
		var ids []string
		steps := c.traces[0].Tools
		keys := map[string]int{}
		var goals []string
		for _, tr := range c.traces {
			if !eps[tr.EpisodeID] {
				eps[tr.EpisodeID] = true
				ids = append(ids, tr.EpisodeID)
			}
			steps = lcs(steps, tr.Tools)
			keys[tr.Key]++
			goals = append(goals, tr.Goal)
		}
		if len(ids) < MinSupport || len(steps) < MinSteps {
			continue
		}
		key := topKey(keys)
		id := slug(key)
		if id == "" {
			id = slug("skill-" + steps[0])
		}
		out = append(out, Draft{ID: id, Name: key, Trigger: trigger(key, goals), Steps: steps, EpisodeIDs: ids})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return len(out[i].EpisodeIDs) > len(out[j].EpisodeIDs)
	})
	return out
}

func topKey(keys map[string]int) string {
	var best string
	for k, n := range keys {
		if n > keys[best] || (n == keys[best] && (best == "" || k < best)) {
			best = k
		}
	}
	return best
}

var (
	wordRE  = regexp.MustCompile(`[a-z0-9_]{3,}`)
	slugRE  = regexp.MustCompile(`[^a-z0-9]+`)
	stopSet = map[string]bool{"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
		"from": true, "into": true, "task": true, "session": true, "please": true, "can": true, "you": true}
)

// trigger is the task key plus the words the cluster's goals share most:
// a short description of when the skill applies, built from words rather
// than copied sentences.
func trigger(key string, goals []string) string {
	count := map[string]int{}
	for _, g := range goals {
		seen := map[string]bool{}
		for _, w := range wordRE.FindAllString(strings.ToLower(g), -1) {
			if !stopSet[w] && w != key && !seen[w] {
				seen[w] = true
				count[w]++
			}
		}
	}
	words := make([]string, 0, len(count))
	for w := range count {
		words = append(words, w)
	}
	sort.Slice(words, func(i, j int) bool {
		if count[words[i]] != count[words[j]] {
			return count[words[i]] > count[words[j]]
		}
		return words[i] < words[j]
	})
	if len(words) > 6 {
		words = words[:6]
	}
	if len(words) == 0 {
		return key
	}
	return key + ": " + strings.Join(words, " ")
}

func slug(s string) string {
	s = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 48 {
		s = strings.TrimRight(s[:48], "-")
	}
	return s
}
