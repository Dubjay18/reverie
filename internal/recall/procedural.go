package recall

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
)

const (
	kindSkill     = "skill"
	kindInsight   = "insight"
	proceduralCap = 50 // skills or insights considered per recall
)

// overlap is the share of the query's words that text contains.
func overlap(query map[string]bool, text string) float64 {
	if len(query) == 0 {
		return 0
	}
	hit := 0
	for w := range words(text) {
		if query[w] {
			hit++
		}
	}
	return float64(hit) / float64(len(query))
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(strings.ReplaceAll(s, "_", " ")), -1) {
		out[w] = true
	}
	return out
}

// gatherSkills offers active skills that match the task, and candidates
// that match it labelled as a proposed method: a candidate is promoted only
// by outcomes reported while it is on offer. With no task, only active
// skills are offered.
func gatherSkills(ctx context.Context, tx store.Tx, q Query, out map[string][]candidate) error {
	qw := words(q.Task)
	for _, status := range []store.SkillStatus{store.SkillActive, store.SkillCandidate} {
		list, err := tx.Skills().List(ctx, store.SkillQuery{Status: status, Limit: proceduralCap})
		if err != nil {
			return err
		}
		for _, sk := range list {
			if !q.AsOf.IsZero() && sk.CreatedAt.After(q.AsOf) {
				continue
			}
			rel := overlap(qw, sk.Name+" "+sk.Trigger+" "+strings.Join(sk.Steps, " "))
			if rel == 0 && (q.Task != "" || status != store.SkillActive) {
				continue
			}
			score := wRelevance*rel + (1-wRelevance)*skills.Rate(sk)
			if status == store.SkillCandidate {
				score /= 2 // never ahead of a proven skill
			}
			out[kindSkill] = append(out[kindSkill], candidate{item: skillItem(sk), score: score, at: sk.UpdatedAt})
		}
	}
	return nil
}

func skillItem(sk store.Skill) reverie.PackItem {
	text := guard.OneLine(sk.Name, 60)
	if sk.Trigger != "" && sk.Trigger != sk.Name {
		text += " — for " + strconv.Quote(guard.OneLine(sk.Trigger, 120))
	}
	text += ": " + strings.Join(sk.Steps, " → ")
	if sk.Status == store.SkillActive {
		text += fmt.Sprintf(" [skill; success rate %.2f over %d uses; report the outcome with its ref]", skills.Rate(sk), sk.Uses)
	} else {
		text += fmt.Sprintf(" [proposed method, unverified candidate: %d/%d successful uses so far; report the outcome with its ref]", sk.Successes, skills.PromoteAfter)
	}
	return reverie.PackItem{ID: skills.TargetID(sk.ID, sk.Version), Kind: kindSkill, Cite: skills.Ref(sk), Text: text}
}

// gatherInsights offers confirmed insights only. A speculative insight never
// reaches a pack, and an insight is always labelled as one, never as a fact.
func gatherInsights(ctx context.Context, tx store.Tx, q Query, out map[string][]candidate) error {
	list, err := tx.Insights().List(ctx, store.InsightQuery{Status: store.InsightConfirmed, Limit: proceduralCap})
	if err != nil {
		return err
	}
	qw := words(q.Task)
	for _, in := range list {
		if !q.AsOf.IsZero() && in.ConfirmedAt.After(q.AsOf) {
			continue
		}
		rel := overlap(qw, in.Body+" "+strings.Join(in.Entities, " "))
		if rel == 0 && q.Task != "" {
			continue
		}
		out[kindInsight] = append(out[kindInsight], candidate{item: insightItem(in), score: wRelevance*rel + (1-wRelevance)*in.Confidence, at: in.ConfirmedAt})
	}
	return nil
}

func insightItem(in store.Insight) reverie.PackItem {
	text := strconv.Quote(guard.OneLine(in.Body, 240)) +
		fmt.Sprintf(" [speculative insight, not a fact; confidence %.2f; drawn from %s; confirmed by %s]",
			in.Confidence, strings.Join(in.EpisodeIDs, " + "), strings.Join(in.ConfirmedBy, ", "))
	return reverie.PackItem{ID: in.ID, Kind: kindInsight, Cite: "insight:" + in.ID, Text: text}
}
