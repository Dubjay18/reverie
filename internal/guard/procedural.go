package guard

import (
	"errors"
	"regexp"
	"strings"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

var (
	ErrSkillNotCandidate   = errors.New("guard: a dream may only create a skill as an unused candidate")
	ErrSkillShape          = errors.New("guard: skill draft is malformed")
	ErrSkillSupport        = errors.New("guard: skill draft needs agent tool use in at least two distinct episodes")
	ErrInsightNotNew       = errors.New("guard: a dream may only create an insight as speculative and unconfirmed")
	ErrInsightShape        = errors.New("guard: insight is malformed")
	ErrInsightSupport      = errors.New("guard: insight needs two distinct episodes with real evidence")
	ErrInsightConfirmation = errors.New("guard: confirmation must come from a later, independent episode with user or agent evidence")
)

const (
	// MaxInsightConfidence caps every insight: they are speculative by nature.
	MaxInsightConfidence = 0.5
	MinSkillSupport      = 2
	MaxSkillSteps        = 20
	MaxInsightBody       = 400
)

var (
	skillIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	skillStepRE = regexp.MustCompile(`^[a-z0-9_.:-]{1,64}$`)
)

// ValidateSkill gates a skill drafted by a dream (ARCHITECTURE.md §9 applied
// to procedural memory): it must land as an unused candidate, every step is
// a plain tool name, its text carries no secret or instruction, and it is
// backed by agent tool use in at least MinSkillSupport distinct episodes
// that are not only recalled memory.
func ValidateSkill(s store.Skill, episodeOrigins map[string][]reverie.Origin) (store.Skill, error) {
	if s.Status != store.SkillCandidate || s.Uses != 0 || s.Successes != 0 {
		return s, ErrSkillNotCandidate
	}
	if !skillIDRE.MatchString(s.ID) || s.Version < 1 || len(s.Steps) < 2 || len(s.Steps) > MaxSkillSteps {
		return s, ErrSkillShape
	}
	for _, st := range s.Steps {
		if !skillStepRE.MatchString(st) {
			return s, ErrSkillShape
		}
	}
	text := s.Name + " " + s.Trigger + " " + strings.Join(s.Steps, " ")
	if ContainsSecret(text) {
		return s, ErrSecretValue
	}
	if LooksLikeInstruction(text) {
		return s, ErrInstructionValue
	}
	var eps []string
	seen := map[string]bool{}
	for _, id := range s.EpisodeIDs {
		if id == "" || seen[id] {
			continue
		}
		origins, ok := episodeOrigins[id]
		if !ok {
			return s, ErrUnknownProvenance
		}
		for _, o := range origins {
			if o == reverie.OriginAgent { // tool calls are the agent's decisions
				seen[id] = true
				eps = append(eps, id)
				break
			}
		}
	}
	if len(eps) < MinSkillSupport {
		return s, ErrSkillSupport
	}
	s.EpisodeIDs = eps
	s.Name, s.Trigger = OneLine(s.Name, 80), OneLine(s.Trigger, 160)
	return s, nil
}

// ValidateInsight gates an insight from Recombine: it is created speculative
// and unconfirmed, its confidence is capped at MaxInsightConfidence, it pairs
// at least two distinct episodes that each hold non-recalled evidence, and its
// body is short and carries no secret or instruction. Nothing here makes it a
// fact; insights live in their own table and never feed facts.
func ValidateInsight(in store.Insight, episodeOrigins map[string][]reverie.Origin) (store.Insight, error) {
	if in.Status != store.InsightSpeculative || len(in.ConfirmedBy) > 0 || !in.ConfirmedAt.IsZero() {
		return in, ErrInsightNotNew
	}
	in.Body = OneLine(in.Body, MaxInsightBody)
	if strings.TrimSpace(in.Body) == "" || in.ID == "" {
		return in, ErrInsightShape
	}
	if ContainsSecret(in.Body + " " + strings.Join(in.Entities, " ")) {
		return in, ErrSecretValue
	}
	if LooksLikeInstruction(in.Body) {
		return in, ErrInstructionValue
	}
	var eps []string
	seen := map[string]bool{}
	for _, id := range in.EpisodeIDs {
		if id == "" || seen[id] {
			continue
		}
		origins, ok := episodeOrigins[id]
		if !ok {
			return in, ErrUnknownProvenance
		}
		if hasEvidence(origins, false) {
			seen[id] = true
			eps = append(eps, id)
		}
	}
	if len(eps) < 2 {
		return in, ErrInsightSupport
	}
	in.EpisodeIDs = eps
	if in.Confidence > MaxInsightConfidence {
		in.Confidence = MaxInsightConfidence
	}
	if in.Confidence < 0 {
		in.Confidence = 0
	}
	return in, nil
}

// ValidateInsightConfirmation checks that episodeID may confirm in: the
// insight is still speculative, and the episode is not one it was drawn from
// and holds user or agent evidence (tool output alone cannot confirm).
func ValidateInsightConfirmation(in store.Insight, episodeID string, episodeOrigins map[string][]reverie.Origin) error {
	if in.Status != store.InsightSpeculative || episodeID == "" {
		return ErrInsightConfirmation
	}
	for _, id := range in.EpisodeIDs {
		if id == episodeID {
			return ErrInsightConfirmation
		}
	}
	origins, ok := episodeOrigins[episodeID]
	if !ok || !hasEvidence(origins, true) {
		return ErrInsightConfirmation
	}
	return nil
}

func hasEvidence(origins []reverie.Origin, userOrAgentOnly bool) bool {
	for _, o := range origins {
		switch o {
		case reverie.OriginUser, reverie.OriginAgent:
			return true
		case reverie.OriginTool:
			if !userOrAgentOnly {
				return true
			}
		}
	}
	return false
}
