// Package repo implements the BackupProof repository format.
//
// Layout on any backend:
//
//	config                 plaintext: format, version, repo id, chunker params
//	keys/<id>              password slots wrapping the master keys (argon2id)
//	data/<id[:2]>/<id>     content blobs, keyed-BLAKE3 named, zstd + XChaCha20-Poly1305
//	snapshots/<id>         encrypted snapshot records
//	proofs/<hash>.json     signed attestations (DSSE); public, contain no file names
//
// Every blob is self-verifying: after decryption its keyed hash must equal its
// name, so silent corruption or substitution is always detected on read.
package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/transfer"
	"github.com/klauspost/compress/zstd"
)

const (
	FormatName    = "backupproof-repo"
	FormatVersion = 1
)

type Config struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	ID      string         `json:"id"`
	Created time.Time      `json:"created"`
	Chunker chunker.Params `json:"chunker"`
}

type Repo struct {
	be     backend.Backend
	cfg    Config
	keys   *bpcrypto.MasterKeys
	gear   *[256]uint64
	enc    *zstd.Encoder
	dec    *zstd.Decoder
	mu     sync.Mutex
	index  map[bpcrypto.ID]struct{}
	loaded bool
}

var ErrNotARepo = errors.New("no BackupProof repository at this location (run init first)")

// Init creates a new repository protected by password.
func Init(ctx context.Context, be backend.Backend, password []byte, params chunker.Params) (*Repo, error) {
	if _, err := be.Get(ctx, "config"); err == nil {
		return nil, errors.New("a repository already exists at this location")
	}
	if len(password) < 8 {
		return nil, errors.New("repository password must be at least 8 characters")
	}
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	cfg := Config{Format: FormatName, Version: FormatVersion, ID: hex.EncodeToString(idb), Created: time.Now().UTC(), Chunker: params}
	mk, err := bpcrypto.NewMasterKeys()
	if err != nil {
		return nil, err
	}
	if err := addKey(ctx, be, mk, password); err != nil {
		return nil, err
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := be.Put(ctx, "config", raw); err != nil {
		return nil, err
	}
	return newRepo(be, cfg, mk)
}

func addKey(ctx context.Context, be backend.Backend, mk *bpcrypto.MasterKeys, password []byte) error {
	kdf, err := bpcrypto.DefaultKDF()
	if err != nil {
		return err
	}
	kf, err := bpcrypto.WrapKeys(mk, password, kdf)
	if err != nil {
		return err
	}
	kf.Created = time.Now().UTC().Format(time.RFC3339)
	kf.Host, _ = os.Hostname()
	raw, _ := json.MarshalIndent(kf, "", "  ")
	id := make([]byte, 8)
	rand.Read(id)
	return be.Put(ctx, "keys/"+hex.EncodeToString(id), raw)
}

// AddPassword adds another password slot (e.g. for a break-glass recovery key).
func (r *Repo) AddPassword(ctx context.Context, password []byte) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return addKey(ctx, r.be, r.keys, password)
}

