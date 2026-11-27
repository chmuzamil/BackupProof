package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// countingStore records what a storage was asked to do, and can fail pack uploads.
type countingStore struct {
	backend.Backend
	mu           sync.Mutex
	lists        map[string]int
	gets         map[string]int
	puts         map[string]int
	failPackPuts bool
	// failPuts fails every write whose key starts with it: an interruption.
	failPuts string
}

func newCountingStore(t *testing.T) (*countingStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	local, err := backend.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &countingStore{Backend: local, lists: map[string]int{}, gets: map[string]int{}, puts: map[string]int{}}, dir
}

func top(key string) string { return key[:strings.IndexByte(key+"/", '/')] }

func (c *countingStore) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	c.mu.Lock()
	c.lists[top(prefix)]++
	c.mu.Unlock()
	return c.Backend.List(ctx, prefix, fn)
}

func (c *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	c.gets[top(key)]++
	c.mu.Unlock()
	return c.Backend.Get(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, data []byte) error {
	c.mu.Lock()
	c.puts[top(key)]++
	fail := c.failPackPuts && strings.HasPrefix(key, "packs/") || c.failPuts != "" && strings.HasPrefix(key, c.failPuts)
	c.mu.Unlock()
	if fail {
		return errors.New("access denied")
	}
	return c.Backend.Put(ctx, key, data)
}

func (c *countingStore) reset() {
	c.mu.Lock()
	c.lists, c.gets, c.puts = map[string]int{}, map[string]int{}, map[string]int{}
	c.mu.Unlock()
}

const pw = "correct horse battery"

// A v2 backup writes packs and one index file, and never lists or writes
// one-object-per-blob data, which is what made large storage slow.
func TestV2BackupWritesPacksAndNeverListsBlobs(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	ctx := context.Background()
	store, _ := newCountingStore(t)
	r, err := repo.Init(ctx, store, []byte(pw), testParams)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config().Version != 2 {
		t.Fatalf("new storage is v%d, want v2", r.Config().Version)
	}
	data := randomPayload(40 << 20) // more than two packs
	first := commitPayload(t, mustBuilder(t, r), "big", data)
	if store.lists["data"] != 0 || store.puts["data"] != 0 {
		t.Errorf("v2 backup listed data/ %d times and wrote %d blob objects", store.lists["data"], store.puts["data"])
	}
	if store.puts["packs"] < 3 || store.puts["index"] != 1 {
		t.Errorf("%d packs and %d index files written, want at least 3 and exactly 1", store.puts["packs"], store.puts["index"])
	}
	packs, _ := r.Packs(ctx)
	for id, o := range packs {
		if o.Size > 20<<20 {
			t.Errorf("pack %s is %d bytes", id.Short(), o.Size)
		}
	}

	// A second backup of the same data reuses it from a fresh open: one
	// index read, no blob listing, nothing new uploaded but the file list.
	store.reset()
	again, err := repo.Open(ctx, store, []byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	b := mustBuilder(t, again)
	second := commitPayload(t, b, "big", data)
	if store.lists["data"] != 0 || store.puts["packs"] > 1 {
		t.Errorf("unchanged data: listed data/ %d times, wrote %d packs", store.lists["data"], store.puts["packs"])
	}
	if second.Stats.NewChunks > 2 {
		t.Errorf("%d new chunks for unchanged data", second.Stats.NewChunks)
	}
	mustRestore(t, again, first, data)
	mustRestore(t, again, second, data)
	mustCheckClean(t, again)
}

// A pack that fails to upload fails the backup: no snapshot is saved, and a
// later backup uploads that data again instead of trusting it is there.
func TestV2FailedPackUploadWritesNoSnapshot(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	fastRetries(t)
	ctx := context.Background()
	store, _ := newCountingStore(t)
	r, err := repo.Init(ctx, store, []byte(pw), testParams)
	if err != nil {
		t.Fatal(err)
	}
	data := randomPayload(3 << 20)
	store.failPackPuts = true
	b := mustBuilder(t, r)
	_, addErr := b.AddStream(ctx, "x", bytes.NewReader(data))
	if _, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "x", Kind: "command"}}); err == nil && addErr == nil {
		t.Fatal("a backup whose packs failed to upload succeeded")
	}
	if ids, _ := r.ListIDs(ctx, "snapshots"); len(ids) != 0 {
		t.Fatalf("%d snapshots saved", len(ids))
	}
	store.failPackPuts = false
	s := commitPayload(t, mustBuilder(t, r), "x", data) // the same Repo, as an agent would reuse it
	mustRestore(t, r, s, data)
	mustCheckClean(t, r)
}

