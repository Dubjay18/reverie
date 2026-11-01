package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/ontology"
)

const minDistillBudget = 50

// DistillPhase extracts structured facts from replayed episode clusters against
// the active ontology and normalises relative time expressions ("yesterday",
// "today", "tomorrow", "last week") to ISO UTC dates anchored to the event's At
// timestamp. When the token budget is exhausted, Distill is the first phase to
// be skipped.
type DistillPhase struct {
	LLM      llm.LLM
	Ontology *ontology.Ontology
}

func (p DistillPhase) Name() string { return "distill" }

type distilledFact struct {
	Entity     string   `json:"entity"`
	EntityType string   `json:"entity_type"`
	Relation   string   `json:"relation"`
	Value      string   `json:"value"`
	Source     string   `json:"source"` // "distilled" (default) or "inferred"
	Confidence float64  `json:"confidence"`
	ValidFrom  string   `json:"valid_from"`
	Rationale  string   `json:"rationale"`
	EpisodeIDs []string `json:"episode_ids"`
}

func (p DistillPhase) Run(ctx context.Context, _ ReadView, st *State) error {
	if p.LLM == nil || len(st.Clusters) == 0 {
		return nil
	}
	if st.Budget != nil && !st.Budget.Allow(minDistillBudget) {
		st.SkippedPhases = append(st.SkippedPhases, p.Name())
		return nil
	}

	sys := distillSystemPrompt(p.Ontology)
	for _, cl := range st.Clusters {
		if st.Budget != nil && !st.Budget.Allow(minDistillBudget) {
			st.SkippedPhases = append(st.SkippedPhases, p.Name())
			return nil
		}
		prompt, anchor, defaultEps := buildClusterPrompt(cl)
		if len(defaultEps) == 0 {
			continue
		}
		resp, err := p.LLM.CompleteJSON(ctx, llm.Request{System: sys, Prompt: prompt})
		if err != nil {
			return err
		}
		if st.Budget != nil {
			_ = st.Budget.Spend(resp.Tokens)
		}
		var out struct {
			Facts []distilledFact `json:"facts"`
		}
		if err := json.Unmarshal(resp.JSON, &out); err != nil {
			return fmt.Errorf("distill: invalid model JSON: %w", err)
		}
		perFactTokens := 0
		if len(out.Facts) > 0 {
			perFactTokens = resp.Tokens / len(out.Facts)
		}
		for _, df := range out.Facts {
			src := reverie.SourceDistilled
			if strings.EqualFold(df.Source, string(reverie.SourceInferred)) {
				src = reverie.SourceInferred
			}
			eps := df.EpisodeIDs
			if len(eps) == 0 {
				eps = defaultEps
			}
			val, validAt := NormalizeRelativeTime(df.Value, df.ValidFrom, anchor)
			st.Proposals = append(st.Proposals, Proposal{
				Phase: p.Name(),
				Fact: reverie.Fact{
					ID:         reverie.NewID().String(),
					Entity:     df.Entity,
					EntityType: df.EntityType,
					Relation:   df.Relation,
					Value:      val,
					Confidence: df.Confidence,
					Source:     src,
					ValidFrom:  validAt,
					EpisodeIDs: eps,
				},
				Rationale:  firstNonEmpty(df.Rationale, "distilled from episode cluster "+cl.Key),
				Tokens:     perFactTokens,
				AnchorTime: anchor,
			})
		}
	}
	return nil
}

func distillSystemPrompt(onto *ontology.Ontology) string {
	if onto == nil {
		onto = ontology.Default()
	}
	var rels []string
	for _, name := range onto.Relations() {
		r, _ := onto.Relation(name)
		rels = append(rels, fmt.Sprintf("%s (%s)", name, r.Cardinality))
	}
	return `Extract durable facts as JSON {"facts":[{"entity":...,"relation":...,"value":...,"source":"distilled|inferred","confidence":0.8,"valid_from":...,"episode_ids":[...]}]}. Allowed relations: ` + strings.Join(rels, ", ")
}

func buildClusterPrompt(cl Cluster) (string, time.Time, []string) {
	var b strings.Builder
	var anchor time.Time
	var eps []string
	for _, ep := range cl.Episodes {
		evs := cl.Events[ep.TaskID]
		if len(evs) == 0 {
			continue
		}
		eps = append(eps, ep.TaskID)
		fmt.Fprintf(&b, "Episode %s (outcome=%s, ended=%s):\n", ep.TaskID, ep.Outcome, ep.EndedAt.UTC().Format(time.RFC3339))
		for _, ev := range evs {
			if ev.At.After(anchor) {
				anchor = ev.At.UTC()
			}
			fmt.Fprintf(&b, "  [%s %s @%s] %s\n", ev.Kind, ev.Origin, ev.At.UTC().Format(time.RFC3339), string(ev.Payload))
		}
	}
	if anchor.IsZero() && len(cl.Episodes) > 0 {
		anchor = cl.Episodes[0].EndedAt.UTC()
	}
	return b.String(), anchor, eps
}

var (
	yesterdayRE = regexp.MustCompile(`(?i)\byesterday\b`)
	todayRE     = regexp.MustCompile(`(?i)\btoday\b`)
	tomorrowRE  = regexp.MustCompile(`(?i)\btomorrow\b`)
	lastWeekRE  = regexp.MustCompile(`(?i)\blast\s+week\b`)
)

// NormalizeRelativeTime rewrites relative time words ("yesterday", "today",
// "tomorrow", "last week") in value and validFrom into ISO UTC dates (YYYY-MM-DD)
// anchored to anchor.
func NormalizeRelativeTime(value, validFrom string, anchor time.Time) (string, time.Time) {
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}
	anchor = anchor.UTC()
	yesterday := anchor.AddDate(0, 0, -1).Format("2006-01-02")
	today := anchor.Format("2006-01-02")
	tomorrow := anchor.AddDate(0, 0, 1).Format("2006-01-02")
	lastWeek := anchor.AddDate(0, 0, -7).Format("2006-01-02")

	validAt := anchor
	vf := strings.TrimSpace(strings.ToLower(validFrom))
	switch {
	case vf == "yesterday" || yesterdayRE.MatchString(value):
		validAt = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	case vf == "tomorrow" || tomorrowRE.MatchString(value):
		validAt = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	case vf == "last week" || lastWeekRE.MatchString(value):
		validAt = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -7)
	case vf == "today":
		validAt = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
	case vf != "":
		if t, err := time.Parse(time.RFC3339, validFrom); err == nil {
			validAt = t.UTC()
		} else if t, err := time.Parse("2006-01-02", validFrom); err == nil {
			validAt = t.UTC()
		}
	}

	value = yesterdayRE.ReplaceAllString(value, yesterday)
	value = todayRE.ReplaceAllString(value, today)
	value = tomorrowRE.ReplaceAllString(value, tomorrow)
	value = lastWeekRE.ReplaceAllString(value, lastWeek)
	return value, validAt
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
