package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/transfer"
)

// fakeStore is storage that misbehaves like a real cloud service can: slow
// and out-of-order responses, 503s with Retry-After, dropped connections,
// corrupted or missing chunks, and uploads that start failing.
type fakeStore struct {
	backend.Backend
	latency func() time.Duration

	mu           sync.Mutex
	busy         map[string]int // data key → 503s left
	drops        map[string]int // data key → dropped connections left
	corrupt      map[string]bool
	putFailAfter int // data puts allowed before every later one fails
	puts         int

	now, max atomic.Int32
	gets     atomic.Int64
	bytes    atomic.Int64
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	local, err := backend.NewLocal(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStore{Backend: local, busy: map[string]int{}, drops: map[string]int{}, corrupt: map[string]bool{}}
}

func randDur(max time.Duration) time.Duration {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max)+1))
	return time.Duration(n.Int64())
}

func (f *fakeStore) Get(ctx context.Context, key string) ([]byte, error) {
	if !strings.HasPrefix(key, "data/") {
		return f.Backend.Get(ctx, key)
	}
	n := f.now.Add(1)
	defer f.now.Add(-1)
	for m := f.max.Load(); n > m && !f.max.CompareAndSwap(m, n); m = f.max.Load() {
	}
	f.gets.Add(1)
	if f.latency != nil {
		t := time.NewTimer(f.latency())
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	f.mu.Lock()
	if f.busy[key] > 0 {
		f.busy[key]--
		f.mu.Unlock()
		return nil, &transfer.StatusError{Code: 503, RetryAfter: 5 * time.Millisecond}
	}
	if f.drops[key] > 0 {
		f.drops[key]--
		f.mu.Unlock()
		return nil, io.ErrUnexpectedEOF
	}
	bad := f.corrupt[key]
	f.mu.Unlock()
	data, err := f.Backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if bad {
		data = append([]byte(nil), data...)
		data[len(data)/2] ^= 0xff
	}
	f.bytes.Add(int64(len(data)))
	return data, nil
}

func (f *fakeStore) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasPrefix(key, "data/") {
		f.mu.Lock()
		f.puts++
		fail := f.putFailAfter > 0 && f.puts > f.putFailAfter
		f.mu.Unlock()
		if fail {
			return &transfer.StatusError{Code: 403, Err: errors.New("access denied")}
		}
	}
	return f.Backend.Put(ctx, key, data)
}

func (f *fakeStore) dataKeys(t *testing.T) []string {
	var keys []string
	_ = f.List(context.Background(), "data/", func(o backend.ObjectInfo) error {
		keys = append(keys, o.Key)
		return nil
	})
	return keys
}

func fastRetries(t *testing.T) {
	old := transfer.BackoffBase
	transfer.BackoffBase = time.Millisecond
	t.Cleanup(func() { transfer.BackoffBase = old })
}

