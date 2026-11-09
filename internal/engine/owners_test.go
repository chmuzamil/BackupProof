package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// A restore for another program keeps folder permissions; a plain one keeps
// everything private.
func TestRestoreOwnersKeepsFolderModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	ctx := context.Background()
	r, _ := newRepo(t)
	src := t.TempDir()
	sub := filepath.Join(src, "site")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := NewBuilder(ctx, r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddTree(ctx, src, "data"); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "x", Kind: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		owners bool
		want   os.FileMode
	}{{false, 0o700}, {true, 0o755}} {
		dir := t.TempDir()
		if _, err := Restore(ctx, r, s.Snapshot, dir, RestoreOptions{Owners: c.owners}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "data", "site"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != c.want {
			t.Errorf("owners=%v: mode %o, want %o", c.owners, fi.Mode().Perm(), c.want)
		}
	}
}
