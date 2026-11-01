package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/guard"
	"github.com/Dubjay18/reverie/internal/ontology"
	"github.com/Dubjay18/reverie/internal/store"
)

// ReconcileReflectPhase synthesises lesson facts from failure/partial episodes
// (using only user/agent-origin events as lesson text, never raw tool errors)
// and dry-runs facts.Assert against current live facts so invalid or duplicate
// proposals are reconciled before Validate and Commit.
type ReconcileReflectPhase struct {
	Ontology *ontology.Ontology
}

func (p ReconcileReflectPhase) Name() string { return "reconcile_reflect" }

func (p ReconcileReflectPhase) Run(ctx context.Context, view ReadView, st *State) error {
	onto := p.Ontology
	if onto == nil {
		onto = ontology.Default()
	}

	// 1. Reflect: failure and partial episodes yield a lesson fact if they have
	// user or agent evidence.
	for _, cl := range st.Clusters {
		for _, ep := range cl.Episodes {
			if ep.Outcome != store.Failure && ep.Outcome != store.Partial {
				continue
			}
			lesson, ok := buildLesson(ep, cl.Events[ep.TaskID])
			if !ok {
				continue
			}
			st.Proposals = append(st.Proposals, Proposal{
				Phase: "reflect",
				Fact: reverie.Fact{
					ID:         reverie.NewID().String(),
					Entity:     "project",
					EntityType: "project",
					Relation:   "lesson",
					Value:      lesson,
					Source:     reverie.SourceDistilled,
					Confidence: 0.75,
					ValidFrom:  ep.EndedAt.UTC(),
					EpisodeIDs: []string{ep.TaskID},
				},
				Rationale:  fmt.Sprintf("lesson from %s episode %s", ep.Outcome, ep.TaskID),
				AnchorTime: ep.EndedAt.UTC(),
			})
		}
	}

	// 2. Reconcile: normalise and dry-run Assert against existing live facts so
	// proposals that fail schema validation are dropped early.
	var kept []Proposal
	for _, prop := range st.Proposals {
		norm, _, err := facts.Normalize(prop.Fact, onto, st.Now)
		if err != nil {
			continue
		}
		live, err := view.LiveFacts(ctx, norm.Entity, norm.Relation)
		if err != nil {
			return err
		}
		if _, err := facts.Assert(live, norm, onto, st.Now); err != nil {
			continue
		}
		prop.Fact = norm
		kept = append(kept, prop)
	}
	st.Proposals = kept
	return nil
}

func buildLesson(ep store.Episode, evs []reverie.Event) (string, bool) {
	var notes []string
	hasUserOrAgent := false
	for _, ev := range evs {
		if ev.Origin != reverie.OriginUser && ev.Origin != reverie.OriginAgent {
			continue
		}
		hasUserOrAgent = true
		if ev.Kind == reverie.Decision || ev.Kind == reverie.Feedback || ev.Kind == reverie.Message {
			var m map[string]any
			if json.Unmarshal(ev.Payload, &m) == nil {
				if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
					notes = append(notes, guard.OneLine(s, 120))
				}
			}
		}
	}
	if !hasUserOrAgent {
		return "", false
	}
	summary := ep.Goal
	if summary == "" && len(notes) > 0 {
		summary = notes[len(notes)-1]
	}
	if summary == "" {
		summary = ep.Title
	}
	if summary == "" {
		summary = ep.TaskID
	}
	text := fmt.Sprintf("task %s (%s): %s", ep.TaskID, ep.Outcome, guard.OneLine(summary, 140))
	if guard.ContainsSecret(text) || guard.LooksLikeInstruction(text) {
		return "", false
	}
	return text, true
}
