package guard

import (
	"errors"
	"regexp"
	"strings"

	"github.com/Dubjay18/reverie"
)

var (
	ErrNoProvenance      = errors.New("guard: dream proposal has no provenance episode")
	ErrUnknownProvenance = errors.New("guard: provenance episode does not exist in scope")
	ErrRecalledOnly      = errors.New("guard: provenance consists only of recalled events")
	ErrImmutableTarget   = errors.New("guard: dreams cannot modify user_stated or pinned facts")
	ErrSourceEscalation  = errors.New("guard: dreams may only propose distilled or inferred facts")
	ErrInstructionValue  = errors.New("guard: proposed value reads like an instruction to a model")
	ErrSecretValue       = errors.New("guard: proposed value contains a secret")
	ErrToolOutputOnly    = errors.New("guard: fact whose only provenance is tool_output cannot assert instructions or secrets")
)

// MaxSingleEpisodeInferredConfidence caps an inferred fact backed by a single
// episode until a second independent episode corroborates it.
const MaxSingleEpisodeInferredConfidence = 0.6

// ValidateProposal applies the deterministic safety gate (ARCHITECTURE.md §9)
// to a proposed dream fact before it can be asserted or committed.
//
// episodeOrigins maps each episode ID in scope to the non-system Event.Origin
// values that occurred in that episode. existing are the currently live facts
// for the same (entity, relation).
//
// It returns the sanitised/capped Fact or an error rejecting the proposal.
func ValidateProposal(f reverie.Fact, existing []reverie.Fact, episodeOrigins map[string][]reverie.Origin) (reverie.Fact, error) {
	// Rule 5: dreams never mint user_stated or agent_stated, and never modify
	// a pinned or user_stated fact on a single-valued relation.
	if f.Source != reverie.SourceDistilled && f.Source != reverie.SourceInferred {
		return f, ErrSourceEscalation
	}
	for _, ex := range existing {
		if !ex.Live() {
			continue
		}
		if ex.Pinned || ex.Source == reverie.SourceUser {
			return f, ErrImmutableTarget
		}
	}

	// Rule 2: a fact needs at least one provenance EpisodeID, and every cited
	// episode must exist and carry non-recalled evidence (Rule 1 bootstrap guard).
	seenEp := map[string]bool{}
	var cleanEps []string
	hasUserOrAgent := false
	hasEvidence := false
	for _, id := range f.EpisodeIDs {
		id = strings.TrimSpace(id)
		if id == "" || seenEp[id] {
			continue
		}
		origins, ok := episodeOrigins[id]
		if !ok {
			return f, ErrUnknownProvenance
		}
		epHasEvidence := false
		for _, o := range origins {
			if o == reverie.OriginRecalled || o == reverie.OriginSystem {
				continue
			}
			epHasEvidence = true
			hasEvidence = true
			if o == reverie.OriginUser || o == reverie.OriginAgent {
				hasUserOrAgent = true
			}
		}
		if epHasEvidence {
			seenEp[id] = true
			cleanEps = append(cleanEps, id)
		}
	}
	if len(cleanEps) == 0 {
		if len(f.EpisodeIDs) > 0 {
			return f, ErrRecalledOnly
		}
		return f, ErrNoProvenance
	}
	if !hasEvidence {
		return f, ErrRecalledOnly
	}
	f.EpisodeIDs = cleanEps

	// Rule 3: secret patterns and imperative-instruction patterns are never
	// stored from a dream; and when the only provenance is tool_output, any
	// instruction-like or secret-bearing text is rejected with ErrToolOutputOnly.
	combined := f.Entity + " " + f.Relation + " " + f.Value
	hasSecret := ContainsSecret(combined) || sensitiveEntityOrRelation(f.Entity, f.Relation)
	hasInstr := LooksLikeInstruction(combined)
	if !hasUserOrAgent {
		// Provenance is exclusively tool_output.
		if hasSecret || hasInstr {
			return f, ErrToolOutputOnly
		}
	}
	if hasSecret {
		return f, ErrSecretValue
	}
	if hasInstr {
		return f, ErrInstructionValue
	}

	// Rule 4: single-episode inferred facts are capped at confidence 0.6 until
	// a second independent episode corroborates.
	if f.Source == reverie.SourceInferred && len(cleanEps) < 2 && f.Confidence > MaxSingleEpisodeInferredConfidence {
		f.Confidence = MaxSingleEpisodeInferredConfidence
	}
	return f, nil
}

// ContainsSecret reports whether s matches any secret redaction pattern or
// already contains the redaction marker from an earlier pass.
func ContainsSecret(s string) bool {
	if strings.Contains(s, Redacted) {
		return true
	}
	return Redact(s) != s
}

var secretNameRE = regexp.MustCompile(`(?i)\b(pass(?:word|wd)?|secret|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)\b`)

func sensitiveEntityOrRelation(entity, relation string) bool {
	return secretNameRE.MatchString(entity) || secretNameRE.MatchString(relation)
}

// ValidateForget applies the deterministic safety gate (ARCHITECTURE.md §9)
// to a Forget-phase proposal (TTL expiry, confidence decay, or compression
// tombstone) targeting an existing fact row.
//
// Pinned and user_stated facts are immutable to dreams; non-live facts cannot
// be tombstoned again.
func ValidateForget(target reverie.Fact) error {
	if !target.Live() {
		return errors.New("guard: cannot forget a non-live fact")
	}
	if target.Pinned || target.Source == reverie.SourceUser {
		return ErrImmutableTarget
	}
	return nil
}