// Open unlocks an existing repository.
func Open(ctx context.Context, be backend.Backend, password []byte) (*Repo, error) {
	raw, err := be.Get(ctx, "config")
	if errors.Is(err, backend.ErrNotFound) {
		return nil, ErrNotARepo
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Format != FormatName {
		return nil, ErrNotARepo
	}
	if cfg.Version > FormatVersion {
		return nil, fmt.Errorf("repository format v%d is newer than this binary supports (v%d)", cfg.Version, FormatVersion)
	}
	var slots []string
	if err := be.List(ctx, "keys/", func(o backend.ObjectInfo) error {
		slots = append(slots, o.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, errors.New("repository has no key slots")
	}
	for _, slot := range slots {
		raw, err := be.Get(ctx, slot)
		if err != nil {
			continue
		}
		var kf bpcrypto.KeyFile
		if json.Unmarshal(raw, &kf) != nil {
			continue
		}
		if mk, err := bpcrypto.UnwrapKeys(&kf, password); err == nil {
			return newRepo(be, cfg, mk)
		}
	}
	return nil, bpcrypto.ErrWrongPassword
}

func newRepo(be backend.Backend, cfg Config, mk *bpcrypto.MasterKeys) (*Repo, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	return &Repo{be: be, cfg: cfg, keys: mk, gear: chunker.GearTable(mk.Seed), enc: enc, dec: dec, index: map[bpcrypto.ID]struct{}{}}, nil
}

func (r *Repo) Config() Config                { return r.cfg }
func (r *Repo) Backend() backend.Backend      { return r.be }
func (r *Repo) Gear() *[256]uint64            { return r.gear }
func (r *Repo) ChunkerParams() chunker.Params { return r.cfg.Chunker }
func (r *Repo) ContentID(b []byte) bpcrypto.ID {
	return r.keys.ContentID(b)
}

func DataKey(id bpcrypto.ID) string {
	s := id.String()
	return "data/" + s[:2] + "/" + s
}

const (
	compNone byte = 0
	compZstd byte = 1
)

func (r *Repo) seal(kind string, id bpcrypto.ID, plain []byte) ([]byte, error) {
	compressed := r.enc.EncodeAll(plain, make([]byte, 0, len(plain)/2+64))
	inner := make([]byte, 0, len(compressed)+1)
	if len(compressed) < len(plain) {
		inner = append(append(inner, compZstd), compressed...)
	} else {
		inner = append(append(inner, compNone), plain...)
	}
	ct, err := r.keys.Seal(inner, []byte(kind+"/"+id.String()))
	if err != nil {
		return nil, err
	}
	return append([]byte{FormatVersion}, ct...), nil
}

func (r *Repo) open(kind string, id bpcrypto.ID, raw []byte) ([]byte, error) {
	if len(raw) < 1 || raw[0] != FormatVersion {
		return nil, fmt.Errorf("%s %s: unknown blob version", kind, id.Short())
	}
	inner, err := r.keys.Open(raw[1:], []byte(kind+"/"+id.String()))
	if err != nil {
		return nil, fmt.Errorf("%s %s: authentication failed (corrupt or tampered)", kind, id.Short())
	}
	if len(inner) == 0 {
		return nil, fmt.Errorf("%s %s: empty payload", kind, id.Short())
	}
	switch inner[0] {
	case compNone:
		return inner[1:], nil
	case compZstd:
		return r.dec.DecodeAll(inner[1:], nil)
	}
	return nil, fmt.Errorf("%s %s: unknown compression %d", kind, id.Short(), inner[0])
}

// LoadIndex lists all data blobs so that dedup checks are in-memory.
func (r *Repo) LoadIndex(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return nil
	}
	err := r.be.List(ctx, "data/", func(o backend.ObjectInfo) error {
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		if id, err := bpcrypto.ParseID(name); err == nil {
			r.index[id] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.loaded = true
	return nil
}

// HasBlob reports whether a data blob is stored (after LoadIndex).
func (r *Repo) HasBlob(id bpcrypto.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.index[id]
	return ok
}

// PutBlob stores plain data and returns its ID and the number of bytes
// uploaded (0 when the blob was already present).
func (r *Repo) PutBlob(ctx context.Context, plain []byte) (bpcrypto.ID, int, error) {
	id := r.keys.ContentID(plain)
	// Claim the ID before uploading so concurrent writers of identical
	// content upload it once. A failed upload releases the claim and the
	// error aborts the snapshot, so no snapshot can reference a missing blob.
	r.mu.Lock()
	if _, ok := r.index[id]; ok {
		r.mu.Unlock()
		return id, 0, nil
	}
	r.index[id] = struct{}{}
	r.mu.Unlock()
	sealed, err := r.seal("data", id, plain)
	if err == nil {
		err = r.put(ctx, DataKey(id), sealed)
	}
	if err != nil {
		r.mu.Lock()
		delete(r.index, id)
		r.mu.Unlock()
		return id, 0, err
	}
	return id, len(sealed), nil
}

// get and put read and write one object, retrying transient failures.
func (r *Repo) get(ctx context.Context, key string) ([]byte, error) {
	var raw []byte
	err := transfer.Do(ctx, func(ctx context.Context) error {
		var err error
		raw, err = r.be.Get(ctx, key)
		return err
	})
	return raw, err
}

func (r *Repo) put(ctx context.Context, key string, data []byte) error {
	return transfer.Do(ctx, func(ctx context.Context) error { return r.be.Put(ctx, key, data) })
}

// GetBlob reads, decrypts and verifies a blob.
func (r *Repo) GetBlob(ctx context.Context, id bpcrypto.ID) ([]byte, error) {
	raw, err := r.FetchBlob(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.OpenBlob(id, raw)
}

// FetchBlob downloads a blob without opening it, so downloading and
// decrypting can run in separate stages.
func (r *Repo) FetchBlob(ctx context.Context, id bpcrypto.ID) ([]byte, error) {
	raw, err := r.get(ctx, DataKey(id))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", id, err)
	}
	return raw, nil
}

// OpenBlob decrypts and verifies a blob from FetchBlob: the authentication
// tag must hold and the content must hash to its ID.
func (r *Repo) OpenBlob(id bpcrypto.ID, raw []byte) ([]byte, error) {
	plain, err := r.open("data", id, raw)
	if err != nil {
		return nil, err
	}
	if r.keys.ContentID(plain) != id {
		return nil, fmt.Errorf("blob %s: content hash mismatch", id.Short())
	}
	return plain, nil
}

// PutJSON / GetJSON store encrypted records such as snapshots.
func (r *Repo) PutJSON(ctx context.Context, kind string, v any) (bpcrypto.ID, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return bpcrypto.ID{}, err
	}
	id := r.keys.ContentID(plain)
	sealed, err := r.seal(kind, id, plain)
	if err != nil {
		return id, err
	}
	return id, r.put(ctx, kind+"/"+id.String(), sealed)
}

func (r *Repo) GetJSON(ctx context.Context, kind string, id bpcrypto.ID, v any) error {
	raw, err := r.get(ctx, kind+"/"+id.String())
	if err != nil {
		return err
	}
	plain, err := r.open(kind, id, raw)
	if err != nil {
		return err
	}
	if r.keys.ContentID(plain) != id {
		return fmt.Errorf("%s %s: content hash mismatch", kind, id.Short())
	}
	return json.Unmarshal(plain, v)
}

func (r *Repo) ListIDs(ctx context.Context, kind string) ([]bpcrypto.ID, error) {
	var ids []bpcrypto.ID
	err := r.be.List(ctx, kind+"/", func(o backend.ObjectInfo) error {
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		if id, err := bpcrypto.ParseID(name); err == nil {
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// ResolveID expands a unique hex prefix to a full ID.
func (r *Repo) ResolveID(ctx context.Context, kind, prefix string) (bpcrypto.ID, error) {
	ids, err := r.ListIDs(ctx, kind)
	if err != nil {
		return bpcrypto.ID{}, err
	}
	var found []bpcrypto.ID
	for _, id := range ids {
		if strings.HasPrefix(id.String(), strings.ToLower(prefix)) {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		return bpcrypto.ID{}, fmt.Errorf("no %s matches %q", kind, prefix)
	case 1:
		return found[0], nil
	}
	return bpcrypto.ID{}, fmt.Errorf("%q is ambiguous (%d matches)", prefix, len(found))
}

// PutPublic stores an unencrypted object (used for signed proofs).
func (r *Repo) PutPublic(ctx context.Context, key string, data []byte) error {
	return r.be.Put(ctx, key, data)
}

func (r *Repo) Close() error {
	r.enc.Close()
	r.dec.Close()
	return r.be.Close()
}
