package dream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/facts"
	"github.com/Dubjay18/reverie/internal/ontology"
	"github.com/Dubjay18/reverie/internal/recall"
	"github.com/Dubjay18/reverie/internal/store"
)

func TestForgetPhaseTTLExpiryDecayCompressAndPinnedExempt(t *testing.T) {
	st := openTestStore(t)
	sc := st.For(testScope)
	ctx := context.Background()

	appendTask(t, sc, "seed:ep", d0, store.Success,
		reverie.NewEvent(testScope, "seed:ep", reverie.Message, reverie.OriginUser, []byte(`{"text":"status update"}`)),
	)

	// 1. Unpinned status fact (TTL 90d in default ontology), 100 days old -> expired.
	// 2. Pinned status fact, 100 days old -> exempt.
	// 3. Low-confidence inferred fact, 150 days old -> decayed below floor.
	// 4. 5 old notes -> compressed into 1 summary note, originals tombstoned.
	_ = sc.Tx(ctx, func(tx store.Tx) error {
		onto := ontology.Default()
		_, err := facts.Remember(ctx, tx, onto, reverie.Fact{
			ID: "status-expired", Entity: "jay", EntityType: "person", Relation: "status", Value: "on vacation",
			Source: reverie.SourceDistilled, Confidence: 0.9, ValidFrom: d0, EpisodeIDs: []string{"seed:ep"},
		}, d0, "r-seed-1")
		if err != nil {
			return err
		}
		_, err = facts.Remember(ctx, tx, onto, reverie.Fact{
			ID: "status-pinned", Entity: "ops", EntityType: "project", Relation: "status", Value: "freeze-window",
			Source: reverie.SourceDistilled, Confidence: 0.9, ValidFrom: d0, EpisodeIDs: []string{"seed:ep"},
		}, d0, "r-seed-2")
		if err != nil {
			return err
		}
		if err := facts.SetPinned(ctx, tx, "status-pinned", true, d0, "r-pin"); err != nil {
			return err
		}
		_, err = facts.Remember(ctx, tx, onto, reverie.Fact{
			ID: "inferred-stale", Entity: "jay", EntityType: "person", Relation: "prefers", Value: "light theme",
			Source: reverie.SourceInferred, Confidence: 0.4, ValidFrom: d0, EpisodeIDs: []string{"seed:ep"},
		}, d0, "r-seed-3")
		if err != nil {
			return err
		}
		for i := 0; i < 5; i++ {
			_, err := facts.Remember(ctx, tx, onto, reverie.Fact{
				ID: "note-" + string(rune('a'+i)), Entity: "jay", EntityType: "person", Relation: "note",
				Value:      "old note item " + string(rune('a'+i)),
				Source:     reverie.SourceDistilled,
				Confidence: 0.8,
				ValidFrom:  d0,
				EpisodeIDs: []string{"seed:ep"},
			}, d0, "r-seed-note")
			if err != nil {
				return err
			}
		}
		return nil
	})

	dreamNow := d0.Add(150 * 24 * time.Hour)

	// Even before a dream runs, Recall at dreamNow must not surface the expired unpinned status fact,
	// while the pinned status fact is still recalled.
	prePack, err := recall.Run(ctx, sc, recall.Query{Task: "status vacation freeze", Budget: 1500, Now: dreamNow})
	if err != nil {
		t.Fatal(err)
	}
	renderedPre := prePack.Render()
	if strings.Contains(renderedPre, "on vacation") {
		t.Fatalf("expired unpinned status fact was recalled before dream:\n%s", renderedPre)
	}
	if !strings.Contains(renderedPre, "freeze-window") {
		t.Fatalf("pinned status fact was not recalled:\n%s", renderedPre)
	}

	// Run dream at dreamNow: ForgetPhase tombstones status-expired, inferred-stale, and the 5 old notes,
	// and creates 1 compressed summary note.
	rep, err := Run(ctx, sc, Config{Now: dreamNow})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Forgotten != 7 || rep.Learned != 1 {
		t.Fatalf("want Forgotten=7 (1 ttl + 1 decay + 5 compressed) and Learned=1 (summary), got %+v", rep)
	}

	postPack, err := recall.Run(ctx, sc, recall.Query{Task: "status vacation freeze note", Budget: 1500, Now: dreamNow})
	if err != nil {
		t.Fatal(err)
	}
	renderedPost := postPack.Render()
	if strings.Contains(renderedPost, "on vacation") || strings.Contains(renderedPost, "light theme") {
		t.Fatalf("tombstoned expired/decayed fact was recalled:\n%s", renderedPost)
	}
	if !strings.Contains(renderedPost, "freeze-window") {
		t.Fatalf("pinned fact must remain recalled after dream:\n%s", renderedPost)
	}
	if !strings.Contains(renderedPost, "summary:") {
		t.Fatalf("compressed summary note was not recalled:\n%s", renderedPost)
	}

	// Reverting the dream restores the tombstoned facts back to active.
	_ = sc.Tx(ctx, func(tx store.Tx) error {
		_, err := facts.RevertRun(ctx, tx, rep.RunID, dreamNow.Add(time.Hour))
		return err
	})
	var restored reverie.Fact
	_ = sc.View(ctx, func(tx store.Tx) error {
		restored, err = tx.Facts().Get(ctx, "status-expired")
		return err
	})
	if err != nil || restored.Status != reverie.FactActive {
		t.Fatalf("expected status-expired to be active after reverting dream, got %+v (%v)", restored, err)
	}
}

