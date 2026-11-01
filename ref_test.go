package reverie

import "testing"

func TestRefRoundtrip(t *testing.T) {
	a, b := NewID(), NewID()
	for _, r := range []Ref{
		{TaskID: "deploy"},
		{TaskID: "deploy", From: a, To: b},
		{TaskID: "deploy", From: a},
		{TaskID: "deploy", To: b},
		{TaskID: "weird#task"},
		{FactID: "01ABC"},
	} {
		got, err := ParseRef(r.String())
		if err != nil || got != r {
			t.Errorf("%q: got %+v err %v, want %+v", r.String(), got, err, r)
		}
	}
	if r, err := ParseRef("ep:deploy @2026-10-31T16:00Z"); err != nil || r.TaskID != "deploy" {
		t.Errorf("trailing citation text must be ignored: %+v %v", r, err)
	}
	for _, bad := range []string{"", "deploy", "ep:", "ev:abc", "ep:deploy#01ZZZ..", "ep:deploy#..01ZZZ", "ep:deploy#01ABC..01DEF",
		"fact:", "ep:deploy#" + b.String() + ".." + a.String()} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}
