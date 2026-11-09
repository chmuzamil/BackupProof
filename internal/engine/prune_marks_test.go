package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

func commitPayload(t *testing.T, b *Builder, name string, payload []byte) snapshot.WithID {
	t.Helper()
	ctx := context.Background()
	if _, err := b.AddStream(ctx, name, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: name, Kind: "command"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A backup already running when data becomes unused keeps it usable: the
// first prune only marks it, and the next sees the new snapshot needs it.
func TestPruneSparesWhatARunningBackupReuses(t *testing.T) {
	ctx := context.Background()
	r, dir := newRepo(t)
	payload := bytes.Repeat([]byte("shared data "), 50000)
	first, err := NewBuilder(ctx, r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := commitPayload(t, first, "old", payload)
	running, err := NewBuilder(ctx, r, Options{})
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
	if res.Deleted != 0 || res.Marked == 0 {
		t.Fatalf("the first prune should only mark: %+v", res)
	}

	// A backup that starts now can't reuse what is marked, so it uploads it.
	be, _ := backend.NewLocal(filepath.Join(dir, "repo"))
	fresh, err := repo.Open(ctx, be, []byte("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.LoadIndex(ctx); err != nil {
		t.Fatal(err)
	}
	marks, err := fresh.PruneMarks(ctx)
	if err != nil || len(marks) == 0 {
		t.Fatalf("marks: %v %v", marks, err)
	}
	stored, err := storedBlobs(ctx, fresh)
	if err != nil || len(stored) == 0 {
		t.Fatalf("stored: %d %v", len(stored), err)
	}
	for id := range stored {
		if fresh.HasBlob(id) {
			t.Fatalf("a new backup could reuse marked blob %s", id)
		}
	}

	kept := commitPayload(t, running, "new", payload)
	// Only what the old snapshot alone used may go now, such as its manifest.
	if _, err = Prune(ctx, r, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, r, kept.Snapshot, t.TempDir(), RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestPruneDeletesOnceTheMarkIsOldEnough(t *testing.T) {
	ctx := context.Background()
	r, _ := newRepo(t)
	b, _ := NewBuilder(ctx, r, Options{})
	s := commitPayload(t, b, "gone", bytes.Repeat([]byte("old data "), 50000))
	if err := Forget(ctx, r, []string{s.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(ctx, r, PruneOptions{Grace: -time.Hour}); err != nil {
		t.Fatal(err)
	}
	marks, _ := r.PruneMarks(ctx)
	for k := range marks {
		marks[k] = time.Now().Add(-48 * time.Hour).Unix()
	}
	if err := r.SavePruneMarks(ctx, marks); err != nil {
		t.Fatal(err)
	}
	res, err := Prune(ctx, r, PruneOptions{Grace: -time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted == 0 || res.Marked != 0 {
		t.Fatalf("%+v", res)
	}
	if left, _ := r.PruneMarks(ctx); len(left) != 0 {
		t.Errorf("marks left: %d", len(left))
	}
}