func TestForgetPhaseDoesNotInventProvenanceForGhostEpisodes(t *testing.T) {
	st := openTestStore(t)
	sc := st.For(testScope)
	ctx := context.Background()

	// Seed 5 distilled jay/note facts citing "ghost:1" (an episode with no events in the store),
	// with ValidFrom/RecordedAt 40 days before dreamNow.
	seedAt := d0
	dreamNow := d0.Add(40 * 24 * time.Hour)
	err := sc.Tx(ctx, func(tx store.Tx) error {
		onto := ontology.Default()
		for i := 0; i < 5; i++ {
			id := "ghost-note-" + string(rune('a'+i))
			_, err := facts.Remember(ctx, tx, onto, reverie.Fact{
				ID:         id,
				Entity:     "jay",
				EntityType: "person",
				Relation:   "note",
				Value:      "ghost note " + string(rune('a'+i)),
				Source:     reverie.SourceDistilled,
				Confidence: 0.8,
				ValidFrom:  seedAt,
				EpisodeIDs: []string{"ghost:1"},
			}, seedAt, "r-ghost-seed")
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed ghost notes: %v", err)
	}

	// Verify directly on ForgetPhase.Run that "ghost:1" is not inserted into EpisodeOrigins.
	state := &State{
		Now:            dreamNow,
		Budget:         NewBudget(DefaultDreamBudget),
		EpisodeOrigins: map[string][]reverie.Origin{},
	}
	if err := (ForgetPhase{Ontology: ontology.Default()}).Run(ctx, NewReadView(sc), state); err != nil {
		t.Fatalf("ForgetPhase.Run: %v", err)
	}
	if origins, ok := state.EpisodeOrigins["ghost:1"]; ok {
		t.Fatalf("expected ghost:1 to be absent from EpisodeOrigins, got %v", origins)
	}

	// Run dream with only ForgetPhase: the compression summary proposal must be
	// rejected by ValidateProposal (ErrUnknownProvenance), so Learned=0 and no
	// live summary fact is stored.
	rep, err := Run(ctx, sc, Config{
		Now:    dreamNow,
		Phases: []Phase{ForgetPhase{Ontology: ontology.Default()}},
	})
	if err != nil {
		t.Fatalf("Run with ForgetPhase: %v", err)
	}
	if rep.Learned != 0 {
		t.Fatalf("expected Learned=0 when provenance episode has no events, got %+v", rep)
	}
	if rep.Forgotten != 5 || rep.Rejected != 1 {
		t.Fatalf("expected Forgotten=5 and Rejected=1, got %+v", rep)
	}

	var live []reverie.Fact
	_ = sc.View(ctx, func(tx store.Tx) error {
		var err error
		live, err = tx.Facts().Live(ctx, "jay", "note")
		return err
	})
	for _, f := range live {
		if strings.HasPrefix(f.Value, "summary:") {
			t.Fatalf("unexpected live summary fact committed from ghost episode: %+v", f)
		}
	}
	if len(live) != 0 {
		t.Fatalf("expected 0 live jay/note facts, got %+v", live)
	}
}
