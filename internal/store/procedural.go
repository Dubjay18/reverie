package store

import (
	"context"
	"time"
)

// Skill lifecycle states. A dream only ever creates candidates; real use
// promotes them (internal/skills).
type SkillStatus string

const (
	SkillCandidate  SkillStatus = "candidate"
	SkillActive     SkillStatus = "active"
	SkillDeprecated SkillStatus = "deprecated"
)

// Skill is one version of a learned procedure. Skills are shared by a
// (Tenant, Project); Agent narrows one to a single agent and is empty for a
// skill every agent in the project may use.
type Skill struct {
	ID         string // stable across versions, e.g. "deploy"
	Version    int    // 1, 2, ...
	Agent      string // "" = shared by the project's agents
	Name       string
	Trigger    string   // when to use it, in a few words
	Steps      []string // tool names in order
	Status     SkillStatus
	Uses       int // outcomes reported against this version
	Successes  int
	EpisodeIDs []string // provenance: the successful episodes it was drawn from
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastUsed   time.Time
}

type SkillQuery struct {
	Status SkillStatus // empty means any
	Limit  int
}

// SkillRepo stores skills for the handle's (Tenant, Project). A handle sees
// shared skills and those narrowed to its own Agent, never another agent's.
type SkillRepo interface {
	// Save upserts one (ID, Version). Agent must be empty or the handle's
	// agent (ErrScopeMismatch otherwise, including when the existing row
	// belongs to another agent).
	Save(ctx context.Context, s Skill) error
	// Get returns one version; version <= 0 means the latest. ErrNotFound if absent.
	Get(ctx context.Context, id string, version int) (Skill, error)
	// Versions returns every version of id, oldest first.
	Versions(ctx context.Context, id string) ([]Skill, error)
	// List returns skills, most recently updated first.
	List(ctx context.Context, q SkillQuery) ([]Skill, error)
}

// Insight states. An insight is a speculative connection drawn by Recombine;
// it surfaces in Recall only once confirmed, and is never turned into a fact.
type InsightStatus string

const (
	InsightSpeculative InsightStatus = "speculative"
	InsightConfirmed   InsightStatus = "confirmed"
	InsightRejected    InsightStatus = "rejected"
)

type Insight struct {
	ID          string
	Body        string
	Entities    []string // what the paired memories had in common
	Confidence  float64  // capped at guard.MaxInsightConfidence
	Status      InsightStatus
	EpisodeIDs  []string // the paired episodes it was drawn from
	ConfirmedBy []string // a later episode, or "user"
	CreatedAt   time.Time
	ConfirmedAt time.Time
}

type InsightQuery struct {
	Status InsightStatus // empty means any
	Limit  int
}

// InsightRepo stores insights for the handle's full scope.
type InsightRepo interface {
	Save(ctx context.Context, in Insight) error
	Get(ctx context.Context, id string) (Insight, error) // ErrNotFound if absent
	// List returns insights, newest first.
	List(ctx context.Context, q InsightQuery) ([]Insight, error)
}
