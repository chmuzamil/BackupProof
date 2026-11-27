package repo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/transfer"
)

// Storage format v2: packs and an index.
//
// A pack is an object holding many blobs, each sealed exactly as a v1 blob
// object, one after another, followed by a sealed list of what the pack holds
// and a 4-byte big-endian length of that list:
//
//	sealed blob | sealed blob | ... | sealed header | uint32 header length
//
// The pack is named by the keyed hash of all its bytes, so a pack that was
// changed no longer matches its name, and each blob in it still has to match
// its own name after decryption. The header means a lost index can be rebuilt
// from the packs alone.
//
// An index file (index/<id>) is an encrypted list of packs and the blobs in
// each, with their offsets. Every backup writes one when it saves its
// snapshot, and prune replaces many with one. Reading the index files
// replaces listing every blob, which is what made very large storage slow.
// Blobs from v1 storage that was upgraded stay where they are and appear in
// the index with no pack.

const (
	// packTarget is the size at which a pack is closed and uploaded.
	packTarget = 16 << 20
	// packUploads is how many packs one writer uploads at once.
	packUploads = 4
	packHdrAAD  = "packhdr"
)

// blobLoc is where a blob is: pack is 1 + its place in Repo.packIDs, or 0
// for a v1 blob object (data/...). 12 bytes, so the location of a million
// blobs fits in tens of megabytes. A pack is far smaller than 4 GB.
type blobLoc struct {
	pack, off, len uint32
}

// packRef numbers a pack for blobLoc. Call with r.mu held.
func (r *Repo) packRef(id bpcrypto.ID) uint32 {
	var zero bpcrypto.ID
	if id == zero {
		return 0
	}
	if n, ok := r.packNum[id]; ok {
		return n
	}
	r.packIDs = append(r.packIDs, id)
	n := uint32(len(r.packIDs))
	r.packNum[id] = n
	return n
}

func (r *Repo) locOf(pack bpcrypto.ID, b PackEntry) blobLoc {
	return blobLoc{pack: r.packRef(pack), off: uint32(b.Off), len: uint32(b.Len)}
}

// PackEntry is one blob in a pack.
type PackEntry struct {
	ID  bpcrypto.ID `json:"id"`
	Off int64       `json:"off"`
	Len int64       `json:"len"`
}

// PackRecord lists a pack's blobs. An empty Pack ID lists v1 blob objects.
type PackRecord struct {
	ID    bpcrypto.ID `json:"pack"`
	Blobs []PackEntry `json:"blobs"`
}

// IndexFile is the content of one index/<id> object.
type IndexFile struct {
	Packs []PackRecord `json:"packs"`
}

type packBuf struct {
	data  []byte
	blobs []PackEntry
}

// PackKey is where a pack is stored.
func PackKey(id bpcrypto.ID) string {
	s := id.String()
	return "packs/" + s[:2] + "/" + s
}

// addToPack adds a sealed blob to the pack being filled, and uploads that
// pack once it is full. An error means the blob isn't stored.
func (r *Repo) addToPack(ctx context.Context, id bpcrypto.ID, sealed []byte) error {
	r.packMu.Lock()
	if r.cur == nil {
		r.cur = &packBuf{data: make([]byte, 0, packTarget+packTarget/8)}
	}
	r.cur.blobs = append(r.cur.blobs, PackEntry{ID: id, Off: int64(len(r.cur.data)), Len: int64(len(sealed))})
	r.cur.data = append(r.cur.data, sealed...)
	var full *packBuf
	if len(r.cur.data) >= packTarget {
		full, r.cur = r.cur, nil
	}
	r.packMu.Unlock()
	if full == nil {
		return nil
	}
	return r.uploadPack(ctx, full)
}

// uploadPack seals the pack's header, uploads it and records where its blobs
// are. On failure none of its blobs count as stored.
func (r *Repo) uploadPack(ctx context.Context, p *packBuf) error {
	select {
	case r.packSem <- struct{}{}:
	case <-ctx.Done():
		r.forget(p.blobs)
		return ctx.Err()
	}
	defer func() { <-r.packSem }()
	hdr, err := json.Marshal(p.blobs)
	if err != nil {
		return err
	}
	sealedHdr, err := r.keys.Seal(hdr, []byte(packHdrAAD))
	if err != nil {
		r.forget(p.blobs)
		return err
	}
	data := append(p.data, sealedHdr...)
	data = binary.BigEndian.AppendUint32(data, uint32(len(sealedHdr)))
	packID := r.keys.ContentID(data)
	if err := r.put(ctx, PackKey(packID), data); err != nil {
		r.forget(p.blobs)
		return fmt.Errorf("uploading a pack of %d blobs: %w", len(p.blobs), err)
	}
	r.mu.Lock()
	for _, b := range p.blobs {
		r.locs[b.ID] = r.locOf(packID, b)
	}
	r.mu.Unlock()
	r.packMu.Lock()
	r.done = append(r.done, PackRecord{ID: packID, Blobs: p.blobs})
	r.packMu.Unlock()
	return nil
}

