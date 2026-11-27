package repo_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// The folder each release's command-line tool backed up (see
// scripts/compat-fixtures.sh), before and after the change between its two backups.
var (
	firstBackup = map[string]string{
		"readme.txt":     "BackupProof compatibility fixture\n",
		"docs/notes.txt": "a nested file\n",
	}
	secondBackup = map[string]string{
		"readme.txt":     "BackupProof compatibility fixture, edited\n",
		"docs/notes.txt": "a nested file\n",
		"docs/added.txt": "added after the first backup\n",
	}
)

const fixturePassword = "compat-fixture-password"

// Storage written by every earlier release must still open and restore with
// this version, pass a full check, and take new backups. The storage folders
// in testdata/compat/<tag>/ were written by that release's own command-line
// tool (see docs/COMPATIBILITY.md).
func TestStorageFromEarlierReleasesStillRestores(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join("testdata", "compat", "v*"))
	if len(dirs) == 0 {
		t.Skip("no storage fixtures yet")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			// Work on a copy: the check and the new backup must not touch the fixture.
			work := filepath.Join(t.TempDir(), "repo")
			if err := os.CopyFS(work, os.DirFS(dir)); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			be, err := backend.NewLocal(work)
			if err != nil {
				t.Fatal(err)
			}
			r, err := repo.Open(ctx, be, []byte(fixturePassword))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			snaps, err := snapshot.List(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if len(snaps) != 2 {
				t.Fatalf("%d backups, want 2", len(snaps))
			}
			slices.SortFunc(snaps, func(a, b snapshot.WithID) int { return a.Time.Compare(b.Time) })
			for i, want := range []map[string]string{firstBackup, secondBackup} {
				restoreAndCompare(t, r, snaps[i], want)
			}

			res, err := engine.Check(ctx, r, engine.CheckOptions{ReadDataPercent: 100})
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if res.MissingBlobs+res.CorruptBlobs+res.UnreadableBlobs > 0 || len(res.Errors) > 0 || res.ReadBlobs != res.ReferencedBlobs {
				t.Errorf("check: %+v", res)
			}

			// A backup made now goes into the old storage and restores too.
			src := t.TempDir()
			now := map[string]string{"readme.txt": "written by the current version\n", "docs/notes.txt": "a nested file\n"}
			writeFiles(t, src, now)
			b, err := engine.NewBuilder(ctx, r, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.AddPath(ctx, src); err != nil {
				t.Fatal(err)
			}
			s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "files", Kind: "files"}})
			if err != nil {
				t.Fatalf("new backup into old storage: %v", err)
			}
			restoreAndCompare(t, r, s, now)
		})
	}
}

func restoreAndCompare(t *testing.T, r *repo.Repo, s snapshot.WithID, want map[string]string) {
	t.Helper()
	target := t.TempDir()
	if _, err := engine.Restore(context.Background(), r, s.Snapshot, target, engine.RestoreOptions{}); err != nil {
		t.Fatalf("restore %s: %v", s.ID, err)
	}
	if root, _, err := engine.TreeRoot(target); err != nil || root != s.Root {
		t.Errorf("restored tree root %s, snapshot says %s (%v)", root, s.Root, err)
	}
	got := map[string]string{}
	_ = filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		data, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(target, p)
		got[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	for name, content := range want {
		found := false
		for p, c := range got {
			if p == name || strings.HasSuffix(p, "/"+name) {
				found = true
				if c != content {
					t.Errorf("%s restored as %q, want %q", name, c, content)
				}
			}
		}
		if !found {
			t.Errorf("%s missing after restore (got %v)", name, keys(got))
		}
	}
	if len(got) != len(want) {
		t.Errorf("restored %d files, want %d: %v", len(got), len(want), keys(got))
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func keys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	slices.Sort(k)
	return k
}
