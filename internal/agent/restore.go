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

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/engine"
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
