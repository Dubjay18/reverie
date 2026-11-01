package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

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
