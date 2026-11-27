package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// dyingStore is storage whose writer "dies" after a set number of deletes:
// from then on every delete and write fails, as if the process had been
// killed or the connection lost for good. failReads breaks listing and
// reading backups, like a storage outage.
type dyingStore struct {
	backend.Backend
	deletesLeft atomic.Int64 // -1: never dies
	dead        atomic.Bool
	failReads   atomic.Bool
}

var errGone = errors.New("connection lost")

func (d *dyingStore) Delete(ctx context.Context, key string) error {
	if d.dead.Load() {
		return errGone
	}
	if d.deletesLeft.Load() >= 0 && d.deletesLeft.Add(-1) < 0 {
		d.dead.Store(true)
		return errGone
	}
	return d.Backend.Delete(ctx, key)
}

func (d *dyingStore) Put(ctx context.Context, key string, data []byte) error {
	if d.dead.Load() {
		return errGone
	}
	return d.Backend.Put(ctx, key, data)
}

func (d *dyingStore) Get(ctx context.Context, key string) ([]byte, error) {
	if d.failReads.Load() && strings.HasPrefix(key, "snapshots/") {
		return nil, errGone
	}
	return d.Backend.Get(ctx, key)
}

func (d *dyingStore) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	if d.failReads.Load() && prefix == "snapshots/" {
		return errGone
	}
	return d.Backend.List(ctx, prefix, fn)
}

func randomPayload(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// mustRestore restores a one-stream backup and compares it with want.
func mustRestore(t *testing.T, r *repo.Repo, s snapshot.WithID, want []byte) {
	t.Helper()
	dir := t.TempDir()
	if _, err := Restore(context.Background(), r, s.Snapshot, dir, RestoreOptions{}); err != nil {
		t.Fatalf("restore %s: %v", s.ID, err)
	}
	var got []byte
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			got, _ = os.ReadFile(p)
		}
		return nil
	})
	if !bytes.Equal(got, want) {
		t.Fatalf("restore %s: %d bytes back, want %d", s.ID, len(got), len(want))
	}
}

func mustCheckClean(t *testing.T, r *repo.Repo) CheckResult {
	t.Helper()
	res, err := Check(context.Background(), r, CheckOptions{ReadDataPercent: 100})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.MissingBlobs+res.CorruptBlobs+res.UnreadableBlobs > 0 || len(res.Errors) > 0 {
		t.Fatalf("check found problems: %+v", res)
	}
	return res
}

