package engine

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"lukechampine.com/blake3"
)

type RestoreOptions struct {
	Include []string // manifest path prefixes; empty restores everything
	Log     Logger
}

type RestoreResult struct {
	Files      int
	Dirs       int
	Bytes      int64
	DurationMs int64
}

func matchInclude(p string, include []string) bool {
	if len(include) == 0 {
		return true
	}
	for _, inc := range include {
		inc = strings.Trim(snapshot.CleanPath(inc), "/")
		if p == inc || strings.HasPrefix(p, inc+"/") || strings.HasPrefix(inc, p+"/") {
			return true
		}
	}
	return false
}

// Restore writes a snapshot below target. Every file's content hash is
// verified while it is written; any mismatch aborts the restore.
func Restore(ctx context.Context, r *repo.Repo, s *snapshot.Snapshot, target string, opts RestoreOptions) (RestoreResult, error) {
	if opts.Log == nil {
		opts.Log = nopLog
	}
	start := time.Now()
	var res RestoreResult
	entries, err := snapshot.ReadManifest(ctx, r, s)
	if err != nil {
		return res, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return res, err
	}
	type dirTime struct {
		path  string
		mtime int64
	}
	var dirs []dirTime
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !matchInclude(e.Path, opts.Include) {
			continue
		}
		if !snapshot.SafeRelPath(e.Path) {
			return res, fmt.Errorf("refusing unsafe path %q in manifest", e.Path)
		}
		dst := filepath.Join(target, filepath.FromSlash(e.Path))
		switch e.Type {
		case snapshot.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return res, err
			}
			dirs = append(dirs, dirTime{dst, e.MTime})
			res.Dirs++
		case snapshot.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return res, err
			}
			os.Remove(dst)
			if err := os.Symlink(filepath.FromSlash(e.Link), dst); err != nil {
				opts.Log("warning: cannot create symlink %s: %v", e.Path, err)
			}
		case snapshot.TypeFile, snapshot.TypeStream:
			if err := restoreFile(ctx, r, e, dst); err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
			res.Files++
			res.Bytes += e.Size
		}
	}
	// Directory mtimes last, deepest first, so file writes don't bump them.
	for i := len(dirs) - 1; i >= 0; i-- {
		if dirs[i].mtime > 0 {
			t := time.Unix(0, dirs[i].mtime)
			os.Chtimes(dirs[i].path, t, t)
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	opts.Log("restored %d files (%d bytes) to %s", res.Files, res.Bytes, target)
	return res, nil
}

func restoreFile(ctx context.Context, r *repo.Repo, e *snapshot.Entry, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".bp-partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := blake3.New(32, nil)
	var size int64
	for _, cs := range e.Chunks {
		id, err := bpcrypto.ParseID(cs)
		if err != nil {
			f.Close()
			return err
		}
		data, err := r.GetBlob(ctx, id)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		h.Write(data)
		size += int64(len(data))
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != e.Hash || size != e.Size {
		os.Remove(tmp)
		return fmt.Errorf("content verification failed (hash or size mismatch)")
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	if runtime.GOOS != "windows" && e.Mode != 0 {
		os.Chmod(dst, fs.FileMode(e.Mode))
	}
	if e.MTime > 0 {
		t := time.Unix(0, e.MTime)
		os.Chtimes(dst, t, t)
	}
	return nil
}

// TreeRoot recomputes the Merkle content root from a restored directory tree.
// Only entries matching the snapshot's paths are considered, so the result is
// directly comparable with Snapshot.Root (and with signed attestations).
func TreeRoot(dir string) (string, int, error) {
	var entries []*snapshot.Entry
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", 0, err
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		mp := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entries = append(entries, &snapshot.Entry{Path: mp, Type: snapshot.TypeSymlink, Link: filepath.ToSlash(t)})
		case info.IsDir():
			entries = append(entries, &snapshot.Entry{Path: mp, Type: snapshot.TypeDir})
		case info.Mode().IsRegular():
			hash, err := hashFile(p)
			if err != nil {
				return err
			}
			entries = append(entries, &snapshot.Entry{Path: mp, Type: snapshot.TypeFile, Size: info.Size(), Hash: hash})
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	snapshot.SortEntries(entries)
	return snapshot.Root(entries).String(), len(entries), nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New(32, nil)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
