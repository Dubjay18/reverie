package reverie

import "time"

// FactSource says how much to trust a fact. The order matters: a more
// trusted source wins a conflict (see internal/facts).
type FactSource string

const (
	// SourceUser: the user said it themselves (the CLI, or a prompt the
	// hook saw typed by the user). Dreams never overwrite it.
	SourceUser FactSource = "user_stated"
	// SourceAgent: an agent recorded it on request through a tool. An agent
	// can be tricked, so this never earns SourceUser's protection.
	SourceAgent FactSource = "agent_stated"
	// SourceDistilled: extracted from episodes by a dream.
	SourceDistilled FactSource = "distilled"
	// SourceInferred: a guess drawn from distilled facts.
	SourceInferred FactSource = "inferred"
)

// Rank orders sources by trust; larger is more trusted. Unknown is 0.
func (s FactSource) Rank() int {
	switch s {
	case SourceUser:
		return 4
	case SourceAgent:
		return 3
	case SourceDistilled:
		return 2
	case SourceInferred:
		return 1
	}
	return 0
}

type FactStatus string

const (
	FactActive     FactStatus = "active"
	FactSuperseded FactStatus = "superseded"
	FactContested  FactStatus = "contested"
	FactTombstoned FactStatus = "tombstoned"
)

// Fact is one statement about the world with two clocks: ValidFrom/ValidTo
// is when it was true, RecordedAt is when Reverie learned it. Facts are
// never deleted; superseding closes ValidTo, forgetting tombstones.
type Fact struct {
	ID           string
	Entity       string // lower-cased, trimmed
	EntityType   string // optional
	Relation     string // lower-cased, trimmed; must exist in the ontology
	Value        string
	Confidence   float64
	Source       FactSource
	Status       FactStatus
	Pinned       bool // never changed or expired by a dream
	ValidFrom    time.Time
	ValidTo      time.Time // zero while still true
	RecordedAt   time.Time
	SupersededBy string
	EpisodeIDs   []string // provenance: the tasks this came from
}

// Live reports whether the fact is currently believed.
func (f Fact) Live() bool {
	return f.ValidTo.IsZero() && (f.Status == FactActive || f.Status == FactContested)
}

// FactInput is what a caller supplies to remember something. Entity and
// Relation default to "user" and "note"; Relation must be in the ontology.
type FactInput struct {
	Entity   string
	Relation string
	Value    string // required
	Source   FactSource
	TaskID   string    // provenance; defaults to "manual"
	ValidAt  time.Time // when it became true; defaults to now
}
