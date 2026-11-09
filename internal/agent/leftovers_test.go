package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClearLeftovers(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"drills/bp-drill-1/secret.txt", "drills/bp-apptest-2/x", "volume-3/x", "restore-4/x", "drills/keep/x", "other/x"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file that only looks like a leftover folder stays.
	if err := os.WriteFile(filepath.Join(dir, "restore-notes"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	(&Agent{dir: dir}).clearLeftovers()
	for _, gone := range []string{"drills/bp-drill-1", "drills/bp-apptest-2", "volume-3", "restore-4"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"drills/keep/x", "other/x", "restore-notes"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s was removed", kept)
		}
	}
}
