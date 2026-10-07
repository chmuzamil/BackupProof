package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

var testParams = chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10}

func newRepo(t *testing.T) (*repo.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	be, _ := backend.NewLocal(filepath.Join(dir, "repo"))
	r, err := repo.Init(context.Background(), be, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func writeTree(t *testing.T, root string) {
	t.Helper()
	big := make([]byte, 300<<10)
	rand.Read(big)
	files := map[string][]byte{
		"a.txt":             []byte("hello proof"),
		"sub/b.bin":         big,
		"sub/deep/c.txt":    []byte(strings.Repeat("dedupe me ", 5000)),
		"sub/deep/copy.txt": []byte(strings.Repeat("dedupe me ", 5000)),
		"empty":             {},
	}
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func backup(t *testing.T, r *repo.Repo, src string, parent *snapshot.WithID) snapshot.WithID {
	t.Helper()
	ctx := context.Background()
	b, err := NewBuilder(ctx, r, Options{Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddPath(ctx, src); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "test", Kind: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBackupRestoreRootMatches(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t)
	src := filepath.Join(dir, "src")
	writeTree(t, src)
	s := backup(t, r, src, nil)

	if s.Stats.Files != 5 {
		t.Fatalf("files = %d, want 5", s.Stats.Files)
	}
	target := filepath.Join(dir, "restore")
	if _, err := Restore(ctx, r, s.Snapshot, target, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	root, _, err := TreeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	if root != s.Root {
		t.Fatalf("restored tree root %s != snapshot root %s", root, s.Root)
	}
	got, _ := os.ReadFile(filepath.Join(target, filepath.FromSlash(snapshot.CleanPath(src)), "a.txt"))
	if string(got) != "hello proof" {
		t.Fatalf("restored content %q", got)
	}
}

func TestIncrementalReusesChunks(t *testing.T) {
	r, dir := newRepo(t)
	src := filepath.Join(dir, "src")
	writeTree(t, src)
	first := backup(t, r, src, nil)
	second := backup(t, r, src, &first)
	if second.Stats.Unchanged != 5 || second.Stats.NewChunks != 0 {
		t.Fatalf("incremental stats: %+v", second.Stats)
	}
	if second.Root != first.Root {
		t.Fatal("unchanged tree must have identical root")
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("changed"), 0o644)
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "a.txt"), future, future)
	third := backup(t, r, src, &second)
	if third.Root == second.Root {
		t.Fatal("changed tree must change root")
	}
}

func TestTamperedBlobDetected(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t)
	src := filepath.Join(dir, "src")
	writeTree(t, src)
	s := backup(t, r, src, nil)

	// Flip one byte in one stored data blob.
	var victim string
	r.Backend().List(ctx, "data/", func(o backend.ObjectInfo) error {
		if victim == "" {
			victim = o.Key
		}
		return nil
	})
	p := filepath.Join(dir, "repo", filepath.FromSlash(victim))
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	os.WriteFile(p, b, 0o600)

	res, err := Check(ctx, r, CheckOptions{ReadDataPercent: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.CorruptBlobs != 1 || res.OK() {
		t.Fatalf("expected 1 corrupt blob, got %+v", res)
	}
	_ = s
}

func TestWrongPassword(t *testing.T) {
	_, dir := newRepo(t)
	be, _ := backend.NewLocal(filepath.Join(dir, "repo"))
	if _, err := repo.Open(context.Background(), be, []byte("wrong password!")); err != bpcrypto.ErrWrongPassword {
		t.Fatalf("got %v", err)
	}
	if _, err := repo.Open(context.Background(), be, []byte("correct horse battery")); err != nil {
		t.Fatal(err)
	}
}

func TestStreamAndPrune(t *testing.T) {
	ctx := context.Background()
	r, _ := newRepo(t)
	b, _ := NewBuilder(ctx, r, Options{})
	payload := bytes.Repeat([]byte("row,"), 100000)
	if _, err := b.AddStream(ctx, "postgres/app.dump", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "db", Kind: "postgres"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Forget(ctx, r, []string{s.ID.String()}); err != nil {
		t.Fatal(err)
	}
	res, err := Prune(ctx, r, PruneOptions{Grace: -time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted == 0 {
		t.Fatal("expected unreferenced blobs to be pruned")
	}
	chk, _ := Check(ctx, r, CheckOptions{})
	if chk.StoredBlobs != 0 {
		t.Fatalf("stored blobs left: %d", chk.StoredBlobs)
	}
}

func TestSampleDeterministic(t *testing.T) {
	ids := map[bpcrypto.ID]struct{}{}
	for i := 0; i < 100; i++ {
		ids[bpcrypto.Hash([]byte{byte(i)})] = struct{}{}
	}
	a := SampleIDs(ids, 10, []byte("seed"))
	b := SampleIDs(ids, 10, []byte("seed"))
	c := SampleIDs(ids, 10, []byte("other"))
	if len(a) != 10 || a[0] != b[0] || a[0] == c[0] && a[1] == c[1] {
		t.Fatal("sampling must be deterministic per seed")
	}
}

// craftedSnapshot builds a snapshot the way an attacker holding the
// repository password could: arbitrary symlink and file entries.
func craftedSnapshot(t *testing.T, r *repo.Repo, links map[string]string, files map[string]string) *snapshot.Snapshot {
	t.Helper()
	ctx := context.Background()
	b, err := NewBuilder(ctx, r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for p, target := range links {
		b.add(&snapshot.Entry{Path: p, Type: snapshot.TypeSymlink, Link: target})
	}
	for p, content := range files {
		if _, err := b.AddReader(ctx, p, 0o644, time.Now(), strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "evil", Kind: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	return s.Snapshot
}

func TestRestoreRefusesWritesThroughSymlinks(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t)
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(outside, 0o755)
	victim := filepath.Join(outside, "victim.txt")
	os.WriteFile(victim, []byte("original"), 0o644)

	// 1. A file placed "beneath" a symlink that points outside the target.
	s := craftedSnapshot(t, r, map[string]string{"evil": filepath.ToSlash(outside)},
		map[string]string{"evil/pwned.txt": "attacker content"})
	if _, err := Restore(ctx, r, s, filepath.Join(dir, "t1"), RestoreOptions{}); err == nil {
		t.Fatal("restore of a file beneath a symlink must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatal("file was written outside the restore target")
	}

	// 2. A symlink named like the temp file of a later entry.
	s = craftedSnapshot(t, r, map[string]string{"x.bp-partial": filepath.ToSlash(victim)},
		map[string]string{"x": "attacker content"})
	Restore(ctx, r, s, filepath.Join(dir, "t2"), RestoreOptions{})
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("victim file outside the target was overwritten: %q", b)
	}

	// 3. A symlink already present in the target directory.
	t3 := filepath.Join(dir, "t3")
	os.MkdirAll(t3, 0o755)
	if err := os.Symlink(outside, filepath.Join(t3, "pre")); err == nil {
		s = craftedSnapshot(t, r, nil, map[string]string{"pre/pwned2.txt": "attacker content"})
		Restore(ctx, r, s, t3, RestoreOptions{})
		if _, err := os.Stat(filepath.Join(outside, "pwned2.txt")); err == nil {
			t.Fatal("restore followed a pre-existing symlink out of the target")
		}
	} else {
		t.Logf("case 3 skipped: cannot create symlinks here (%v)", err)
	}
}

// TestRestoreInPlaceRefusesPlantedSymlinks: restoring in place writes as root
// into folders other people own. A folder someone swapped for a symlink after
// the backup (here to the target's own etc folder) must not be followed.
func TestRestoreInPlaceRefusesPlantedSymlinks(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t)
	target := filepath.Join(dir, "root")
	os.MkdirAll(filepath.Join(target, "etc"), 0o755)
	shadow := filepath.Join(target, "etc", "shadow")
	os.WriteFile(shadow, []byte("root secrets"), 0o600)
	os.MkdirAll(filepath.Join(target, "home", "alice"), 0o755)
	link := filepath.Join(target, "home", "alice", "docs")
	if err := os.Symlink(filepath.Join("..", "..", "etc"), link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if os.Geteuid() == 0 { // the link is alice's, not root's (root's own links are trusted)
		os.Lchown(link, 65534, 65534)
	}
	s := craftedSnapshot(t, r, nil, map[string]string{"home/alice/docs/shadow": "alice was here", "home/alice/notes.txt": "fine"})
	if _, err := Restore(ctx, r, s, target, RestoreOptions{Original: true}); err == nil {
		t.Fatal("in-place restore through a planted symlink must be refused")
	}
	if b, _ := os.ReadFile(shadow); string(b) != "root secrets" {
		t.Fatalf("file outside the folder was overwritten: %q", b)
	}

	// Without the symlink, the same restore works.
	os.Remove(filepath.Join(target, "home", "alice", "docs"))
	if _, err := Restore(ctx, r, s, target, RestoreOptions{Original: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "home", "alice", "docs", "shadow")); string(b) != "alice was here" {
		t.Fatalf("restored file: %q", b)
	}
}
