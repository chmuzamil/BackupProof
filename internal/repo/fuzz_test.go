package repo

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
)

func fuzzRepo(f *testing.F) (*Repo, backend.Backend) {
	f.Helper()
	be, err := backend.NewLocal(filepath.Join(f.TempDir(), "repo"))
	if err != nil {
		f.Fatal(err)
	}
	r, err := Init(context.Background(), be, []byte("fuzz password"), chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10})
	if err != nil {
		f.Fatal(err)
	}
	return r, be
}

// Whatever storage hands back for a chunk, opening it never panics, and only
// the exact bytes that were stored for that chunk are accepted.
func FuzzOpenBlob(f *testing.F) {
	r, be := fuzzRepo(f)
	ctx := context.Background()
	plain := bytes.Repeat([]byte("chunk data "), 200)
	id, _, err := r.PutBlob(ctx, plain)
	if err != nil {
		f.Fatal(err)
	}
	stored, err := be.Get(ctx, DataKey(id))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(stored)
	f.Add([]byte{})
	f.Add([]byte{FormatVersion})
	f.Add(append([]byte{FormatVersion}, make([]byte, 40)...))
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := r.OpenBlob(id, raw)
		if err == nil && (!bytes.Equal(raw, stored) || !bytes.Equal(got, plain)) {
			t.Fatalf("accepted %d bytes that aren't the stored chunk", len(raw))
		}
	})
}

// A damaged or hostile config or key file makes opening the storage fail
// with an error, never a panic.
func FuzzOpenStorage(f *testing.F) {
	_, be := fuzzRepo(f)
	ctx := context.Background()
	config, err := be.Get(ctx, "config")
	if err != nil {
		f.Fatal(err)
	}
	var keyName string
	_ = be.List(ctx, "keys/", func(o backend.ObjectInfo) error { keyName = o.Key; return nil })
	key, err := be.Get(ctx, keyName)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(config, key)
	f.Add([]byte("{}"), key)
	f.Add(config, []byte("{}"))
	f.Add([]byte(`{"format":"backupproof","version":99}`), key)
	f.Fuzz(func(t *testing.T, cfg, k []byte) {
		dir := t.TempDir()
		fb, err := backend.NewLocal(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := fb.Put(ctx, "config", cfg); err != nil {
			t.Fatal(err)
		}
		if err := fb.Put(ctx, keyName, k); err != nil {
			t.Fatal(err)
		}
		// Each fuzz worker makes its own storage, so the seeds' key may not
		// be this process's: the rule here is only that nothing panics.
		if r, err := Open(ctx, fb, []byte("fuzz password")); err == nil {
			r.Close()
		}
	})
}
