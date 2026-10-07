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
	// StripPrefix restores only entries below this manifest folder and drops
	// it from their paths (for example "C" to put "C/Users/…" back on drive C:).
	StripPrefix string
	// Original is for putting files back where they were backed up from: a
	// file keeps the owner of the file it replaces (new files and folders take
	// their folder's owner) and folders get their backed-up permissions.
	Original bool
	Log      Logger
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
//
// All filesystem operations go through an os.Root opened on target, so the
// operating system refuses any path that would leave the target directory,
// including through symlinks (whether they came from the snapshot or already
// existed in target). Symlinks from the snapshot are created last, after
// every file and directory, so no entry is ever written through one. A
// crafted snapshot therefore cannot write outside target.
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
	root, err := os.OpenRoot(target)
	if err != nil {
		return res, err
	}
	defer root.Close()

	type dirTime struct {
		path  string
		mtime int64
	}
	var dirs []dirTime
	var links []*snapshot.Entry
	linkSet := map[string]bool{}
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
		if opts.StripPrefix != "" {
			rel, ok := strings.CutPrefix(e.Path, opts.StripPrefix+"/")
			if !ok {
				continue
			}
			c := *e
			c.Path = rel
			e = &c
		}
		// Nothing may be placed beneath an entry that is a symlink.
		for dir := pathDir(e.Path); dir != ""; dir = pathDir(dir) {
			if linkSet[dir] {
				return res, fmt.Errorf("refusing %q: its parent %q is a symlink in this snapshot", e.Path, dir)
			}
		}
		name := filepath.FromSlash(e.Path)
		switch e.Type {
		case snapshot.TypeDir:
			existed := pathExists(root, name)
			if err := root.MkdirAll(name, 0o700); err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
			if opts.Original && !existed {
				adoptParentOwner(root, name)
			}
			if opts.Original && e.Mode != 0 && runtime.GOOS != "windows" {
				root.Chmod(name, fs.FileMode(e.Mode).Perm())
			}
			dirs = append(dirs, dirTime{name, e.MTime})
			res.Dirs++
		case snapshot.TypeSymlink:
			links = append(links, e)
			linkSet[e.Path] = true
		case snapshot.TypeFile, snapshot.TypeStream:
			if err := restoreFile(ctx, r, root, e, name, opts.Original); err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
			res.Files++
			res.Bytes += e.Size
		}
	}
	for _, e := range links {
		name := filepath.FromSlash(e.Path)
		if dir := filepath.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
		}
		root.Remove(name)
		if err := root.Symlink(filepath.FromSlash(e.Link), name); err != nil {
			opts.Log("warning: cannot create symlink %s: %v", e.Path, err)
		}
	}
	// Directory mtimes last, deepest first, so file writes don't bump them.
	for i := len(dirs) - 1; i >= 0; i-- {
		if dirs[i].mtime > 0 {
			t := time.Unix(0, dirs[i].mtime)
			root.Chtimes(dirs[i].path, t, t)
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	opts.Log("restored %d files (%d bytes) to %s", res.Files, res.Bytes, target)
	return res, nil
}

func restoreFile(ctx context.Context, r *repo.Repo, root *os.Root, e *snapshot.Entry, name string, original bool) error {
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// In place, the restored file keeps the owner of the one it replaces.
	uid, gid, hadOwner := -1, -1, false
	if original {
		if fi, err := root.Lstat(name); err == nil && fi.Mode().IsRegular() {
			uid, gid, hadOwner = ownerOf(fi)
		}
	}
	tmp := name + ".bp-partial"
	root.Remove(tmp)
	// O_EXCL: never open something that already exists (such as a symlink).
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := blake3.New(32, nil)
	var size int64
	for _, cs := range e.Chunks {
		id, err := bpcrypto.ParseID(cs)
		if err != nil {
			f.Close()
			root.Remove(tmp)
			return err
		}
		data, err := r.GetBlob(ctx, id)
		if err != nil {
			f.Close()
			root.Remove(tmp)
			return err
		}
		_, _ = h.Write(data) // hash writes never fail
		size += int64(len(data))
		if _, err := f.Write(data); err != nil {
			f.Close()
			root.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != e.Hash || size != e.Size {
		root.Remove(tmp)
		return fmt.Errorf("content verification failed (hash or size mismatch)")
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	if original {
		if hadOwner {
			_ = root.Lchown(name, uid, gid)
		} else {
			adoptParentOwner(root, name)
		}
	}
	if runtime.GOOS != "windows" && e.Mode != 0 {
		root.Chmod(name, fs.FileMode(e.Mode))
	}
	if e.MTime > 0 {
		t := time.Unix(0, e.MTime)
		root.Chtimes(name, t, t)
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
