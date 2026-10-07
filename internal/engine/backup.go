// Package engine implements backup, restore, check and prune on top of the
// repository format.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chmuzamil/backupproof/internal/chunker"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"lukechampine.com/blake3"
)

// Version is stamped at release time with
// -ldflags "-X github.com/chmuzamil/backupproof/internal/engine.Version=backupproof/X.Y.Z".
var Version = "backupproof/0.1.1"

// Logger receives human readable progress lines.
type Logger func(format string, args ...any)

func nopLog(string, ...any) {}

// Builder accumulates files and streams into a single snapshot.
type Builder struct {
	r        *repo.Repo
	log      Logger
	entries  []*snapshot.Entry
	seen     map[string]bool
	parent   map[string]*snapshot.Entry
	parentID string
	stats    snapshot.Stats
	started  time.Time
	excludes []string
	deny     []string

	sem      chan struct{}
	wg       sync.WaitGroup
	errMu    sync.Mutex
	err      error
	newBytes atomic.Int64
	newChunk atomic.Int64
}

type Options struct {
	Parent    *snapshot.WithID // previous snapshot of the same source, for fast incrementals
	Excludes  []string         // glob patterns matched against base names and manifest paths
	DenyPaths []string         // never read these (and anything below them)
	Workers   int
	Log       Logger
}

func NewBuilder(ctx context.Context, r *repo.Repo, opts Options) (*Builder, error) {
	if err := r.LoadIndex(ctx); err != nil {
		return nil, fmt.Errorf("load index: %w", err)
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Log == nil {
		opts.Log = nopLog
	}
	b := &Builder{
		r: r, log: opts.Log, seen: map[string]bool{}, parent: map[string]*snapshot.Entry{},
		started: time.Now(), excludes: opts.Excludes, deny: opts.DenyPaths, sem: make(chan struct{}, opts.Workers),
	}
	if opts.Parent != nil {
		entries, err := snapshot.ReadManifest(ctx, r, opts.Parent.Snapshot)
		if err != nil {
			opts.Log("parent snapshot unreadable, doing a full backup: %v", err)
		} else {
			for _, e := range entries {
				b.parent[e.Path] = e
			}
			b.parentID = opts.Parent.ID.String()
		}
	}
	return b, nil
}

func (b *Builder) setErr(err error) {
	b.errMu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.errMu.Unlock()
}

func (b *Builder) failed() error {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	return b.err
}

func (b *Builder) add(e *snapshot.Entry) error {
	if !snapshot.SafeRelPath(e.Path) {
		return fmt.Errorf("unsafe manifest path %q", e.Path)
	}
	if b.seen[e.Path] {
		return nil
	}
	b.seen[e.Path] = true
	b.entries = append(b.entries, e)
	return nil
}

// addParents records the directory chain of p so restores recreate it.
func (b *Builder) addParents(p string) error {
	for dir := pathDir(p); dir != ""; dir = pathDir(dir) {
		if b.seen[dir] {
			return nil
		}
		if err := b.add(&snapshot.Entry{Path: dir, Type: snapshot.TypeDir}); err != nil {
			return err
		}
		b.stats.Dirs++
	}
	return nil
}

func pathDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

func (b *Builder) excluded(osPath, name string) bool {
	for _, pat := range b.excludes {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, filepath.ToSlash(osPath)); ok {
			return true
		}
	}
	return false
}

// AddPath walks a file or directory tree.
func (b *Builder) AddPath(ctx context.Context, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err != nil {
		return fmt.Errorf("source path %s: %w", root, err)
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			b.log("warning: skipping %s: %v", p, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e := b.failed(); e != nil {
			return e
		}
		// Checked per directory (and for the start path): files inside a denied
		// directory are never reached because the walk skips it.
		if (d.IsDir() || p == abs) && IsDenied(p, b.deny) {
			b.log("skipping %s: protected by this server", p)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != abs && b.excluded(p, d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			b.log("warning: skipping %s: %v", p, err)
			return nil
		}
		mp := snapshot.CleanPath(p)
		if mp == "" {
			return nil
		}
		if err := b.addParents(mp); err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				b.log("warning: skipping symlink %s: %v", p, err)
				return nil
			}
			return b.add(&snapshot.Entry{Path: mp, Type: snapshot.TypeSymlink, Link: filepath.ToSlash(target), MTime: info.ModTime().UnixNano()})
		case info.IsDir():
			if !b.seen[mp] {
				b.stats.Dirs++
			}
			return b.add(&snapshot.Entry{Path: mp, Type: snapshot.TypeDir, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()})
		case info.Mode().IsRegular():
			return b.addFile(ctx, p, mp, info)
		default:
			b.log("skipping special file %s", p)
			return nil
		}
	})
}