// forget releases the claims of blobs that didn't get stored.
func (r *Repo) forget(blobs []PackEntry) {
	r.mu.Lock()
	for _, b := range blobs {
		delete(r.index, b.ID)
	}
	r.mu.Unlock()
}

// Flush uploads the pack being filled and writes an index file for every pack
// uploaded since the last Flush. snapshot.Save calls it, so a snapshot is
// only saved once everything it references is stored and indexed.
func (r *Repo) Flush(ctx context.Context) error {
	if r.cfg.Version < 2 {
		return nil
	}
	r.packMu.Lock()
	last := r.cur
	r.cur = nil
	r.packMu.Unlock()
	if last != nil && len(last.blobs) > 0 {
		if err := r.uploadPack(ctx, last); err != nil {
			return err
		}
	}
	r.packMu.Lock()
	done := r.done
	r.done = nil
	r.packMu.Unlock()
	if len(done) == 0 {
		return nil
	}
	if _, err := r.WriteIndex(ctx, IndexFile{Packs: done}); err != nil {
		r.packMu.Lock()
		r.done = append(done, r.done...)
		r.packMu.Unlock()
		return fmt.Errorf("writing the index: %w", err)
	}
	return nil
}

// WriteIndex stores an index file and keeps a copy in the local cache.
func (r *Repo) WriteIndex(ctx context.Context, f IndexFile) (bpcrypto.ID, error) {
	plain, err := json.Marshal(f)
	if err != nil {
		return bpcrypto.ID{}, err
	}
	id := r.keys.ContentID(plain)
	sealed, err := r.seal("index", id, plain)
	if err != nil {
		return id, err
	}
	if err := r.put(ctx, "index/"+id.String(), sealed); err != nil {
		return id, err
	}
	r.cachePut(id, sealed)
	return id, nil
}

// IndexFileWithID is an index file and its name.
type IndexFileWithID struct {
	ID bpcrypto.ID
	IndexFile
}

// ReadIndexes reads every index file in the storage, using the local cache
// for files already seen (index files never change once written).
func (r *Repo) ReadIndexes(ctx context.Context) ([]IndexFileWithID, error) {
	ids, err := r.ListIDs(ctx, "index")
	if err != nil {
		return nil, err
	}
	out := make([]IndexFileWithID, len(ids))
	errs := make([]error, len(ids))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i].ID = id
			errs[i] = r.readIndex(ctx, id, &out[i].IndexFile)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	r.cachePrune(ids)
	return out, nil
}

func (r *Repo) readIndex(ctx context.Context, id bpcrypto.ID, f *IndexFile) error {
	raw, cached := r.cacheGet(id)
	if !cached {
		var err error
		if raw, err = r.get(ctx, "index/"+id.String()); err != nil {
			return fmt.Errorf("index %s: %w", id.Short(), err)
		}
	}
	plain, err := r.open("index", id, raw)
	if err == nil && r.keys.ContentID(plain) != id {
		err = fmt.Errorf("index %s: content hash mismatch", id.Short())
	}
	if err != nil {
		if cached { // a damaged cache copy: fetch the real one
			r.cacheDrop(id)
			return r.readIndex(ctx, id, f)
		}
		return err
	}
	if !cached {
		r.cachePut(id, raw)
	}
	return json.Unmarshal(plain, f)
}

