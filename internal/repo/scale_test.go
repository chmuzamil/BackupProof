package repo

import (
	"context"
	"encoding/hex"
	"fmt"
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