func mustBuilder(t *testing.T, r *repo.Repo) *Builder {
	t.Helper()
	b, err := NewBuilder(context.Background(), r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func countKeys(t *testing.T, be backend.Backend, prefix string) int {
	t.Helper()
	n := 0
	if err := be.List(context.Background(), prefix, func(backend.ObjectInfo) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// A prune that stops at any point (killed, connection lost) never removes
// data a backup still needs, and the next prune finishes the job.
func TestInterruptedPruneLeavesStorageUsable(t *testing.T) {
	ctx := context.Background()
	shared := randomPayload(400 << 10)
	for deletes := int64(0); deletes <= 8; deletes++ {
		t.Run(fmt.Sprintf("dies after %d deletes", deletes), func(t *testing.T) {
			local, _ := backend.NewLocal(filepath.Join(t.TempDir(), "repo"))
			store := &dyingStore{Backend: local}
			store.deletesLeft.Store(-1)
			r, err := repo.Init(ctx, store, []byte("correct horse battery"), testParams)
			if err != nil {
				t.Fatal(err)
			}
			old := commitPayload(t, mustBuilder(t, r), "old", append(randomPayload(600<<10), shared...))
			keepData := append(append([]byte(nil), shared...), randomPayload(200<<10)...)
			kept := commitPayload(t, mustBuilder(t, r), "kept", keepData)
			if err := Forget(ctx, r, []string{old.ID.String()}); err != nil {
				t.Fatal(err)
			}

			store.deletesLeft.Store(deletes)
			_, _ = Prune(ctx, r, PruneOptions{Grace: -time.Hour, MarkAge: -1})

			// The writer is gone; a new one opens the storage afresh.
			fresh, err := repo.Open(ctx, local, []byte("correct horse battery"))
			if err != nil {
				t.Fatal(err)
			}
			mustRestore(t, fresh, kept, keepData)
			if _, err := Prune(ctx, fresh, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
				t.Fatalf("the next prune: %v", err)
			}
			if res := mustCheckClean(t, fresh); res.UnusedBlobs != 0 {
				t.Errorf("the next prune left %d unused blobs", res.UnusedBlobs)
			}
			mustRestore(t, fresh, kept, keepData)
		})
	}
}

// Prune refuses to delete anything while it can't see every backup.
func TestPruneDeletesNothingDuringStorageOutage(t *testing.T) {
	ctx := context.Background()
	local, _ := backend.NewLocal(filepath.Join(t.TempDir(), "repo"))
	store := &dyingStore{Backend: local}
	store.deletesLeft.Store(-1)
	r, err := repo.Init(ctx, store, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	data := randomPayload(300 << 10)
	s := commitPayload(t, mustBuilder(t, r), "only", data)
	before := countKeys(t, local, "data/")

	store.failReads.Store(true)
	if _, err := Prune(ctx, r, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err == nil {
		t.Error("prune ran without being able to list the backups")
	}
	store.failReads.Store(false)
	if after := countKeys(t, local, "data/"); after != before {
		t.Fatalf("%d chunks deleted during the outage", before-after)
	}
	mustRestore(t, r, s, data)
}

// Two machines backing up into the same storage at the same time, with
// overlapping data, both end up with complete, restorable backups.
func TestTwoWritersAtOnce(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	local, _ := backend.NewLocal(dir)
	if _, err := repo.Init(ctx, local, []byte("correct horse battery"), testParams); err != nil {
		t.Fatal(err)
	}
	shared := randomPayload(1 << 20)
	payloads := [][]byte{
		append(randomPayload(500<<10), shared...),
		append(append([]byte(nil), shared...), randomPayload(500<<10)...),
	}
	snaps := make([]snapshot.WithID, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			be, _ := backend.NewLocal(dir) // each writer opens the storage itself
			r, err := repo.Open(ctx, be, []byte("correct horse battery"))
			if err != nil {
				errs[i] = err
				return
			}
			b, err := NewBuilder(ctx, r, Options{})
			if err != nil {
				errs[i] = err
				return
			}
			name := fmt.Sprintf("w%d", i)
			if _, err := b.AddStream(ctx, name, bytes.NewReader(payloads[i])); err != nil {
				errs[i] = err
				return
			}
			snaps[i], errs[i] = b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: name, Kind: "command"}})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	r, err := repo.Open(ctx, local, []byte("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := snapshot.List(ctx, r); err != nil || len(listed) != 2 {
		t.Fatalf("%d backups listed (%v), want 2", len(listed), err)
	}
	for i := range 2 {
		mustRestore(t, r, snaps[i], payloads[i])
	}
	mustCheckClean(t, r)
	// Nothing either backup uses may be pruned.
	if _, err := Prune(ctx, r, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		mustRestore(t, r, snaps[i], payloads[i])
	}
}

// A backup that failed partway leaves only unused chunks behind; the next
// backup of the same data completes, and prune later clears the leftovers.
func TestBackupResumesAfterFailedUpload(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()
	fs := newFakeStore(t)
	r, err := repo.Init(ctx, fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	data := randomPayload(2 << 20)
	fs.putFailAfter = 10
	b := mustBuilder(t, r)
	_, addErr := b.AddStream(ctx, "big", bytes.NewReader(data))
	if _, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "big", Kind: "command"}}); err == nil && addErr == nil {
		t.Fatal("the failing backup succeeded")
	}

	fs.mu.Lock()
	fs.putFailAfter = 0
	fs.mu.Unlock()
	fresh, err := repo.Open(ctx, fs, []byte("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	s := commitPayload(t, mustBuilder(t, fresh), "big", data)
	mustRestore(t, fresh, s, data)
	mustCheckClean(t, fresh)

	if _, err := Prune(ctx, fresh, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
		t.Fatal(err)
	}
	if res := mustCheckClean(t, fresh); res.UnusedBlobs != 0 {
		t.Errorf("%d chunks from the failed backup left after prune", res.UnusedBlobs)
	}
	mustRestore(t, fresh, s, data)
}
