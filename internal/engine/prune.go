package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

type PruneOptions struct {
	// Grace protects recently written blobs that may belong to a backup that
	// is still running. Without repository locks this is what makes prune
	// safe to run concurrently with backups.
	Grace  time.Duration
	DryRun bool
	Log    Logger
}

type PruneResult struct {
	Deleted      int   `json:"deleted"`
	DeletedBytes int64 `json:"deletedBytes"`
	KeptRecent   int   `json:"keptRecent"`
	Locked       int   `json:"locked"`
}

// Forget removes snapshot records. Data is reclaimed by Prune.
func Forget(ctx context.Context, r *repo.Repo, ids []string) error {
	for _, id := range ids {
		if err := r.Backend().Delete(ctx, "snapshots/"+id); err != nil {
			return fmt.Errorf("forget %s: %w", id[:8], err)
		}
	}
	return nil
}

// Prune deletes blobs no longer referenced by any snapshot.
func Prune(ctx context.Context, r *repo.Repo, opts PruneOptions) (PruneResult, error) {
	if opts.Log == nil {
		opts.Log = nopLog
	}
	if opts.Grace == 0 {
		opts.Grace = 6 * time.Hour
	}
	var res PruneResult
	snaps, err := snapshot.List(ctx, r)
	if err != nil {
		return res, err
	}
	refs, errs := References(ctx, r, snaps)
	if len(errs) > 0 {
		// Never delete data when we cannot see every reference.
		return res, fmt.Errorf("refusing to prune: %d snapshot(s) unreadable: %s", len(errs), errs[0])
	}
	stored, err := storedBlobs(ctx, r)
	if err != nil {
		return res, err
	}
	cutoff := time.Now().Add(-opts.Grace)
	for id, info := range stored {
		if _, ok := refs[id]; ok {
			continue
		}
		if info.Modified.After(cutoff) {
			res.KeptRecent++
			continue
		}
		if opts.DryRun {
			res.Deleted++
			res.DeletedBytes += info.Size
			continue
		}
		if err := r.Backend().Delete(ctx, repo.DataKey(id)); err != nil {
			// Object-locked blobs cannot be deleted until retention expires.
			res.Locked++
			continue
		}
		res.Deleted++
		res.DeletedBytes += info.Size
	}
	opts.Log("prune: deleted %d blobs (%d bytes), %d too recent, %d locked", res.Deleted, res.DeletedBytes, res.KeptRecent, res.Locked)
	return res, nil
}
