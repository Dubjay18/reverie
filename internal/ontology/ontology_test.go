package ontology

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	o := Default()
	for rel, want := range map[string]Cardinality{"lives_in": One, "uses": Many, "note": Many} {
		r, err := o.Relation(rel)
		if err != nil || r.Cardinality != want {
			t.Errorf("%s: %+v %v", rel, r, err)
		}
	}
	if r, _ := o.Relation("status"); r.TTLDays != 90 {
		t.Errorf("ttl not parsed: %+v", r)
	}
	if _, err := o.Relation("nonsense"); !errors.Is(err, ErrUnknownRelation) || !strings.Contains(err.Error(), "lives_in") {
		t.Errorf("unknown relation should list the known ones: %v", err)
	}
}

func TestParseRejectsMistakes(t *testing.T) {
	for name, y := range map[string]string{
		"bad cardinality": "relations:\n  r: { cardinality: lots }",
		"missing":         "relations:\n  r: {}",
		"typo in key":     "relations:\n  r: { cardinalty: one }",
		"unknown top key": "relationz: {}",
		"bad name":        "relations:\n  Bad-Name: { cardinality: one }",
		"unknown subject": "relations:\n  r: { cardinality: one, subject: ghost }",
		"negative ttl":    "relations:\n  r: { cardinality: one, ttl_days: -1 }",
		"no relations":    "entities:\n  person: {}",
		"not yaml":        ":::",
	} {
		if _, err := Parse(strings.NewReader(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadReportsThePath(t *testing.T) {
	if _, err := Load("/definitely/not/here.yaml"); err == nil {
		t.Fatal("expected an error")
	}
}
