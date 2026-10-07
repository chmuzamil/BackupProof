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
