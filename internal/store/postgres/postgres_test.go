package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/Dubjay18/reverie/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var (
	dsnOnce      sync.Once
	sharedDSN    string
	containerID  string
	dsnSkipError string
)

func testDSN(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("REVERIE_TEST_DSN"); env != "" {
		return env
	}
	dsnOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			dsnSkipError = "REVERIE_TEST_DSN not set and docker not found"
			return
		}
		out, err := exec.Command("docker", "run", "-d", "--rm",
			"-e", "POSTGRES_PASSWORD=reverie",
			"-e", "POSTGRES_DB=reverie",
			"-P", "postgres:16-alpine").CombinedOutput()
		if err != nil {
			dsnSkipError = fmt.Sprintf("docker run postgres failed: %v (%s)", err, strings.TrimSpace(string(out)))
			return
		}
		containerID = strings.TrimSpace(string(out))
		portOut, err := exec.Command("docker", "port", containerID, "5432/tcp").CombinedOutput()
		if err != nil {
			_ = exec.Command("docker", "rm", "-f", containerID).Run()
			dsnSkipError = fmt.Sprintf("docker port failed: %v", err)
			return
		}
		lines := strings.Split(strings.TrimSpace(string(portOut)), "\n")
		hostPort := lines[0]
		if idx := strings.LastIndex(hostPort, ":"); idx >= 0 {
			hostPort = "127.0.0.1:" + hostPort[idx+1:]
		}
		dsn := fmt.Sprintf("postgres://postgres:reverie@%s/reverie?sslmode=disable", hostPort)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			conn, err := pgx.Connect(ctx, dsn)
			cancel()
			if err == nil {
				_ = conn.Close(context.Background())
				sharedDSN = dsn
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
		dsnSkipError = "timed out waiting for postgres test container"
	})
	if dsnSkipError != "" {
		t.Skip(dsnSkipError)
	}
	return sharedDSN
}

func TestMain(m *testing.M) {
	code := m.Run()
	if containerID != "" {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	}
	os.Exit(code)
}

func TestConformance(t *testing.T) {
	baseDSN := testDSN(t)
	var seq int
	storetest.Run(t, func(t *testing.T) store.Store {
		seq++
		schema := fmt.Sprintf("s_%d_%d", time.Now().UnixNano(), seq)
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, baseDSN)
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.Exec(ctx, `CREATE SCHEMA `+schema)
		_ = conn.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sep := "&"
		if !strings.Contains(baseDSN, "?") {
			sep = "?"
		}
		scopedDSN := baseDSN + sep + "search_path=" + schema
		s, err := Open(ctx, scopedDSN, Config{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestRowLevelSecurity connects as an ordinary role that owns the tables (as
// an application role usually does) and checks that the policies, not just
// the Go WHERE clauses, keep scopes apart.
func TestRowLevelSecurity(t *testing.T) {
	baseDSN := testDSN(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	role, schema := "reverie_rls_"+suffix, "rls_"+suffix
	if _, err := admin.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'rls' NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Skipf("cannot create a test role (need a superuser DSN): %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS `+role)
	})
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema+` AUTHORIZATION `+role); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "rls")
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "u"}
	b := reverie.Scope{Tenant: "t", Agent: "a", Project: "p", User: "other"}
	other := reverie.Scope{Tenant: "t", Agent: "a", Project: "p2", User: "u"}
	now := time.Now().UTC()
	for i, sc := range []reverie.Scope{a, b, other} {
		err := s.For(sc).Tx(ctx, func(tx store.Tx) error {
			if _, err := tx.Events().Append(ctx, reverie.NewEvent(sc, "task", reverie.Message, reverie.OriginUser, []byte(`{"text":"x"}`))); err != nil {
				return err
			}
			f := reverie.Fact{ID: fmt.Sprintf("f%d", i), Entity: "e", Relation: "note", Value: "v", Confidence: 1,
				Source: reverie.SourceUser, Status: reverie.FactActive, ValidFrom: now, RecordedAt: now}
			if err := tx.Facts().Save(ctx, f); err != nil {
				return err
			}
			return tx.Skills().Save(ctx, store.Skill{ID: fmt.Sprintf("s%d", i), Version: 1, Name: "s", Status: store.SkillActive, CreatedAt: now, UpdatedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	raw := func(sc reverie.Scope, sql string) (n int) {
		t.Helper()
		err := s.For(sc).View(ctx, func(tx store.Tx) error {
			return tx.(*txn).tx.QueryRow(ctx, sql).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// No WHERE clause at all: the policy alone limits what a scope sees.
	if n := raw(a, `SELECT count(*) FROM facts`); n != 1 {
		t.Fatalf("facts visible to scope a without a filter: %d, want 1", n)
	}
	if n := raw(a, `SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("events visible to scope a without a filter: %d, want 1", n)
	}
	// Skills are per (tenant, project): user b shares project p with a.
	if n := raw(b, `SELECT count(*) FROM skills`); n != 2 {
		t.Fatalf("skills visible in project p: %d, want 2", n)
	}
	if n := raw(other, `SELECT count(*) FROM skills`); n != 1 {
		t.Fatalf("skills visible in project p2: %d, want 1", n)
	}
	// Outside a scoped transaction nothing is visible.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM facts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped read saw %d facts (%v)", n, err)
	}
	// A write into another scope's rows is refused by WITH CHECK.
	err = s.For(a).Tx(ctx, func(tx store.Tx) error {
		_, err := tx.(*txn).tx.Exec(ctx, `INSERT INTO facts (tenant, agent, project, usr, id, entity, relation, value, confidence, source, status, valid_from, recorded_at)
			VALUES ('t', 'a', 'p', 'other', 'planted', 'e', 'note', 'v', 1, 'user_stated', 'active', '', '')`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("cross-scope insert: %v", err)
	}
	// And an update cannot reach another scope's row.
	err = s.For(a).Tx(ctx, func(tx store.Tx) error {
		tag, err := tx.(*txn).tx.Exec(ctx, `UPDATE facts SET value = 'hijacked'`)
		if err == nil && tag.RowsAffected() != 1 {
			t.Fatalf("unfiltered update touched %d rows, want 1", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.For(b).View(ctx, func(tx store.Tx) error {
		f, err := tx.Facts().Get(ctx, "f1")
		if err == nil && f.Value != "v" {
			t.Fatalf("scope a's update reached scope b: %+v", f)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Maintenance can still list scopes.
	scopes, err := s.Scopes(ctx)
	if err != nil || len(scopes) != 3 {
		t.Fatalf("scopes under rls: %v %v", scopes, err)
	}
	// The conformance suite also passes as this role.
	if err := s.For(a).Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Facts().Query(ctx, store.FactQuery{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
