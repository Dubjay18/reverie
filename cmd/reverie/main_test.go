package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dubjay18/reverie"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
)

func TestDatabaseFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "r.db")
	eng, err := openEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	eng.Close()
	for _, p := range []string{path, filepath.Dir(path)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is accessible to group or others: %v", p, mode)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is accessible to group or others: %v", suffix, info.Mode().Perm())
		}
	}
}

func TestDreamCLISubcommands(t *testing.T) {
	db := filepath.Join(t.TempDir(), "r.db")
	eng, err := openEngine(db)
	if err != nil {
		t.Fatal(err)
	}
	sc := reverie.Scope{Tenant: "local", Agent: "cli", Project: "test", User: "u"}
	evs := []reverie.Event{
		reverie.NewEvent(sc, "task-1", reverie.Message, reverie.OriginUser, []byte(`{"text":"deploy failed"}`)),
		reverie.NewEvent(sc, "task-1", reverie.TaskEnded, reverie.OriginSystem, []byte(`{"outcome":"failure"}`)),
	}
	if err := eng.Record(context.Background(), evs...); err != nil {
		t.Fatal(err)
	}
	_ = eng.Close()

	base := []string{"--db", db, "--tenant", "local", "--agent", "cli", "--project", "test", "--user", "u"}
	if err := dreamCmd(context.Background(), append(append([]string{}, base...), "--budget", "500")); err != nil {
		t.Fatalf("dream: %v", err)
	}

	eng, err = openEngine(db)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := eng.DreamRuns(context.Background(), sc, 5)
	_ = eng.Close()
	if err != nil || len(runs) != 1 {
		t.Fatalf("want 1 dream run, got %v (%v)", runs, err)
	}
	runID := runs[0].ID

	if err := dreamCmd(context.Background(), append(append([]string{}, base...), "show", runID)); err != nil {
		t.Fatalf("dream show: %v", err)
	}
	if err := dreamCmd(context.Background(), append(append([]string{}, base...), "revert", runID)); err != nil {
		t.Fatalf("dream revert: %v", err)
	}

	eng, err = openEngine(db)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := eng.Facts(context.Background(), sc, store.FactQuery{}, time.Time{})
	_ = eng.Close()
	if err != nil || len(fs) != 0 {
		t.Fatalf("want 0 facts after dream revert, got %+v (%v)", fs, err)
	}
}
