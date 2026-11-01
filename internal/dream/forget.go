package dream

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/ontology"
	"github.com/Dubjay18/reverie/internal/store"
)

const (
	DecayHalfLife          = 60 * 24 * time.Hour
	DecayFloor             = 0.25
	CompressGroupThreshold = 5
	CompressAgeThreshold   = 30 * 24 * time.Hour
)

// ForgetPhase enforces ontology TTLs, confidence decay, and old-fact summary
// compression. It proposes only tombstones (and summary facts backed by
// existing provenance) and never touches pinned or user_stated facts.
type ForgetPhase struct {
	Ontology *ontology.Ontology
}

func (ForgetPhase) Name() string { return "forget" }

func (p ForgetPhase) Run(ctx context.Context, view ReadView, st *State) error {
	onto := p.Ontology
	if onto == nil {
		onto = ontology.Default()
	}
	factsList, err := view.QueryFacts(ctx, store.FactQuery{Limit: store.MaxLimit})
	if err != nil {
		return err
	}
	// Ensure provenance episodes of live facts are registered in EpisodeOrigins
	// if we need to propose a compressed summary fact.
	for _, f := range factsList {
		for _, epID := range f.EpisodeIDs {
			if _, ok := st.EpisodeOrigins[epID]; !ok {
				evs, err := view.Events(ctx, store.EventQuery{TaskID: epID, Limit: 50})
				if err == nil && len(evs) > 0 {
					for _, ev := range evs {
						st.EpisodeOrigins[epID] = append(st.EpisodeOrigins[epID], ev.Origin)
					}
				}
			}
		}
	}

	forgotten := map[string]bool{}
	now := st.Now

	// 1. TTL enforcement and 2. Confidence decay.
	for _, f := range factsList {
		if !f.Live() || f.Pinned || f.Source == reverie.SourceUser {
			continue
		}
		refTime := f.ValidFrom
		if f.RecordedAt.After(refTime) {
			refTime = f.RecordedAt
		}
		if refTime.IsZero() {
			refTime = now
		}
		age := now.Sub(refTime)
		if age < 0 {
			age = 0
		}

		if rel, err := onto.Relation(f.Relation); err == nil && rel.TTLDays > 0 {
			ttl := time.Duration(rel.TTLDays) * 24 * time.Hour
			if age >= ttl {
				st.Proposals = append(st.Proposals, Proposal{
					Phase:        "forget",
					Op:           "forget",
					TargetFactID: f.ID,
					Fact:         f,
					Rationale:    fmt.Sprintf("ttl expired (%d days)", rel.TTLDays),
				})
				forgotten[f.ID] = true
				continue
			}
		}

		// Confidence decay for uncorroborated inferred/distilled facts (excluding
		// many-cardinality groups eligible for compression below).
		if f.Source == reverie.SourceInferred || (f.Source == reverie.SourceDistilled && f.Confidence < 0.65) {
			effective := f.Confidence * math.Pow(0.5, float64(age)/float64(DecayHalfLife))
			if effective < DecayFloor {
				st.Proposals = append(st.Proposals, Proposal{
					Phase:        "forget",
					Op:           "forget",
					TargetFactID: f.ID,
					Fact:         f,
					Rationale:    fmt.Sprintf("decayed below %.2f (effective %.2f)", DecayFloor, effective),
				})
				forgotten[f.ID] = true
				continue
			}
		}
	}

	// 3. Compress old many-cardinality fact groups (>= CompressGroupThreshold facts older than CompressAgeThreshold).
	type groupKey struct{ entity, entityType, relation string }
	groups := map[groupKey][]reverie.Fact{}
	for _, f := range factsList {
		if !f.Live() || f.Pinned || f.Source == reverie.SourceUser || forgotten[f.ID] {
			continue
		}
		rel, err := onto.Relation(f.Relation)
		if err != nil || rel.Cardinality != ontology.Many {
			continue
		}
		refTime := f.RecordedAt
		if refTime.IsZero() {
			refTime = f.ValidFrom
		}
		if !refTime.IsZero() && now.Sub(refTime) >= CompressAgeThreshold {
			k := groupKey{f.Entity, f.EntityType, f.Relation}
			groups[k] = append(groups[k], f)
		}
	}

	for k, g := range groups {
		if len(g) < CompressGroupThreshold {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].ID < g[j].ID })
		var vals []string
		var eps []string
		seenEp := map[string]bool{}
		for _, f := range g {
			vals = append(vals, f.Value)
			for _, ep := range f.EpisodeIDs {
				if !seenEp[ep] {
					seenEp[ep] = true
					eps = append(eps, ep)
				}
			}
			st.Proposals = append(st.Proposals, Proposal{
				Phase:        "forget",
				Op:           "forget",
				TargetFactID: f.ID,
				Fact:         f,
				Rationale:    fmt.Sprintf("compressed into summary of %d %s facts", len(g), k.relation),
			})
		}
		summaryVal := "summary: " + strings.Join(vals, "; ")
		if len(summaryVal) > 220 {
			summaryVal = summaryVal[:217] + "..."
		}
		st.Proposals = append(st.Proposals, Proposal{
			Phase: "forget",
			Fact: reverie.Fact{
				ID:         reverie.NewID().String(),
				Entity:     k.entity,
				EntityType: k.entityType,
				Relation:   k.relation,
				Value:      summaryVal,
				Confidence: 0.75,
				Source:     reverie.SourceDistilled,
				ValidFrom:  now,
				EpisodeIDs: eps,
			},
			Rationale: fmt.Sprintf("compressed %d old %s facts", len(g), k.relation),
		})
	}
	return nil
}
