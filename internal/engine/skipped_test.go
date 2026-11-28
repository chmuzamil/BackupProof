package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// Unreadable files are counted and named in a warning; files that vanished
// while the backup ran are normal and aren't.
func TestSkippedFilesWarn(t *testing.T) {
	r, _ := newRepo(t)
	b, err := NewBuilder(context.Background(), r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b.skip("/srv/gone.tmp", fs.ErrNotExist)
	if w := b.Warnings(); len(w) != 0 {
		t.Fatalf("a vanished file made a warning: %v", w)
	}
	b.skip("/srv/a", fs.ErrPermission)
	b.skip("/srv/b", errors.New("input/output error"))
	w := b.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "2 files or folders couldn't be read") || !strings.Contains(w[0], "/srv/a, /srv/b") {
		t.Fatalf("warnings: %v", w)
	}
	if b.skipped != 2 {
		t.Fatalf("skipped %d, want 2", b.skipped)
	}
}

// On Unix, a file the agent may not read is skipped, counted in the
// snapshot's stats, and named in a warning.
func TestUnreadableFileCounted(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a Unix user who can be denied a file")
	}
	r, _ := newRepo(t)
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "ok.txt"), []byte("fine"), 0o644)
	secret := filepath.Join(src, "secret.txt")
	os.WriteFile(secret, []byte("no"), 0o000)
	ctx := context.Background()
	b, _ := NewBuilder(ctx, r, Options{})
	if err := b.AddPath(ctx, src); err != nil {
		t.Fatal(err)
	}
	snap, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "t", Kind: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Stats.Skipped != 1 {
		t.Fatalf("skipped %d, want 1", snap.Stats.Skipped)
	}
	if w := b.Warnings(); len(w) != 1 || !strings.Contains(w[0], secret) {
		t.Fatalf("warnings: %v", w)
	}
}

// A protected folder that is a link is backed up with the folder it points
// to, not as a lone link.
func TestSymlinkedRootFollowed(t *testing.T) {
	r, _ := newRepo(t)
	base := t.TempDir()
	real := filepath.Join(base, "releases", "42")
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "index.php"), []byte("<?php"), 0o644)
	link := filepath.Join(base, "current")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("can't make a symlink here: %v", err)
	}
	ctx := context.Background()
	b, _ := NewBuilder(ctx, r, Options{})
	if err := b.AddPath(ctx, link); err != nil {
		t.Fatal(err)
	}
	var sawLink, sawFile bool
	for _, e := range b.Entries() {
		switch e.Path {
		case snapshot.CleanPath(link):
			sawLink = e.Type == snapshot.TypeSymlink
		case snapshot.CleanPath(filepath.Join(real, "index.php")):
			sawFile = e.Type == snapshot.TypeFile
		}
	}
	if !sawLink || !sawFile {
		t.Fatalf("link saved: %v, file behind it saved: %v", sawLink, sawFile)
	}
}

// /proc is recognised as a pseudo filesystem, a normal folder isn't.
func TestPseudoFS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	r, _ := newRepo(t)
	b, _ := NewBuilder(context.Background(), r, Options{})
	for _, c := range []struct{ path, want string }{{"/proc", "proc"}, {t.TempDir(), ""}} {
		info, err := os.Lstat(c.path)
		if err != nil {
			t.Skip(err)
		}
		if got := b.pseudoFS(c.path, info); got != c.want {
			t.Errorf("%s: %q, want %q", c.path, got, c.want)
		}
	}
}
