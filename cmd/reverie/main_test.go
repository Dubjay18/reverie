package main

import (
	"os"
	"path/filepath"
	"testing"
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
