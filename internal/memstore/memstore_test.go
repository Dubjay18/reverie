package memstore

import (
	"context"
	"testing"

	"github.com/Dubjay18/reverie"
)

func TestRecordRecallRespectsScopeAndBudget(t *testing.T) {
	ctx := context.Background()
	m := New()
	a := reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u1"}
	b := a
	b.User = "u2"

	if err := m.Record(ctx,
		reverie.NewEvent(a, "t1", reverie.Message, []byte(`{"text":"hello"}`)),
		reverie.NewEvent(b, "t1", reverie.Message, []byte(`{"text":"other user"}`)),
	); err != nil {
		t.Fatal(err)
	}

	pack, err := m.Recall(ctx, a, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Episodes) != 1 {
		t.Fatalf("want 1 item in scope, got %d", len(pack.Episodes))
	}

	pack, err = m.Recall(ctx, a, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pack.Tokens > pack.Budget || pack.Dropped != 1 {
		t.Fatalf("budget not enforced: tokens=%d budget=%d dropped=%d", pack.Tokens, pack.Budget, pack.Dropped)
	}
}
