package sqlite

import (
	"io/fs"
	"path/filepath"
	"testing"
)

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no embedded migrations: %v", err)
	}
	path := filepath.Join(t.TempDir(), "r.db")

	for i := 0; i < 2; i++ {
		s, err := Open(path, Config{})
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != len(files) {
			t.Fatalf("open #%d: want %d applied migrations, got %d", i, len(files), n)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
