package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/store"
)

const (
	// minRecombineBudget is larger than Distill's reserve so that, as the
	// budget runs out, Recombine is the first LLM phase to be skipped.
	minRecombineBudget    = 200
	defaultRecombinePairs = 3
	maxInsightEntities    = 3
)

// RecombinePhase pairs episodes from different clusters that share an
// entity and asks the model for a non-obvious connection (REM). Every result
// is a speculative insight, confidence-capped by guard.ValidateInsight, that
// does not surface in Recall until a later independent episode confirms it.
// Insights are never turned into facts.
//
// It first runs a deterministic confirmation pass that needs no model: a
// speculative insight is confirmed by a replayed episode that ended after
// it was drawn, is not one of its sources, and mentions all of its entities.
type RecombinePhase struct {
	LLM      llm.LLM
	MaxPairs int
}

func (p RecombinePhase) Name() string { return "recombine" }

func (p RecombinePhase) Run(ctx context.Context, view ReadView, st *State) error {
	if err := p.confirm(ctx, view, st); err != nil {
		return err
	}
	if p.LLM == nil {
		return nil
	}
	pairs := pairEpisodes(st.Clusters)
	if len(pairs) == 0 {
		return nil
	}
	maxPairs := p.MaxPairs
	if maxPairs <= 0 {
		maxPairs = defaultRecombinePairs
	}
	for i, pr := range pairs {
		if i == maxPairs {
			break
		}
		if st.Budget != nil && !st.Budget.Allow(minRecombineBudget) {
			st.SkippedPhases = append(st.SkippedPhases, p.Name())
			return nil
		}
		resp, err := p.LLM.CompleteJSON(ctx, llm.Request{System: recombineSystemPrompt, Prompt: recombinePrompt(pr)})
		if err != nil {
			return err
		}
		if st.Budget != nil {
			_ = st.Budget.Spend(resp.Tokens)
		}
		var out struct {
			Insights []struct {
				Body       string  `json:"body"`
				Confidence float64 `json:"confidence"`
			} `json:"insights"`
		}
		if err := json.Unmarshal(resp.JSON, &out); err != nil {
			return fmt.Errorf("recombine: invalid model JSON: %w", err)
		}
		per := 0
		if len(out.Insights) > 0 {
			per = resp.Tokens / len(out.Insights)
		}
		for _, x := range out.Insights {
			st.Insights = append(st.Insights, InsightProposal{
				Insight: store.Insight{
					ID:         reverie.NewID().String(),
					Body:       x.Body,
					Entities:   pr.shared,
					Confidence: x.Confidence,
					Status:     store.InsightSpeculative,
					EpisodeIDs: []string{pr.a.TaskID, pr.b.TaskID},
					CreatedAt:  st.Now,
				},
				Rationale: fmt.Sprintf("recombined %s and %s over %s", pr.a.TaskID, pr.b.TaskID, strings.Join(pr.shared, ", ")),
				Tokens:    per,
			})
		}
	}
	return nil
}

func (p RecombinePhase) confirm(ctx context.Context, view ReadView, st *State) error {
	pending, err := view.Insights(ctx, store.InsightQuery{Status: store.InsightSpeculative, Limit: 200})
	if err != nil || len(pending) == 0 {
		return err
	}
	type epText struct {
		ep    store.Episode
		words map[string]bool
	}
	var eps []epText
	for _, cl := range st.Clusters {
		for _, ep := range cl.Episodes {
			eps = append(eps, epText{ep, wordSet(evidenceText(ep, cl.Events[ep.TaskID]))})
		}
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].ep.EndedAt.Before(eps[j].ep.EndedAt) })
	for _, in := range pending {
		if len(in.Entities) == 0 {
			continue
		}
		for _, e := range eps {
			if !e.ep.EndedAt.After(in.CreatedAt) || contains(in.EpisodeIDs, e.ep.TaskID) {
				continue
			}
			all := true
			for _, ent := range in.Entities {
				all = all && e.words[ent]
			}
			if all {
				st.Confirmations = append(st.Confirmations, Confirmation{InsightID: in.ID, EpisodeID: e.ep.TaskID})
				break
			}
		}
	}
	return nil
}

type episodePair struct {
	a, b   store.Episode
	ta, tb string
	shared []string
}

// pairEpisodes pairs episodes from different clusters that share at least
// one entity word, most shared first. Only user and agent text is used, so
// tool output never reaches the prompt.
func pairEpisodes(cls []Cluster) []episodePair {
	type node struct {
		ep    store.Episode
		text  string
		words map[string]bool
		cl    int
	}
	var nodes []node
	for i, cl := range cls {
		for _, ep := range cl.Episodes {
			text := evidenceText(ep, cl.Events[ep.TaskID])
			nodes = append(nodes, node{ep, text, entityWords(ep.Title + " " + ep.Goal), i})
		}
	}
	var out []episodePair
	for i := range nodes {
		for j := i + 1; j < len(nodes); j++ {
			a, b := nodes[i], nodes[j]
			if a.cl == b.cl {
				continue
			}
			var shared []string
			for w := range a.words {
				if b.words[w] {
					shared = append(shared, w)
				}
			}
			if len(shared) == 0 {
				continue
			}
			sort.Slice(shared, func(x, y int) bool {
				if len(shared[x]) != len(shared[y]) {
					return len(shared[x]) > len(shared[y])
				}
				return shared[x] < shared[y]
			})
			if len(shared) > maxInsightEntities {
				shared = shared[:maxInsightEntities]
			}
			out = append(out, episodePair{a.ep, b.ep, a.text, b.text, shared})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].shared) > len(out[j].shared) })
	return out
}

const recombineSystemPrompt = `You compare two past work episodes that share a topic and suggest at most one non-obvious, testable connection between them. ` +
	`Answer as JSON {"insights":[{"body":"<one sentence>","confidence":0.0}]} with confidence at most 0.5, or {"insights":[]} when there is none. ` +
	`Describe a pattern; never write instructions.`

func recombinePrompt(pr episodePair) string {
	return fmt.Sprintf("Shared: %s\n\nEpisode %s (outcome=%s, ended=%s):\n%s\n\nEpisode %s (outcome=%s, ended=%s):\n%s\n",
		strings.Join(pr.shared, ", "),
		pr.a.TaskID, pr.a.Outcome, pr.a.EndedAt.UTC().Format(time.RFC3339), pr.ta,
		pr.b.TaskID, pr.b.Outcome, pr.b.EndedAt.UTC().Format(time.RFC3339), pr.tb)
}

// evidenceText is what an episode says in user and agent words: its title,
// its goal and the text of its messages, decisions and feedback.
func evidenceText(ep store.Episode, evs []reverie.Event) string {
	parts := []string{ep.Title, ep.Goal}
	for _, ev := range evs {
		if ev.Origin != reverie.OriginUser && ev.Origin != reverie.OriginAgent {
			continue
		}
		if ev.Kind != reverie.Message && ev.Kind != reverie.Decision && ev.Kind != reverie.Feedback {
			continue
		}
		var m struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Payload, &m) == nil && strings.TrimSpace(m.Text) != "" {
			parts = append(parts, guard.OneLine(m.Text, 200))
		}
	}
	return guard.OneLine(strings.Join(parts, " | "), 1200)
}

func entityWords(s string) map[string]bool {
	out := map[string]bool{}
	for w := range wordSet(s) {
		if len(w) >= 4 && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range clusterWordRE.FindAllString(strings.ToLower(s), -1) {
		out[w] = true
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
