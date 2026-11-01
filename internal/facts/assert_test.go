package facts

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/ontology"
)

var (
	onto = ontology.Default()
	t0   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

func day(n int) time.Time { return t0.AddDate(0, 0, n) }

var seq int

func fact(entity, rel, value string, src reverie.FactSource, at time.Time) reverie.Fact {
	seq++
	return reverie.Fact{ID: fmt.Sprintf("f%03d", seq), Entity: entity, Relation: rel, Value: value, Source: src,
		ValidFrom: at, Status: reverie.FactActive, EpisodeIDs: []string{fmt.Sprintf("ep%d", seq)}}
}

func live(f reverie.Fact) reverie.Fact { f.RecordedAt = t0; f.Confidence = 1; return f }

func mustAssert(t *testing.T, existing []reverie.Fact, in reverie.Fact) Resolution {
	t.Helper()
	r, err := Assert(existing, in, onto, day(100))
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	return r
}

func TestMovedHouseSupersedes(t *testing.T) {
	lagos := live(fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(0)))
	r := mustAssert(t, []reverie.Fact{lagos}, fact("jay", "lives_in", "London", reverie.SourceUser, day(10)))
	if r.Op != OpSupersede || len(r.Updates) != 1 {
		t.Fatalf("%+v", r)
	}
	old := r.Updates[0]
	if old.Value != "Lagos" || old.Status != reverie.FactSuperseded || !old.ValidTo.Equal(day(10)) || old.SupersededBy != r.Fact.ID {
		t.Fatalf("old fact not closed correctly: %+v", old)
	}
	if r.Fact.Status != reverie.FactActive || !r.Fact.ValidTo.IsZero() {
		t.Fatalf("new fact should be open and active: %+v", r.Fact)
	}
}

func TestManyCardinalityNeverConflicts(t *testing.T) {
	goLang := live(fact("jay", "uses", "Go", reverie.SourceUser, day(0)))
	r := mustAssert(t, []reverie.Fact{goLang}, fact("jay", "uses", "TypeScript", reverie.SourceDistilled, day(5)))
	if r.Op != OpInsert || len(r.Updates) != 0 || r.Fact.Status != reverie.FactActive {
		t.Fatalf("%+v", r)
	}
}

func TestSameStatementMergesAndRaisesConfidence(t *testing.T) {
	a := live(fact("jay", "uses", "Go", reverie.SourceDistilled, day(0)))
	a.Confidence = 0.6
	in := fact("jay", "uses", " go ", reverie.SourceDistilled, day(3)) // case and padding differ
	in.Confidence = 0.6
	r := mustAssert(t, []reverie.Fact{a}, in)
	if r.Op != OpMerge || len(r.Updates) != 1 {
		t.Fatalf("%+v", r)
	}
	m := r.Updates[0]
	if m.ID != a.ID || m.Confidence <= 0.6 || m.Confidence >= 1 || len(m.EpisodeIDs) != 2 {
		t.Fatalf("merge should raise confidence (got %v) and add provenance (%v)", m.Confidence, m.EpisodeIDs)
	}
}

func TestMergeUpgradesSourceButNeverDowngrades(t *testing.T) {
	d := live(fact("jay", "uses", "Go", reverie.SourceDistilled, day(0)))
	r := mustAssert(t, []reverie.Fact{d}, fact("jay", "uses", "Go", reverie.SourceUser, day(1)))
	if r.Updates[0].Source != reverie.SourceUser {
		t.Fatalf("user restating a distilled fact should upgrade it: %+v", r.Updates[0])
	}
	u := live(fact("jay", "uses", "Go", reverie.SourceUser, day(0)))
	r = mustAssert(t, []reverie.Fact{u}, fact("jay", "uses", "Go", reverie.SourceInferred, day(1)))
	if r.Updates[0].Source != reverie.SourceUser {
		t.Fatalf("a guess must not downgrade a user statement: %+v", r.Updates[0])
	}
}

func TestUserBeatsDistilled(t *testing.T) {
	u := live(fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(0)))
	r := mustAssert(t, []reverie.Fact{u}, fact("jay", "lives_in", "Abuja", reverie.SourceDistilled, day(50)))
	if r.Op != OpReject {
		t.Fatalf("a newer distilled value must not replace a user statement: %+v", r)
	}
}

