package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// A copy in another storage has the same content root, restores to the same
// files, and later copies reuse what is already there.
func TestCopySnapshot(t *testing.T) {
	ctx := context.Background()
	src, dir := newRepo(t)
	dst, _ := newRepo(t)
	data := filepath.Join(dir, "data")
	writeTree(t, data)
	backup := func() snapshot.WithID {
		b, err := NewBuilder(ctx, src, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.AddPath(ctx, data); err != nil {
			t.Fatal(err)
		}
		s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "data", Kind: "files"}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := backup()
	cp, st, err := CopySnapshot(ctx, src, dst, first.ID, first.Root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Root != first.Root || st.NewChunks == 0 {
		t.Fatalf("copy root %s (want %s), new chunks %d", cp.Root, first.Root, st.NewChunks)
	}
	out := filepath.Join(dir, "out")
	if _, err := Restore(ctx, dst, cp.Snapshot, out, RestoreOptions{}); err != nil {
		t.Fatalf("restore from the copy: %v", err)
	}
	if root, _, err := TreeRoot(out); err != nil || root != first.Root {
		t.Fatalf("restored copy root %s, want %s (%v)", root, first.Root, err)
	}
	// A second backup with one changed file: only its chunks are copied.
	if err := os.WriteFile(filepath.Join(data, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := backup()
	_, st2, err := CopySnapshot(ctx, src, dst, second.ID, second.Root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st2.NewChunks != 1 {
		t.Errorf("second copy uploaded %d new chunks, want 1", st2.NewChunks)
	}
	// A copy is refused when the backup doesn't match its proof.
	if _, _, err := CopySnapshot(ctx, src, dst, second.ID, first.Root, nil); err == nil {
		t.Error("copy with the wrong expected root should fail")
	}
}