// loadIndexV2 reads the index files, for both reading and reusing blobs.
// Blobs waiting to be deleted by prune are readable but not reused.
func (r *Repo) loadIndexV2(ctx context.Context) error {
	r.mu.Lock()
	loaded := r.loaded
	r.mu.Unlock()
	if loaded {
		return nil
	}
	files, err := r.ReadIndexes(ctx)
	if err != nil {
		return err
	}
	marks, err := r.loadPruneMarks(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range files {
		for _, p := range f.Packs {
			ref := r.packRef(p.ID)
			if _, marked := marks["p:"+p.ID.String()]; marked && ref != 0 {
				r.skipPacks[ref] = true
			}
			for _, b := range p.Blobs {
				if _, ok := r.locs[b.ID]; !ok {
					r.locs[b.ID] = blobLoc{pack: ref, off: uint32(b.Off), len: uint32(b.Len)}
				}
				if _, marked := marks[b.ID.String()]; marked {
					r.skipBlobs[b.ID] = true
				}
			}
		}
	}
	r.loaded, r.locsRead = true, true
	return nil
}

// ensureLocs loads the index once before the first read.
func (r *Repo) ensureLocs(ctx context.Context) error {
	r.locsOnce.Lock()
	defer r.locsOnce.Unlock()
	r.mu.Lock()
	read := r.locsRead
	r.mu.Unlock()
	if read {
		return nil
	}
	return r.loadIndexV2(ctx)
}

// fetchV2 reads a sealed blob from its pack, or from its own object for a
// blob that came from v1 storage or isn't in any index.
func (r *Repo) fetchV2(ctx context.Context, id bpcrypto.ID, reloaded bool) ([]byte, error) {
	if err := r.ensureLocs(ctx); err != nil {
		return nil, fmt.Errorf("reading the index: %w", err)
	}
	r.mu.Lock()
	loc, ok := r.locs[id]
	var pack bpcrypto.ID
	if ok && loc.pack > 0 {
		pack = r.packIDs[loc.pack-1]
	}
	r.mu.Unlock()
	if !ok || loc.pack == 0 {
		raw, err := r.get(ctx, DataKey(id))
		if err != nil {
			return nil, fmt.Errorf("blob %s: %w", id, err)
		}
		return raw, nil
	}
	var raw []byte
	err := transfer.Do(ctx, func(ctx context.Context) error {
		var err error
		raw, err = r.be.GetRange(ctx, PackKey(pack), int64(loc.off), int64(loc.len))
		return err
	})
	if errors.Is(err, backend.ErrNotFound) && !reloaded {
		// A prune may have moved the blob to a new pack since the index was
		// read: read the index again and look once more.
		r.reloadLocs(ctx)
		return r.fetchV2(ctx, id, true)
	}
	if err != nil {
		return nil, fmt.Errorf("blob %s in pack %s: %w", id, pack.Short(), err)
	}
	return raw, nil
}

// reloadLocs re-reads the index for reading; reuse decisions stay as loaded.
func (r *Repo) reloadLocs(ctx context.Context) {
	files, err := r.ReadIndexes(ctx)
	if err != nil {
		return
	}
	r.mu.Lock()
	for _, f := range files {
		for _, p := range f.Packs {
			for _, b := range p.Blobs {
				r.locs[b.ID] = r.locOf(p.ID, b)
			}
		}
	}
	r.mu.Unlock()
}

// StoredBlob is a blob the index knows about.
type StoredBlob struct {
	Pack bpcrypto.ID // zero for a v1 blob object
	Size int64
	// Modified is when a v1 blob object was written (zero for packed blobs,
	// whose packs carry the time).
	Modified time.Time
}

// StoredBlobs lists the blobs in the storage: in v1 by listing every blob
// object, in v2 from the index.
func (r *Repo) StoredBlobs(ctx context.Context) (map[bpcrypto.ID]StoredBlob, error) {
	out := map[bpcrypto.ID]StoredBlob{}
	if r.cfg.Version < 2 {
		err := r.be.List(ctx, "data/", func(o backend.ObjectInfo) error {
			name := o.Key[strings.LastIndex(o.Key, "/")+1:]
			if id, err := bpcrypto.ParseID(name); err == nil {
				out[id] = StoredBlob{Size: o.Size, Modified: o.Modified}
			}
			return nil
		})
		return out, err
	}
	files, err := r.ReadIndexes(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		for _, p := range f.Packs {
			for _, b := range p.Blobs {
				out[b.ID] = StoredBlob{Pack: p.ID, Size: b.Len}
			}
		}
	}
	return out, nil
}

// Packs lists the pack objects in the storage.
func (r *Repo) Packs(ctx context.Context) (map[bpcrypto.ID]backend.ObjectInfo, error) {
	out := map[bpcrypto.ID]backend.ObjectInfo{}
	err := r.be.List(ctx, "packs/", func(o backend.ObjectInfo) error {
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		if id, err := bpcrypto.ParseID(name); err == nil {
			out[id] = o
		}
		return nil
	})
	return out, err
}

// FetchSealed reads a blob exactly as stored, for repacking it unchanged.
func (r *Repo) FetchSealed(ctx context.Context, pack bpcrypto.ID, e PackEntry) ([]byte, error) {
	var raw []byte
	err := transfer.Do(ctx, func(ctx context.Context) error {
		var err error
		raw, err = r.be.GetRange(ctx, PackKey(pack), e.Off, e.Len)
		return err
	})
	return raw, err
}

// RepackBlob writes an already sealed blob into a new pack, after checking it
// opens and matches its name. Prune uses it to keep the blobs still needed
// from a pack it is about to delete.
func (r *Repo) RepackBlob(ctx context.Context, id bpcrypto.ID, sealed []byte) error {
	if _, err := r.OpenBlob(id, sealed); err != nil {
		return err
	}
	return r.addToPack(ctx, id, sealed)
}

// TakeWritten returns and clears the packs uploaded since the last Flush or
// TakeWritten, after uploading the pack being filled. Prune folds them into
// the index it writes instead of writing a separate one.
func (r *Repo) TakeWritten(ctx context.Context) ([]PackRecord, error) {
	r.packMu.Lock()
	last := r.cur
	r.cur = nil
	r.packMu.Unlock()
	if last != nil && len(last.blobs) > 0 {
		if err := r.uploadPack(ctx, last); err != nil {
			return nil, err
		}
	}
	r.packMu.Lock()
	defer r.packMu.Unlock()
	done := r.done
	r.done = nil
	return done, nil
}

// Upgrade moves v1 storage to v2. Its blob objects stay where they are and
// are listed once into index files; from then on backups write packs and
// never list every blob again. Agents older than 0.4 can't use the storage
// afterwards, so upgrade them first.
func (r *Repo) Upgrade(ctx context.Context, log func(string, ...any)) error {
	if r.cfg.Version >= 2 {
		return fmt.Errorf("this storage already uses format v%d", r.cfg.Version)
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	const perFile = 100000
	var batch []PackEntry
	files, blobs := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := r.WriteIndex(ctx, IndexFile{Packs: []PackRecord{{Blobs: batch}}}); err != nil {
			return err
		}
		files++
		batch = nil
		return nil
	}
	err := r.be.List(ctx, "data/", func(o backend.ObjectInfo) error {
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		id, err := bpcrypto.ParseID(name)
		if err != nil {
			return nil
		}
		batch = append(batch, PackEntry{ID: id, Len: o.Size})
		blobs++
		if len(batch) >= perFile {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return fmt.Errorf("indexing the existing blobs: %w", err)
	}
	log("indexed %d existing blobs in %d index files", blobs, files)
	cfg := r.cfg
	cfg.Version = 2
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	// The config is written last: until then the storage is still v1 and
	// the extra index files are ignored.
	if err := r.put(ctx, "config", raw); err != nil {
		return fmt.Errorf("writing the new format version: %w", err)
	}
	r.cfg = cfg
	r.mu.Lock()
	r.loaded, r.locsRead = false, false
	r.index = map[bpcrypto.ID]struct{}{}
	r.locs = map[bpcrypto.ID]blobLoc{}
	r.mu.Unlock()
	log("the storage now uses format v2")
	return nil
}

// --- local index cache -------------------------------------------------------

// defaultCacheDir is where index files are cached for one storage:
// BP_CACHE_DIR, or the user's cache folder. "" turns caching off.
func defaultCacheDir(repoID string) string {
	if os.Getenv("BP_NO_CACHE") != "" || repoID == "" {
		return ""
	}
	base := os.Getenv("BP_CACHE_DIR")
	if base == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(d, "backupproof")
	}
	return filepath.Join(base, "index", repoID)
}

func (r *Repo) cachePath(id bpcrypto.ID) string { return filepath.Join(r.cacheDir, id.String()) }

func (r *Repo) cacheGet(id bpcrypto.ID) ([]byte, bool) {
	if r.cacheDir == "" {
		return nil, false
	}
	b, err := os.ReadFile(r.cachePath(id))
	return b, err == nil
}

// cachePut stores a sealed index file. Failures only cost a later download.
func (r *Repo) cachePut(id bpcrypto.ID, sealed []byte) {
	if r.cacheDir == "" {
		return
	}
	if os.MkdirAll(r.cacheDir, 0o700) != nil {
		return
	}
	tmp := r.cachePath(id) + ".tmp"
	if os.WriteFile(tmp, sealed, 0o600) == nil {
		_ = os.Rename(tmp, r.cachePath(id))
	}
}

func (r *Repo) cacheDrop(id bpcrypto.ID) {
	if r.cacheDir != "" {
		_ = os.Remove(r.cachePath(id))
	}
}

// cachePrune removes cached index files the storage no longer has.
func (r *Repo) cachePrune(keep []bpcrypto.ID) {
	if r.cacheDir == "" {
		return
	}
	entries, err := os.ReadDir(r.cacheDir)
	if err != nil {
		return
	}
	live := make(map[string]bool, len(keep))
	for _, id := range keep {
		live[id.String()] = true
	}
	for _, e := range entries {
		if !live[e.Name()] {
			_ = os.Remove(filepath.Join(r.cacheDir, e.Name()))
		}
	}
}
