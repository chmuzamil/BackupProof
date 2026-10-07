package engine

import (
	"context"
	"errors"
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
	pl := &placer{base: root, abs: target, open: map[string]*os.Root{}}
	defer pl.close()
	// at returns the folder an entry goes in and its name there. In place,
	// that folder is opened level by level (see placer).
	at := func(name string) (*os.Root, string, error) {
		if !opts.Original {
			return root, name, nil
		}
		d, err := pl.dir(filepath.Dir(name))
		return d, filepath.Base(name), err
	}

	type dirTime struct {
		root  *os.Root
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
			if !opts.Original {
				if err := root.MkdirAll(name, 0o700); err != nil {
					return res, fmt.Errorf("%s: %w", e.Path, err)
				}
				dirs = append(dirs, dirTime{root, name, e.MTime})
				res.Dirs++
				continue
			}
			parent, base, err := at(name)
			if err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
			existed := pathExists(parent, base)
			d, err := pl.dir(name)
			if err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
			if !restoreOwner(d, ".", e) && !existed {
				adoptParentOwner(parent, base)
			}
			if e.Mode != 0 && runtime.GOOS != "windows" {
				d.Chmod(".", fs.FileMode(e.Mode).Perm())
			}
			dirs = append(dirs, dirTime{d, ".", e.MTime})
			res.Dirs++
		case snapshot.TypeSymlink:
			links = append(links, e)
			linkSet[e.Path] = true
		case snapshot.TypeFile, snapshot.TypeStream:
			parent, base, err := at(name)
			if err == nil {
				err = restoreFile(ctx, r, parent, e, base, opts.Original)
			}
			if err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
			res.Files++
			res.Bytes += e.Size
		}
	}
	for _, e := range links {
		name := filepath.FromSlash(e.Path)
		if dir := filepath.Dir(name); dir != "." && !opts.Original {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return res, fmt.Errorf("%s: %w", e.Path, err)
			}
		}
		parent, base, err := at(name)
		if err != nil {
			return res, fmt.Errorf("%s: %w", e.Path, err)
		}
		parent.Remove(base)
		if err := parent.Symlink(filepath.FromSlash(e.Link), base); err != nil {
			opts.Log("warning: cannot create symlink %s: %v", e.Path, err)
		} else if opts.Original {
			restoreOwner(parent, base, e)
		}
	}
	// Directory mtimes last, deepest first, so file writes don't bump them.
	for i := len(dirs) - 1; i >= 0; i-- {
		if dirs[i].mtime > 0 {
			t := time.Unix(0, dirs[i].mtime)
			dirs[i].root.Chtimes(dirs[i].path, t, t)
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
		if !restoreOwner(root, name, e) {
			if hadOwner {
				_ = root.Lchown(name, uid, gid)
			} else {
				adoptParentOwner(root, name)
			}
		}
		// Chmod and Chtimes follow links: touch only the file just placed.
		if fi, err := root.Lstat(name); err != nil || !fi.Mode().IsRegular() {
			return nil
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

// placer opens folders for an in-place restore one level at a time, each
// inside the folder above it. Restoring in place means writing as root into
// folders other people may own, so a folder that is a symlink on disk is
// refused (unless root made it in a folder only root can change, such as
// /var/run), and because every level is opened inside its parent, a folder
// swapped for a symlink after the check still can't lead outside that parent.
// Open folders are kept, so later entries use the folder that was checked.
type placer struct {
	base *os.Root
	abs  string
	open map[string]*os.Root
}

func (p *placer) close() {
	for _, r := range p.open {
		r.Close()
	}
}

// dir opens (creating if needed) the folder name, relative to the target.
func (p *placer) dir(name string) (*os.Root, error) {
	if name == "." || name == "" {
		return p.base, nil
	}
	if r, ok := p.open[name]; ok {
		return r, nil
	}
	parent, err := p.dir(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	base := filepath.Base(name)
	fi, err := parent.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		if err := parent.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		fi, err = parent.Lstat(base)
	}
	if err != nil {
		return nil, err
	}
	var r *os.Root
	switch {
	case fi.Mode().Type() == fs.ModeDir:
		r, err = parent.OpenRoot(base)
	case fi.Mode()&fs.ModeSymlink != 0 && trustedLink(parent, fi):
		var real string
		if real, err = filepath.EvalSymlinks(filepath.Join(p.abs, name)); err == nil {
			r, err = os.OpenRoot(real)
		}
	default:
		return nil, fmt.Errorf("%s is a link or not a folder on this server, so nothing is restored through it; restore to a folder instead", filepath.Join(p.abs, name))
	}
	if err != nil {
		return nil, err
	}
	p.open[name] = r
	return r, nil
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