// A missing pack shows up as missing data in both a restore and a check.
func TestV2MissingPack(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	ctx := context.Background()
	store, dir := newCountingStore(t)
	r, err := repo.Init(ctx, store, []byte(pw), testParams)
	if err != nil {
		t.Fatal(err)
	}
	data := randomPayload(1 << 20)
	s := commitPayload(t, mustBuilder(t, r), "x", data)
	packs, _ := r.Packs(ctx)
	for id := range packs {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(repo.PackKey(id)))); err != nil {
			t.Fatal(err)
		}
	}
	fresh, _ := repo.Open(ctx, store, []byte(pw))
	if _, err := Restore(ctx, fresh, s.Snapshot, t.TempDir(), RestoreOptions{}); err == nil {
		t.Error("restored a backup whose pack is gone")
	}
	res, err := Check(ctx, fresh, CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.MissingBlobs == 0 || res.OK() {
		t.Errorf("check after losing the pack: %+v", res)
	}
}

// Upgrading v1 storage keeps every old backup restorable without copying
// anything, and new backups then write packs.
func TestUpgradeV1ToV2(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	ctx := context.Background()
	store, _ := newCountingStore(t)
	r, err := repo.InitVersion(ctx, store, []byte(pw), testParams, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldData := randomPayload(600 << 10)
	old := commitPayload(t, mustBuilder(t, r), "x", oldData)
	blobsBefore := countKeys(t, store, "data/")

	if err := r.Upgrade(ctx, t.Logf); err != nil {
		t.Fatal(err)
	}
	cfgBefore, _ := store.Get(ctx, "config")
	if err := r.Upgrade(ctx, nil); err != nil {
		t.Errorf("upgrading v2 storage again: %v", err)
	}
	if cfgAfter, _ := store.Get(ctx, "config"); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Error("upgrading v2 storage again changed it")
	}
	store.reset()
	up, err := repo.Open(ctx, store, []byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	if up.Config().Version != 2 {
		t.Fatalf("still v%d after the upgrade", up.Config().Version)
	}
	mustRestore(t, up, old, oldData)
	newData := append(append([]byte(nil), oldData...), randomPayload(300<<10)...)
	s := commitPayload(t, mustBuilder(t, up), "x", newData)
	if store.lists["data"] != 0 || store.puts["data"] != 0 {
		t.Errorf("after upgrading, a backup listed data/ %d times and wrote %d blob objects", store.lists["data"], store.puts["data"])
	}
	if s.Stats.NewChunks > 30 {
		t.Errorf("%d new chunks: the old blobs weren't reused", s.Stats.NewChunks)
	}
	if got := countKeys(t, store, "data/"); got != blobsBefore {
		t.Errorf("the upgrade moved blob objects: %d before, %d after", blobsBefore, got)
	}
	mustRestore(t, up, s, newData)
	mustCheckClean(t, up)

	// Forgetting the old backup lets prune delete old blob objects it alone used.
	if err := Forget(ctx, up, []string{old.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(ctx, up, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := repo.Open(ctx, store, []byte(pw))
	mustRestore(t, fresh, s, newData)
	if res := mustCheckClean(t, fresh); res.UnusedBlobs != 0 {
		t.Errorf("%d unused blobs after prune", res.UnusedBlobs)
	}
}

// An upgrade cut off before it finishes leaves the storage on v1 and usable,
// and running it again finishes the job, even after a prune in between
// deleted blobs the half-written index lists, and with an index file that was
// cut off mid-write. The finished index never claims a blob that's gone, so
// new backups upload it again instead of pointing at nothing.
func TestUpgradeInterruptedAndRunAgain(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	fastRetries(t)
	ctx := context.Background()
	store, dir := newCountingStore(t)
	r, err := repo.InitVersion(ctx, store, []byte(pw), testParams, 1)
	if err != nil {
		t.Fatal(err)
	}
	aData, bData := randomPayload(500<<10), randomPayload(500<<10)
	a := commitPayload(t, mustBuilder(t, r), "a", aData)
	b := commitPayload(t, mustBuilder(t, r), "b", bData)

	// Interrupted after the index files, before the format version.
	store.failPuts = "config"
	if err := r.Upgrade(ctx, t.Logf); err == nil {
		t.Fatal("the upgrade finished although the config couldn't be written")
	}
	store.failPuts = ""
	if n := countKeys(t, store, "index/"); n == 0 {
		t.Fatal("the interrupted upgrade wrote no index files; the test needs some")
	}
	still, err := repo.Open(ctx, store, []byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	if v := still.Config().Version; v != 1 {
		t.Fatalf("an interrupted upgrade left v%d", v)
	}
	mustRestore(t, still, a, aData)
	mustRestore(t, still, b, bData)
	mustCheckClean(t, still)

	// Another interruption cut an index file off mid-write.
	if err := os.WriteFile(filepath.Join(dir, "index", strings.Repeat("ab", 32)), []byte("cut off"), 0o600); err != nil {
		t.Fatal(err)
	}
	// While still v1, a prune deletes the blobs only backup a used, which
	// the leftover index files still list.
	if err := Forget(ctx, still, []string{a.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(ctx, still, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
		t.Fatal(err)
	}

	again, _ := repo.Open(ctx, store, []byte(pw))
	if err := again.Upgrade(ctx, t.Logf); err != nil {
		t.Fatalf("running the upgrade again: %v", err)
	}
	up, err := repo.Open(ctx, store, []byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	if v := up.Config().Version; v != 2 {
		t.Fatalf("v%d after running the upgrade again", v)
	}
	indexed, err := up.StoredBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := map[string]bool{}
	if err := store.List(ctx, "data/", func(o backend.ObjectInfo) error {
		onDisk[o.Key[strings.LastIndex(o.Key, "/")+1:]] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for id := range indexed {
		if !onDisk[id.String()] {
			t.Errorf("the index lists blob %s, which the prune deleted", id.Short())
		}
	}
	if len(indexed) != len(onDisk) {
		t.Errorf("%d blobs indexed, %d stored", len(indexed), len(onDisk))
	}
	mustRestore(t, up, b, bData)
	// Backing up a's data again must upload what the prune deleted.
	a2 := commitPayload(t, mustBuilder(t, up), "a", aData)
	fresh, _ := repo.Open(ctx, store, []byte(pw))
	mustRestore(t, fresh, a2, aData)
	mustCheckClean(t, fresh)
}

// Prune deletes packs nothing needs, rewrites packs that are mostly unneeded,
// folds the index files into one, and every remaining backup still restores.
func TestPruneV2RepacksAndConsolidates(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	ctx := context.Background()
	store, _ := newCountingStore(t)
	r, err := repo.Init(ctx, store, []byte(pw), testParams)
	if err != nil {
		t.Fatal(err)
	}
	keep := randomPayload(2 << 20)
	var gone []snapshot.WithID
	for range 3 {
		gone = append(gone, commitPayload(t, mustBuilder(t, r), "x", append(randomPayload(6<<20), keep[:1<<20]...)))
	}
	kept := commitPayload(t, mustBuilder(t, r), "x", keep)
	var ids []string
	for _, s := range gone {
		ids = append(ids, s.ID.String())
	}
	if err := Forget(ctx, r, ids); err != nil {
		t.Fatal(err)
	}

	// First run only marks.
	res, err := Prune(ctx, r, PruneOptions{Grace: -time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 || res.Marked == 0 {
		t.Fatalf("the first prune should only mark: %+v", res)
	}
	// A backup starting now can't reuse what is marked.
	fresh, _ := repo.Open(ctx, store, []byte(pw))
	if err := fresh.LoadIndex(ctx); err != nil {
		t.Fatal(err)
	}
	res, err = Prune(ctx, fresh, PruneOptions{Grace: -time.Hour, MarkAge: -1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted == 0 || res.IndexFiles < 4 {
		t.Errorf("second prune: %+v", res)
	}
	if n := countKeys(t, store, "index/"); n != 1 {
		t.Errorf("%d index files after prune, want 1", n)
	}
	after, _ := repo.Open(ctx, store, []byte(pw))
	mustRestore(t, after, kept, keep)
	if c := mustCheckClean(t, after); c.UnusedBlobs != 0 || c.OrphanPacks != 0 {
		t.Errorf("after prune: %d unused blobs, %d orphan packs", c.UnusedBlobs, c.OrphanPacks)
	}
}

// Packs written by a backup that never finished are deleted by prune once
// they're old enough, and not before.
func TestPruneV2ClearsUnfinishedBackups(t *testing.T) {
	t.Setenv("BP_NO_CACHE", "1")
	ctx := context.Background()
	store, _ := newCountingStore(t)
	r, err := repo.Init(ctx, store, []byte(pw), testParams)
	if err != nil {
		t.Fatal(err)
	}
	s := commitPayload(t, mustBuilder(t, r), "x", randomPayload(1<<20))
	// A backup that uploaded a full pack and then died before its index.
	b := mustBuilder(t, r)
	if _, err := b.AddStream(ctx, "y", bytes.NewReader(randomPayload(20<<20))); err != nil {
		t.Fatal(err)
	}
	if err := b.up.Wait(); err != nil {
		t.Fatal(err)
	}
	orphansBefore := mustCheckClean(t, r).OrphanPacks
	if orphansBefore == 0 {
		t.Fatal("the unfinished backup left no pack to clear")
	}
	res, err := Prune(ctx, r, PruneOptions{Grace: time.Hour, MarkAge: -1})
	if err != nil || res.Deleted != 0 {
		t.Fatalf("prune deleted the pack of a backup that may still be running: %+v %v", res, err)
	}
	if _, err = Prune(ctx, r, PruneOptions{Grace: -time.Hour, MarkAge: -1}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := repo.Open(ctx, store, []byte(pw))
	if c := mustCheckClean(t, fresh); c.OrphanPacks != 0 {
		t.Errorf("%d orphan packs left", c.OrphanPacks)
	}
	if _, err := Restore(ctx, fresh, s.Snapshot, t.TempDir(), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Index files are cached locally: opening the storage again downloads only
// the index files it hasn't seen.
func TestIndexCache(t *testing.T) {
	t.Setenv("BP_CACHE_DIR", t.TempDir())
	t.Setenv("BP_NO_CACHE", "")
	ctx := context.Background()
	store, _ := newCountingStore(t)
	r, err := repo.Init(ctx, store, []byte(pw), testParams)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		commitPayload(t, mustBuilder(t, r), "x", randomPayload(200<<10))
	}
	store.reset()
	again, _ := repo.Open(ctx, store, []byte(pw))
	if err := again.LoadIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if store.gets["index"] != 0 {
		t.Errorf("downloaded %d index files that were cached", store.gets["index"])
	}
	// A damaged cache copy is noticed and replaced.
	cache := filepath.Join(os.Getenv("BP_CACHE_DIR"), "index", r.Config().ID)
	entries, _ := os.ReadDir(cache)
	if len(entries) != 3 {
		t.Fatalf("%d cached index files, want 3", len(entries))
	}
	os.WriteFile(filepath.Join(cache, entries[0].Name()), []byte("garbage"), 0o600)
	store.reset()
	third, _ := repo.Open(ctx, store, []byte(pw))
	if err := third.LoadIndex(ctx); err != nil {
		t.Fatalf("a damaged cache broke loading: %v", err)
	}
	if store.gets["index"] != 1 {
		t.Errorf("%d index downloads after damaging one cached file, want 1", store.gets["index"])
	}
}
