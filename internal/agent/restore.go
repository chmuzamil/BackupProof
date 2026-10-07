package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/chunker"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/ops"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

// loadAttested opens the backup the server named and checks it is the one
// the signed proof describes.
func loadAttested(ctx context.Context, r *repo.Repo, lease *protocol.Lease) (*snapshot.Snapshot, []*snapshot.Entry, error) {
	if lease.Restore == nil || lease.SnapshotID == "" {
		return nil, nil, errors.New("the restore job is missing its settings")
	}
	id, err := bpcrypto.ParseID(lease.SnapshotID)
	if err != nil {
		return nil, nil, err
	}
	s, err := snapshot.Load(ctx, r, id)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the backup: %w", err)
	}
	if s.Root != lease.ExpectedRoot {
		return nil, nil, errors.New("this backup doesn't match its signed proof; refusing to restore it")
	}
	entries, err := snapshot.ReadManifest(ctx, r, s)
	if err != nil {
		return nil, nil, err
	}
	return s, entries, nil
}

func included(p string, paths []string) bool {
	if len(paths) == 0 {
		return true
	}
	for _, x := range paths {
		if p == x || strings.HasPrefix(p, x+"/") || strings.HasPrefix(x, p+"/") {
			return true
		}
	}
	return false
}

// originalPath is where a backed-up path came from on this machine:
// "srv/www/x" → /srv/www/x, and on Windows "C/Users/x" → C:\Users\x.
func originalPath(p string) (drive, full string, ok bool) {
	if runtime.GOOS != "windows" {
		return "", "/" + p, true
	}
	first, rest, _ := strings.Cut(p, "/")
	if len(first) != 1 {
		return "", "", false
	}
	return first, first + `:\` + filepath.FromSlash(rest), true
}

func (a *Agent) runRestore(ctx context.Context, r *repo.Repo, lease *protocol.Lease, jl *jobLog) (any, error) {
	s, entries, err := loadAttested(ctx, r, lease)
	if err != nil {
		return nil, err
	}
	p := lease.Restore
	opts := engine.RestoreOptions{Include: p.Paths, Log: jl.Logf}
	var total engine.RestoreResult
	add := func(res engine.RestoreResult) {
		total.Files += res.Files
		total.Dirs += res.Dirs
		total.Bytes += res.Bytes
	}
	start := time.Now()
	if p.Folder != "" {
		target, err := filepath.Abs(p.Folder)
		if err != nil {
			return nil, err
		}
		if engine.IsDenied(target, a.deny) {
			return nil, errors.New("that folder holds the BackupProof server's own data; choose another folder")
		}
		jl.Logf("restoring backup %s to %s", lease.SnapshotID[:8], target)
		res, err := engine.Restore(ctx, r, s, target, opts)
		if err != nil {
			return nil, err
		}
		add(res)
	} else if lease.Source.Kind == "docker" {
		// Docker volumes go back into their volumes, replacing what is there;
		// mounted folders go back where they came from.
		vols, mounts, err := dockerRestoreParts(entries, p.Paths)
		if err != nil {
			return nil, err
		}
		if len(mounts) > 0 {
			if runtime.GOOS == "windows" {
				return nil, errors.New("mounted folders can only be put back on Linux or macOS; restore them to a folder instead")
			}
			var roots []string
			seenRoot := map[string]bool{}
			for _, e := range entries {
				rel, ok := strings.CutPrefix(e.Path, "docker/mounts/")
				if !ok || !included(e.Path, mounts) {
					continue
				}
				if full := "/" + rel; engine.IsDenied(full, a.deny) {
					return nil, fmt.Errorf("%s is part of the BackupProof server's own data and can't be overwritten; restore to a folder instead", full)
				}
				if top := mountRoot(e.Path, mounts); top != "" && !seenRoot[top] {
					seenRoot[top] = true
					roots = append(roots, top)
				}
			}
			restart := source.StopContainers(ctx, source.ContainersUsing(ctx, roots), jl.Logf)
			jl.Logf("putting mounted folders back where they came from")
			res, err := engine.Restore(ctx, r, s, "/", engine.RestoreOptions{Include: mounts, StripPrefix: "docker/mounts", Original: true, Log: jl.Logf})
			add(res)
			restart()
			if err != nil {
				return nil, err
			}
		}
		for _, v := range vols {
			tmp, err := os.MkdirTemp(a.dir, "volume-")
			if err != nil {
				return nil, err
			}
			owners := map[string]source.Owner{}
			for _, e := range entries {
				if rel, ok := strings.CutPrefix(e.Path, "docker/volumes/"+v+"/"); ok && e.UID != nil && e.GID != nil {
					owners[rel] = source.Owner{UID: *e.UID, GID: *e.GID}
				}
			}
			err = source.RestoreVolume(ctx, v, tmp, owners, func(target string) error {
				o := engine.RestoreOptions{StripPrefix: "docker/volumes/" + v, Original: true, Log: jl.Logf}
				res, err := engine.Restore(ctx, r, s, target, o)
				add(res)
				return err
			}, jl.Logf)
			os.RemoveAll(tmp)
			if err != nil {
				return nil, fmt.Errorf("volume %s: %w", v, err)
			}
		}
	} else {
		// Back where it came from: check every destination first, so nothing
		// is written if any of it is off limits.
		drives := map[string]bool{}
		n := 0
		for _, e := range entries {
			if !included(e.Path, p.Paths) {
				continue
			}
			drive, full, ok := originalPath(e.Path)
			if !ok {
				return nil, fmt.Errorf("can't tell where %q came from on this server; restore to a folder instead", e.Path)
			}
			if engine.IsDenied(full, a.deny) {
				return nil, fmt.Errorf("%s is part of the BackupProof server's own data and can't be overwritten; restore to a folder instead", full)
			}
			drives[drive] = true
			n++
		}
		if n == 0 {
			return nil, errors.New("there is nothing in that selection to restore")
		}
		opts.Original = true
		if runtime.GOOS == "windows" {
			for d := range drives {
				o := opts
				o.StripPrefix = d
				jl.Logf("restoring backup %s to its original place on drive %s:", lease.SnapshotID[:8], d)
				res, err := engine.Restore(ctx, r, s, d+`:\`, o)
				if err != nil {
					return nil, err
				}
				add(res)
			}
		} else {
			jl.Logf("restoring backup %s to its original place", lease.SnapshotID[:8])
			res, err := engine.Restore(ctx, r, s, "/", opts)
			if err != nil {
				return nil, err
			}
			add(res)
		}
	}
	jl.Logf("restored %d files (%d bytes), every file checked against its content hash", total.Files, total.Bytes)
	return map[string]any{"files": total.Files, "bytes": total.Bytes, "durationMs": time.Since(start).Milliseconds()}, nil
}

// dockerRestoreParts splits a selection in a Docker backup into the volumes
// to replace (always whole: part of a volume can be restored to a folder)
// and the mounted folders to put back, as include paths.
func dockerRestoreParts(entries []*snapshot.Entry, paths []string) (vols, mounts []string, err error) {
	all := len(paths) == 0
	for _, p := range paths {
		parts := strings.Split(p, "/")
		switch {
		case p == "docker":
			all = true
		case p == "docker/mounts" || strings.HasPrefix(p, "docker/mounts/"):
			mounts = append(mounts, p)
		case p == "docker/volumes" || (len(parts) == 3 && parts[0] == "docker" && parts[1] == "volumes"):
		default:
			return nil, nil, fmt.Errorf("%s: only whole volumes and mounted folders can be put back; restore it to a folder instead", p)
		}
	}
	if all {
		mounts = []string{"docker/mounts"}
	}
	seen := map[string]bool{}
	hasMounts := false
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "docker/mounts/") && included(e.Path, mounts) {
			hasMounts = true
		}
		rest, ok := strings.CutPrefix(e.Path, "docker/volumes/")
		if !ok {
			continue
		}
		v, _, _ := strings.Cut(rest, "/")
		if v != "" && !seen[v] && included("docker/volumes/"+v, paths) {
			seen[v] = true
			vols = append(vols, v)
		}
	}
	if !hasMounts {
		mounts = nil
	}
	if len(vols) == 0 && len(mounts) == 0 {
		return nil, nil, errors.New("this backup has no Docker volumes or mounted folders in that selection")
	}
	return vols, mounts, nil
}

// mountRoot is the place on this server an entry under docker/mounts goes
// back to, cut at the selected path so containers using it can be stopped.
func mountRoot(entryPath string, sel []string) string {
	for _, s := range sel {
		if s == "docker/mounts" {
			// The whole set: stop containers using this entry's top folder.
			rel := strings.TrimPrefix(entryPath, "docker/mounts/")
			parts := strings.SplitN(rel, "/", 3)
			if len(parts) >= 2 {
				return "/" + parts[0] + "/" + parts[1]
			}
			return "/" + rel
		}
		if entryPath == s || strings.HasPrefix(entryPath, s+"/") {
			return "/" + strings.TrimPrefix(s, "docker/mounts/")
		}
	}
	return ""
}

func (a *Agent) runRestoreDB(ctx context.Context, r *repo.Repo, lease *protocol.Lease, jl *jobLog) (any, error) {
	s, _, err := loadAttested(ctx, r, lease)
	if err != nil {
		return nil, err
	}
	p := lease.Restore
	dump, err := source.DumpPath(lease.Source)
	if err != nil {
		return nil, err
	}
	if lease.Source.Kind == "sqlite" {
		path := p.DBTarget
		if p.DBReplace && len(lease.Source.Paths) > 0 {
			path = lease.Source.Paths[0]
		}
		if engine.IsDenied(path, a.deny) {
			return nil, errors.New("that file is part of the BackupProof server's own data; choose another place")
		}
	}
	work, err := os.MkdirTemp(filepath.Join(a.dir), "restore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	start := time.Now()
	jl.Logf("restoring the database dump from backup %s", lease.SnapshotID[:8])
	if _, err := engine.Restore(ctx, r, s, work, engine.RestoreOptions{Include: []string{dump}, Log: jl.Logf}); err != nil {
		return nil, err
	}
	file := filepath.Join(work, filepath.FromSlash(dump))
	if _, err := os.Stat(file); err != nil {
		return nil, fmt.Errorf("the backup has no %s", dump)
	}
	where, err := source.RestoreDatabase(ctx, lease.Source, file, p.DBTarget, p.DBReplace, jl.Logf)
	if err != nil {
		return nil, err
	}
	jl.Logf("restored into %s", where)
	return map[string]any{"into": where, "durationMs": time.Since(start).Milliseconds()}, nil
}

// runCopy copies the backup the server named into the item's second storage.
func (a *Agent) runCopy(ctx context.Context, env *ops.Env, lease *protocol.Lease, jl *jobLog) (any, error) {
	c := lease.Copy
	if c == nil || lease.SnapshotID == "" {
		return nil, errors.New("the copy job is missing its settings")
	}
	if c.Repository.Type == "local" {
		if err := ops.StorageDenied(c.Repository.Path, a.deny); err != nil {
			return nil, err
		}
	}
	be, err := backend.Open(ctx, c.Repository, c.Creds)
	if err != nil {
		return nil, fmt.Errorf("second storage: %w", err)
	}
	be = backend.Throttle(be, lease.UploadBps, lease.DownloadBps)
	dst, err := repo.Open(ctx, be, []byte(c.Password))
	if errors.Is(err, repo.ErrNotARepo) {
		jl.Logf("initializing the second storage at %s", be.Location())
		dst, err = repo.Init(ctx, be, []byte(c.Password), chunker.DefaultParams)
	}
	if err != nil {
		be.Close()
		return nil, err
	}
	defer dst.Close()
	info := proof.StorageInfo{Location: be.Location(), ObjectLockMode: c.Repository.ObjectLockMode, ObjectLockDays: c.Repository.ObjectLockDays}
	rec, err := ops.Copy(ctx, env, dst, info, lease.SnapshotID, lease.ExpectedRoot, lease.Source.Name, lease.Retention)
	if err != nil {
		return nil, err
	}
	return map[string]any{"snapshotId": rec.SnapshotID}, nil
}