func TestLessTrustedDisagreementIsContestedOnBothSides(t *testing.T) {
	agent := live(fact("jay", "role", "engineer", reverie.SourceAgent, day(0)))
	r := mustAssert(t, []reverie.Fact{agent}, fact("jay", "role", "manager", reverie.SourceInferred, day(5)))
	if r.Op != OpContest || r.Fact.Status != reverie.FactContested || len(r.Updates) != 1 || r.Updates[0].Status != reverie.FactContested {
		t.Fatalf("both sides should be contested: %+v", r)
	}
}

func TestEqualTrustNewerWins(t *testing.T) {
	a := live(fact("jay", "role", "engineer", reverie.SourceDistilled, day(0)))
	r := mustAssert(t, []reverie.Fact{a}, fact("jay", "role", "lead", reverie.SourceDistilled, day(9)))
	if r.Op != OpSupersede {
		t.Fatalf("%+v", r)
	}
}

func TestHigherTrustSupersedesLowerEvenIfOlderStatement(t *testing.T) {
	d := live(fact("jay", "role", "engineer", reverie.SourceDistilled, day(0)))
	r := mustAssert(t, []reverie.Fact{d}, fact("jay", "role", "lead", reverie.SourceAgent, day(9)))
	if r.Op != OpSupersede {
		t.Fatalf("%+v", r)
	}
}

func TestOlderTruthIsStoredAsHistory(t *testing.T) {
	cur := live(fact("jay", "lives_in", "London", reverie.SourceUser, day(10)))
	r := mustAssert(t, []reverie.Fact{cur}, fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(0)))
	if r.Op != OpHistorical || r.Fact.Status != reverie.FactSuperseded || !r.Fact.ValidTo.Equal(day(10)) || r.Fact.SupersededBy != cur.ID || len(r.Updates) != 0 {
		t.Fatalf("a backfilled past value must not disturb the current one: %+v", r)
	}
}

func TestPinnedFactsAreImmune(t *testing.T) {
	p := live(fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(0)))
	p.Pinned = true
	if r := mustAssert(t, []reverie.Fact{p}, fact("jay", "lives_in", "Abuja", reverie.SourceDistilled, day(5))); r.Op != OpReject {
		t.Fatalf("dream vs pinned: %+v", r)
	}
	r := mustAssert(t, []reverie.Fact{p}, fact("jay", "lives_in", "London", reverie.SourceUser, day(5)))
	if r.Op != OpContest || len(r.Updates) != 0 || r.Fact.Status != reverie.FactContested {
		t.Fatalf("user vs pinned must contest without touching the pin: %+v", r)
	}
}

func TestUserResolvesAContest(t *testing.T) {
	a := live(fact("jay", "role", "engineer", reverie.SourceAgent, day(0)))
	a.Status = reverie.FactContested
	b := live(fact("jay", "role", "manager", reverie.SourceInferred, day(2)))
	b.Status = reverie.FactContested
	r := mustAssert(t, []reverie.Fact{a, b}, fact("jay", "role", "director", reverie.SourceUser, day(5)))
	if r.Op != OpSupersede || len(r.Updates) != 2 {
		t.Fatalf("the user's word should close both contested facts: %+v", r)
	}
}

func TestOtherEntitiesAndRelationsAreIgnored(t *testing.T) {
	other := live(fact("sam", "lives_in", "Paris", reverie.SourceUser, day(0)))
	rel := live(fact("jay", "works_at", "Acme", reverie.SourceUser, day(0)))
	dead := live(fact("jay", "lives_in", "Kano", reverie.SourceUser, day(0)))
	dead.Status = reverie.FactSuperseded
	r := mustAssert(t, []reverie.Fact{other, rel, dead}, fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(5)))
	if r.Op != OpInsert {
		t.Fatalf("%+v", r)
	}
}