// testSnapshot backs up files of several sizes (one twice, so chunks repeat),
// an empty file and a stream.
func testSnapshot(t *testing.T, r *repo.Repo) (*snapshot.Snapshot, map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	big := make([]byte, 700<<10)
	rand.Read(big)
	files := map[string][]byte{
		"big.bin":      big,
		"copy/big.bin": big,
		"small.txt":    []byte("hello"),
		"empty":        {},
	}
	for i := 0; i < 20; i++ {
		b := make([]byte, 20<<10)
		rand.Read(b)
		files[filepath.ToSlash(filepath.Join("many", string(rune('a'+i))+".bin"))] = b
	}
	for name, data := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := NewBuilder(ctx, r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddTree(ctx, src, "files"); err != nil {
		t.Fatal(err)
	}
	stream := make([]byte, 400<<10)
	rand.Read(stream)
	if _, err := b.AddReader(ctx, "dump.sql", 0o600, time.Now(), bytes.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	s, err := b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "t", Kind: "files"}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{"dump.sql": stream}
	for name, data := range files {
		out["files/"+name] = data
	}
	return s.Snapshot, out
}

func withSettings(s transfer.Settings) context.Context {
	return transfer.WithSettings(context.Background(), s)
}

// A parallel restore through slow, reordering, throttling and flaky storage
// gives exactly the files a one-at-a-time restore gives, within the memory cap.
func TestParallelRestoreMatchesSequential(t *testing.T) {
	fastRetries(t)
	fs := newFakeStore(t)
	r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	s, want := testSnapshot(t, r)
	keys := fs.dataKeys(t)
	fs.mu.Lock()
	for i, k := range keys {
		switch i % 5 {
		case 1:
			fs.busy[k] = 2
		case 3:
			fs.drops[k] = 1
		}
	}
	fs.mu.Unlock()
	fs.latency = func() time.Duration { return randDur(8 * time.Millisecond) }

	var peak atomic.Int64
	fetchClosed = func(f *Fetcher) { peak.Store(max(peak.Load(), f.Peak())) }
	t.Cleanup(func() { fetchClosed = nil })

	seqDir, parDir := t.TempDir(), t.TempDir()
	if _, err := Restore(withSettings(transfer.Settings{Concurrency: 1}), r, s, seqDir, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if fs.max.Load() != 1 {
		t.Errorf("concurrency 1 had %d reads in flight", fs.max.Load())
	}
	fs.max.Store(0)
	const capBytes = 256 << 10
	if _, err := Restore(withSettings(transfer.Settings{Concurrency: 16, MaxInflight: capBytes}), r, s, parDir, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if fs.max.Load() < 2 {
		t.Errorf("parallel restore had only %d reads in flight", fs.max.Load())
	}
	for name, data := range want {
		a, _ := os.ReadFile(filepath.Join(seqDir, filepath.FromSlash(name)))
		b, _ := os.ReadFile(filepath.Join(parDir, filepath.FromSlash(name)))
		if !bytes.Equal(a, data) || !bytes.Equal(b, data) {
			t.Errorf("%s differs (sequential %d bytes, parallel %d, want %d)", name, len(a), len(b), len(data))
		}
	}
	if root, _, _ := TreeRoot(parDir); root != s.Root {
		t.Error("parallel restore's tree root doesn't match the snapshot")
	}
	// The cap may be exceeded only by the one chunk the reader is waiting for.
	if p := peak.Load(); p > capBytes+int64(testParams.Max) {
		t.Errorf("held %d bytes at once, cap %d", p, capBytes)
	}
}

// A stream (as for zip downloads) read through the Fetcher comes out in order
// and identical to reading one chunk at a time; repeated chunks are fetched once.
func TestFetcherStreamOrderAndDedup(t *testing.T) {
	fs := newFakeStore(t)
	r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := testSnapshot(t, r)
	entries, err := snapshot.ReadManifest(context.Background(), r, s)
	if err != nil {
		t.Fatal(err)
	}
	var wants []Want
	for _, e := range entries {
		wants = appendWants(wants, e)
	}
	fs.latency = func() time.Duration { return randDur(5 * time.Millisecond) }
	read := func(ctx context.Context) []byte {
		pf := NewFetcher(ctx, r, wants, FetchOptions{})
		defer pf.Close()
		var out bytes.Buffer
		for _, w := range wants {
			data, err := pf.Get(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			out.Write(data)
		}
		return out.Bytes()
	}
	seq := read(withSettings(transfer.Settings{Concurrency: 1}))
	par := read(withSettings(transfer.Settings{Concurrency: 8, MaxInflight: 4 << 20}))
	if !bytes.Equal(seq, par) {
		t.Fatal("parallel stream differs from sequential")
	}
	// A chunk listed again while its first download is still waiting is
	// downloaded once.
	var dup []Want
	for _, w := range wants[:10] {
		dup = append(dup, w, w)
	}
	fs.latency = func() time.Duration { return 20 * time.Millisecond }
	ctx := withSettings(transfer.Settings{Concurrency: 8, MaxInflight: 4 << 20})
	pf := NewFetcher(ctx, r, dup, FetchOptions{})
	before := fs.gets.Load()
	for _, w := range dup {
		if _, err := pf.Get(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
	}
	pf.Close()
	if got := fs.gets.Load() - before; got != 10 {
		t.Errorf("%d reads for 10 chunks each listed twice in a row", got)
	}
}

// A corrupted or missing chunk fails the restore, names the chunk, and
// leaves no file that looks complete.
func TestRestoreFailsOnBadChunk(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing"} {
		t.Run(mode, func(t *testing.T) {
			fastRetries(t)
			fs := newFakeStore(t)
			r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := testSnapshot(t, r)
			entries, _ := snapshot.ReadManifest(context.Background(), r, s)
			var victim *snapshot.Entry
			for _, e := range entries {
				if e.Path == "files/big.bin" {
					victim = e
				}
			}
			id, _ := bpcrypto.ParseID(victim.Chunks[len(victim.Chunks)/2])
			if mode == "corrupt" {
				fs.corrupt[repo.DataKey(id)] = true
			} else {
				_ = fs.Delete(context.Background(), repo.DataKey(id))
			}
			dir := t.TempDir()
			_, err = Restore(withSettings(transfer.Settings{Concurrency: 8}), r, s, dir, RestoreOptions{})
			if err == nil {
				t.Fatal("restore with a bad chunk succeeded")
			}
			if !strings.Contains(err.Error(), id.String()) {
				t.Errorf("error doesn't name chunk %s: %v", id, err)
			}
			for _, name := range []string{"files/big.bin", "files/copy/big.bin"} {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
					t.Errorf("%s exists after a failed restore", name)
				}
			}
			filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
				if strings.HasSuffix(p, ".bp-partial") {
					t.Errorf("partial file left behind: %s", p)
				}
				return nil
			})
		})
	}
}

// Stopping a restore part way leaves no goroutines behind.
func TestCancelLeavesNoGoroutines(t *testing.T) {
	fs := newFakeStore(t)
	r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := testSnapshot(t, r)
	fs.latency = func() time.Duration { return 30 * time.Millisecond }
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(withSettings(transfer.Settings{Concurrency: 16}), 80*time.Millisecond)
	defer cancel()
	if _, err := Restore(ctx, r, s, t.TempDir(), RestoreOptions{}); err == nil {
		t.Fatal("restore finished before it was cancelled")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Errorf("%d goroutines left behind:\n%s", n-before, buf[:runtime.Stack(buf, true)])
	}
}

// The download speed limit holds for all workers together.
func TestSpeedLimitIsGlobal(t *testing.T) {
	fs := newFakeStore(t)
	r0, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := testSnapshot(t, r0)
	const rate = 2 << 20 // 2 MiB/s
	r, err := repo.Open(context.Background(), backend.Throttle(fs, 0, rate), []byte("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	before := fs.bytes.Load()
	if _, err := Restore(withSettings(transfer.Settings{Concurrency: 32}), r, s, t.TempDir(), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := float64(fs.bytes.Load() - before)
	got := moved / time.Since(start).Seconds()
	if got > rate*1.10 || got < rate*0.90 {
		t.Errorf("moved %.0f bytes at %.2f MiB/s, limit 2 MiB/s", moved, got/(1<<20))
	}
}

// A backup whose uploads start failing part way writes no snapshot.
func TestFailedUploadWritesNoSnapshot(t *testing.T) {
	fastRetries(t)
	fs := newFakeStore(t)
	r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	fs.putFailAfter = 10
	ctx := withSettings(transfer.Settings{Concurrency: 8})
	b, err := NewBuilder(ctx, r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2<<20)
	rand.Read(data)
	_, addErr := b.AddReader(ctx, "big.bin", 0o600, time.Now(), bytes.NewReader(data))
	_, err = b.Commit(ctx, &snapshot.Snapshot{Source: snapshot.Source{Name: "t", Kind: "files"}})
	if err == nil && addErr == nil {
		t.Fatal("backup with failing uploads succeeded")
	}
	ids, _ := r.ListIDs(context.Background(), "snapshots")
	if len(ids) != 0 {
		t.Errorf("%d snapshots written after a failed upload", len(ids))
	}
}

// The upload side keeps its data under the memory cap and stores every
// chunk, in parallel.
func TestParallelBackupUploads(t *testing.T) {
	fs := newFakeStore(t)
	r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), testParams)
	if err != nil {
		t.Fatal(err)
	}
	s, want := testSnapshot(t, r)
	dir := t.TempDir()
	if _, err := Restore(context.Background(), r, s, dir, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, data := range want {
		if got, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); !bytes.Equal(got, data) {
			t.Errorf("%s differs after a parallel backup and restore", name)
		}
	}
}

// BenchmarkRestoreLatency restores 64 MiB in ~4 MiB chunks from storage with
// an 80 ms round trip, one chunk at a time and in parallel.
func BenchmarkRestoreLatency(b *testing.B) {
	params := chunker.Params{Min: 2 << 20, Avg: 4 << 20, Max: 8 << 20}
	local, _ := backend.NewLocal(filepath.Join(b.TempDir(), "repo"))
	fs := &fakeStore{Backend: local, busy: map[string]int{}, drops: map[string]int{}, corrupt: map[string]bool{}}
	r, err := repo.Init(context.Background(), fs, []byte("correct horse battery"), params)
	if err != nil {
		b.Fatal(err)
	}
	data := make([]byte, 64<<20)
	rand.Read(data)
	bld, _ := NewBuilder(context.Background(), r, Options{})
	bld.AddReader(context.Background(), "big.bin", 0o600, time.Now(), bytes.NewReader(data))
	s, err := bld.Commit(context.Background(), &snapshot.Snapshot{Source: snapshot.Source{Name: "b", Kind: "files"}})
	if err != nil {
		b.Fatal(err)
	}
	fs.latency = func() time.Duration { return 80 * time.Millisecond }
	for _, c := range []struct {
		name string
		s    transfer.Settings
	}{{"sequential", transfer.Settings{Concurrency: 1}}, {"adaptive", transfer.Settings{}}} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				if _, err := Restore(withSettings(c.s), r, s.Snapshot, b.TempDir(), RestoreOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
