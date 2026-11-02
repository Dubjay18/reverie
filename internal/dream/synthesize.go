package dream

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
)

const defaultSynthesizeLookback = 50

// SynthesizePhase drafts candidate skills from successful episodes that
// share a tool sequence (internal/skills.Synthesize). It looks at this run's
// replayed episodes plus up to Lookback recent closed ones, so a procedure
// repeated across dreams is still noticed. It calls no model, so the budget
// never skips it. A draft whose steps match an existing version of the same
// skill is dropped; a draft with new steps becomes the next version. Every
// draft lands as a candidate; only reported use promotes it.
type SynthesizePhase struct {
	Lookback int
}

func (p SynthesizePhase) Name() string { return "synthesize" }

func (p SynthesizePhase) Run(ctx context.Context, view ReadView, st *State) error {
	lookback := p.Lookback
	if lookback <= 0 {
		lookback = defaultSynthesizeLookback
	}
	if st.EpisodeOrigins == nil {
		st.EpisodeOrigins = map[string][]reverie.Origin{}
	}
	var traces []skills.Trace
	seen := map[string]bool{}
	for _, cl := range st.Clusters {
		for _, ep := range cl.Episodes {
			seen[ep.TaskID] = true
			if ep.Outcome == store.Success {
				traces = append(traces, skills.Trace{EpisodeID: ep.TaskID, Key: cl.Key, Goal: ep.Goal, Tools: skills.ToolSequence(cl.Events[ep.TaskID])})
			}
		}
	}
	recent, err := view.Recent(ctx, lookback)
	if err != nil {
		return err
	}
	for _, ep := range recent {
		if seen[ep.TaskID] || ep.Outcome != store.Success || ep.ToolCalls < skills.MinSteps {
			continue
		}
		evs, err := view.Events(ctx, store.EventQuery{TaskID: ep.TaskID, From: ep.FirstEvent, To: ep.LastEvent})
		if err != nil {
			return err
		}
		// Record provenance the same way Replay does, so Validate can check it.
		origins := make([]reverie.Origin, 0, len(evs))
		for _, ev := range evs {
			origins = append(origins, ev.Origin)
		}
		st.EpisodeOrigins[ep.TaskID] = origins
		traces = append(traces, skills.Trace{EpisodeID: ep.TaskID, Key: clusterKey(ep), Goal: ep.Goal, Tools: skills.ToolSequence(evs)})
	}

	drafted := map[string]bool{}
	for _, d := range skills.Synthesize(traces) {
		if drafted[d.ID] { // one new version per skill per run: the best supported
			continue
		}
		vs, err := view.SkillVersions(ctx, d.ID)
		if err != nil {
			return err
		}
		next, known := 1, false
		for _, v := range vs {
			next = max(next, v.Version+1)
			known = known || slices.Equal(v.Steps, d.Steps)
		}
		if known {
			continue
		}
		drafted[d.ID] = true
		st.Skills = append(st.Skills, SkillProposal{
			Skill: store.Skill{
				ID: d.ID, Version: next, Name: d.Name, Trigger: d.Trigger, Steps: d.Steps,
				Status: store.SkillCandidate, EpisodeIDs: d.EpisodeIDs, CreatedAt: st.Now, UpdatedAt: st.Now,
			},
			Rationale: fmt.Sprintf("%d successful episodes share the tool sequence %s", len(d.EpisodeIDs), strings.Join(d.Steps, " -> ")),
		})
	}
	return nil
}