func TestInvalidInput(t *testing.T) {
	ok := fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(0))
	cases := map[string]func(*reverie.Fact){
		"no value":         func(f *reverie.Fact) { f.Value = "  " },
		"no entity":        func(f *reverie.Fact) { f.Entity = "" },
		"no id":            func(f *reverie.Fact) { f.ID = "" },
		"unknown relation": func(f *reverie.Fact) { f.Relation = "teleports_to" },
		"unknown source":   func(f *reverie.Fact) { f.Source = "rumour" },
		"wrong subject":    func(f *reverie.Fact) { f.EntityType = "project" },
	}
	for name, mut := range cases {
		f := ok
		mut(&f)
		if _, err := Assert(nil, f, onto, day(1)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// apply is a minimal in-memory writer for Resolutions, used to check the
// invariant over random histories.
func apply(db map[string]reverie.Fact, r Resolution) {
	for _, u := range r.Updates {
		db[u.ID] = u
	}
	if r.Fact.ID != "" {
		db[r.Fact.ID] = r.Fact
	}
}

func all(db map[string]reverie.Fact) (out []reverie.Fact) {
	for _, f := range db {
		out = append(out, f)
	}
	return
}

// The contradiction test from the plan: whatever order facts arrive in, a
// "one" relation never ends up with two different values both plainly active.
func TestNeverTwoActiveValuesOnAOneRelation(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	sources := []reverie.FactSource{reverie.SourceUser, reverie.SourceAgent, reverie.SourceDistilled, reverie.SourceInferred}
	values := []string{"Lagos", "London", "Abuja", "Paris", "Kano"}
	for trial := 0; trial < 300; trial++ {
		db := map[string]reverie.Fact{}
		for step := 0; step < 12; step++ {
			in := fact("jay", "lives_in", values[r.Intn(len(values))], sources[r.Intn(len(sources))], day(r.Intn(60)))
			if r.Intn(8) == 0 && len(db) > 0 { // occasionally pin whatever is live
				for id, f := range db {
					if f.Live() {
						f.Pinned = true
						db[id] = f
						break
					}
				}
			}
			res, err := Assert(all(db), in, onto, day(100+step))
			if err != nil {
				t.Fatal(err)
			}
			apply(db, res)

			var active []reverie.Fact
			for _, f := range db {
				if f.Live() && f.Status == reverie.FactActive {
					active = append(active, f)
				}
			}
			if len(active) > 1 {
				t.Fatalf("trial %d step %d: %d active values on a one relation: %+v", trial, step, len(active), active)
			}
			for _, f := range db {
				if f.Status == reverie.FactSuperseded && (f.ValidTo.IsZero() || f.SupersededBy == "") {
					t.Fatalf("a superseded fact must be closed and point at its successor: %+v", f)
				}
			}
		}
	}
}

func TestUserRestatementDoesNotSettleADisputeWithAPin(t *testing.T) {
	pinned := live(fact("jay", "lives_in", "Lagos", reverie.SourceUser, day(0)))
	pinned.Pinned = true
	rival := live(fact("jay", "lives_in", "Abuja", reverie.SourceUser, day(1)))
	rival.Status = reverie.FactContested
	r := mustAssert(t, []reverie.Fact{pinned, rival}, fact("jay", "lives_in", "Abuja", reverie.SourceUser, day(5)))
	if r.Op != OpMerge || len(r.Updates) != 1 || r.Updates[0].Status != reverie.FactContested {
		t.Fatalf("a pin must not be overridden by restating the rival: %+v", r)
	}
}

func TestUserRestatementSettlesADisputeOfUnpinnedFacts(t *testing.T) {
	a := live(fact("jay", "lives_in", "Lagos", reverie.SourceAgent, day(0)))
	a.Status = reverie.FactContested
	b := live(fact("jay", "lives_in", "London", reverie.SourceDistilled, day(1)))
	b.Status = reverie.FactContested
	r := mustAssert(t, []reverie.Fact{a, b}, fact("jay", "lives_in", "lagos", reverie.SourceUser, day(5)))
	if r.Op != OpMerge || len(r.Updates) != 2 {
		t.Fatalf("%+v", r)
	}
	var won, lost reverie.Fact
	for _, u := range r.Updates {
		if u.ID == a.ID {
			won = u
		} else {
			lost = u
		}
	}
	if won.Status != reverie.FactActive || won.Source != reverie.SourceUser || lost.Status != reverie.FactSuperseded || lost.SupersededBy != a.ID {
		t.Fatalf("won %+v lost %+v", won, lost)
	}
}