func (b *Builder) addFile(ctx context.Context, osPath, mp string, info fs.FileInfo) error {
	b.stats.Files++
	b.stats.Bytes += info.Size()
	if prev, ok := b.parent[mp]; ok && prev.Type == snapshot.TypeFile &&
		prev.Size == info.Size() && prev.MTime == info.ModTime().UnixNano() {
		e := *prev
		e.Mode = uint32(info.Mode().Perm())
		b.stats.Unchanged++
		b.stats.Chunks += len(e.Chunks)
		return b.add(&e)
	}
	f, err := os.Open(osPath)
	if err != nil {
		b.log("warning: cannot read %s: %v", osPath, err)
		b.stats.Files--
		b.stats.Bytes -= info.Size()
		return nil
	}
	defer f.Close()
	e := &snapshot.Entry{Path: mp, Type: snapshot.TypeFile, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()}
	if err := b.chunkInto(ctx, f, e); err != nil {
		return fmt.Errorf("%s: %w", osPath, err)
	}
	if e.Size != info.Size() {
		b.log("note: %s changed size while being read (%d -> %d bytes)", osPath, info.Size(), e.Size)
		b.stats.Bytes += e.Size - info.Size()
	}
	return b.add(e)
}

// AddStream stores a generated artifact (e.g. a database dump) read from r.
func (b *Builder) AddStream(ctx context.Context, name string, r io.Reader) (*snapshot.Entry, error) {
	mp := snapshot.CleanPath(name)
	if err := b.addParents(mp); err != nil {
		return nil, err
	}
	e := &snapshot.Entry{Path: mp, Type: snapshot.TypeStream, Mode: 0o600, MTime: time.Now().UnixNano()}
	if err := b.chunkInto(ctx, r, e); err != nil {
		return nil, err
	}
	b.stats.Streams++
	b.stats.Bytes += e.Size
	return e, b.add(e)
}

func (b *Builder) chunkInto(ctx context.Context, r io.Reader, e *snapshot.Entry) error {
	h := blake3.New(32, nil)
	ch := chunker.New(io.TeeReader(r, h), b.r.Gear(), b.r.ChunkerParams())
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		data := append([]byte(nil), c...)
		id := b.r.ContentID(data)
		e.Chunks = append(e.Chunks, id.String())
		e.Size += int64(len(data))
		b.stats.Chunks++
		b.sem <- struct{}{}
		b.wg.Add(1)
		go func() {
			defer func() { <-b.sem; b.wg.Done() }()
			_, n, err := b.r.PutBlob(ctx, data)
			if err != nil {
				b.setErr(err)
				return
			}
			if n > 0 {
				b.newBytes.Add(int64(n))
				b.newChunk.Add(1)
			}
		}()
		if err := b.failed(); err != nil {
			return err
		}
	}
	e.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return nil
}

// Commit waits for uploads, writes the manifest and the snapshot record.
func (b *Builder) Commit(ctx context.Context, snap *snapshot.Snapshot) (snapshot.WithID, error) {
	b.wg.Wait()
	if err := b.failed(); err != nil {
		return snapshot.WithID{}, err
	}
	if len(b.entries) == 0 {
		return snapshot.WithID{}, errors.New("nothing to back up: no readable files or streams")
	}
	snapshot.SortEntries(b.entries)
	manifest, err := snapshot.WriteManifest(ctx, b.r, b.entries)
	if err != nil {
		return snapshot.WithID{}, err
	}
	b.stats.NewChunks = int(b.newChunk.Load())
	b.stats.Uploaded = b.newBytes.Load()
	b.stats.DurationMs = time.Since(b.started).Milliseconds()
	snap.Manifest = manifest
	snap.Entries = len(b.entries)
	snap.Root = snapshot.Root(b.entries).String()
	snap.Stats = b.stats
	snap.Parent = b.parentID
	snap.Engine = Version
	if snap.Time.IsZero() {
		snap.Time = time.Now().UTC()
	}
	if snap.Host == "" {
		snap.Host, _ = os.Hostname()
	}
	id, err := snapshot.Save(ctx, b.r, snap)
	if err != nil {
		return snapshot.WithID{}, err
	}
	return snapshot.WithID{ID: id, Snapshot: snap}, nil
}

// Entries exposes the collected entries (sorted after Commit).
func (b *Builder) Entries() []*snapshot.Entry { return b.entries }

// AddReader stores a file whose content comes from r (used by importers),
// under manifest path name with the given mtime.
func (b *Builder) AddReader(ctx context.Context, name string, mode uint32, mtime time.Time, r io.Reader) (*snapshot.Entry, error) {
	mp := snapshot.CleanPath(name)
	if mp == "" {
		return nil, fmt.Errorf("empty path")
	}
	if err := b.addParents(mp); err != nil {
		return nil, err
	}
	if mode == 0 {
		mode = 0o644
	}
	e := &snapshot.Entry{Path: mp, Type: snapshot.TypeFile, Mode: mode, MTime: mtime.UnixNano()}
	if err := b.chunkInto(ctx, r, e); err != nil {
		return nil, err
	}
	b.stats.Files++
	b.stats.Bytes += e.Size
	return e, b.add(e)
}

// AddTree adds the contents of osRoot with manifest paths relative to it,
// placed under prefix (e.g. a restic restore staged in a temp dir).
func (b *Builder) AddTree(ctx context.Context, osRoot, prefix string) error {
	osRoot, err := filepath.Abs(osRoot)
	if err != nil {
		return err
	}
	return filepath.WalkDir(osRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == osRoot {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(osRoot, p)
		if err != nil {
			return err
		}
		mp := snapshot.CleanPath(prefix + "/" + filepath.ToSlash(rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return b.addParents(mp + "/x")
		case info.Mode().IsRegular():
			if err := b.addParents(mp); err != nil {
				return err
			}
			return b.addFile(ctx, p, mp, info)
		}
		return nil
	})
}
