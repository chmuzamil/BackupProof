package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/chmuzamil/backupproof/internal/chunker"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
)

// manyChunks is storage holding n chunks that only lists them, as a cloud
// service would page through them 1,000 at a time.
type manyChunks struct {
	backend.Backend
	n     int
	pages int
}

// Get finds nothing else: no prune marks.
func (m *manyChunks) Get(context.Context, string) ([]byte, error) { return nil, backend.ErrNotFound }

func (m *manyChunks) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	var id bpcrypto.ID
	for i := range m.n {
		if i%1000 == 0 {
			m.pages++
		}
		copy(id[:], fmt.Appendf(nil, "%032d", i))
		name := hex.EncodeToString(id[:])
		if err := fn(backend.ObjectInfo{Key: "data/" + name[:2] + "/" + name, Size: 1 << 20}); err != nil {
			return err
		}
	}
	return nil
}

// How long and how much memory a backup spends before it starts, listing a
// storage of BP_SCALE chunks (1,000,000 is about 1 TB at the average chunk
// size). Opt-in, since it's slow:
//
//	BP_SCALE=1000000 go test -run TestIndexScale -v ./internal/repo/
func TestIndexScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("BP_SCALE"))
	if n <= 0 {
		t.Skip("set BP_SCALE to a chunk count to run")
	}
	store := &manyChunks{n: n}
	r := &Repo{be: store, index: map[bpcrypto.ID]struct{}{}}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	if err := r.LoadIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if len(r.index) != n {
		t.Fatalf("indexed %d of %d chunks", len(r.index), n)
	}
	t.Logf("%d chunks: listed and indexed in %s here, %d MB of memory held, %d list requests to a cloud service (at 50-100 ms each: %s-%s before every backup starts)",
		n, took.Round(time.Millisecond), (after.HeapAlloc-before.HeapAlloc)>>20, store.pages,
		(time.Duration(store.pages) * 50 * time.Millisecond).Round(time.Second), (time.Duration(store.pages) * 100 * time.Millisecond).Round(time.Second))
}

// The same measurement for format v2: a backup reads index files instead of
// listing every blob. BP_SCALE blobs are written into index files of 100,000
// (as many backups, or one prune, would leave them), then loaded fresh.
//
//	BP_SCALE=1000000 go test -run TestIndexScaleV2 -v ./internal/repo/
func TestIndexScaleV2(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("BP_SCALE"))
	if n <= 0 {
		t.Skip("set BP_SCALE to a chunk count to run")
	}
	t.Setenv("BP_NO_CACHE", "1")
	ctx := context.Background()
	be, err := backend.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := Init(ctx, be, []byte("scale test password"), chunkerParamsForScale)
	if err != nil {
		t.Fatal(err)
	}
	var id bpcrypto.ID
	var pack bpcrypto.ID
	files := 0
	for start := 0; start < n; start += 100000 {
		var blobs []PackEntry
		for i := start; i < min(start+100000, n); i++ {
			rand.Read(id[:]) // real blob names are random-looking, and don't compress
			if i%16 == 0 {
				rand.Read(pack[:])
			}
			blobs = append(blobs, PackEntry{ID: id, Off: int64(i%16) << 20, Len: 1 << 20})
		}
		if _, err := r.WriteIndex(ctx, IndexFile{Packs: []PackRecord{{ID: pack, Blobs: blobs}}}); err != nil {
			t.Fatal(err)
		}
		files++
	}
	var size int64
	_ = be.List(ctx, "index/", func(o backend.ObjectInfo) error { size += o.Size; return nil })

	fresh, err := Open(ctx, be, []byte("scale test password"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	if err := fresh.LoadIndex(ctx); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if len(fresh.locs) != n {
		t.Fatalf("indexed %d of %d", len(fresh.locs), n)
	}
	t.Logf("%d chunks in %d index files (%d MB stored, cached after the first read): loaded in %s here, %d MB of memory held, 1 list request and %d downloads to a cloud service the first time, 1 list request after that",
		n, files, size>>20, took.Round(time.Millisecond), (after.HeapAlloc-before.HeapAlloc)>>20, files)
}

var chunkerParamsForScale = chunker.Params{Min: 512 << 10, Avg: 1 << 20, Max: 8 << 20}
