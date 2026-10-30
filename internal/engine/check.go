package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/transfer"
)

type CheckOptions struct {
	// ReadDataPercent downloads and verifies this share of referenced blobs
	// (0 = structure only, 100 = everything).
	ReadDataPercent float64
	// Seed makes the sample unpredictable but reproducible; BackupProof uses
	// the previous ledger head so operators cannot cherry-pick what is read.
	Seed []byte
	Log  Logger
}

type CheckResult struct {
	Snapshots       int   `json:"snapshots"`
	ReferencedBlobs int   `json:"referencedBlobs"`
	StoredBlobs     int   `json:"storedBlobs"`
	StoredBytes     int64 `json:"storedBytes"` // size of all stored data
	MissingBlobs    int   `json:"missingBlobs"`
	UnusedBlobs     int   `json:"unusedBlobs"`
	ReadBlobs       int   `json:"readBlobs"`
	ReadBytes       int64 `json:"readBytes"`
	CorruptBlobs    int   `json:"corruptBlobs"` // read, but failed the integrity check
	// UnreadableBlobs couldn't be read at all (storage errors, timeouts),
	// even when tried again at the end: their data is unknown, not damaged.
	UnreadableBlobs int      `json:"unreadableBlobs,omitempty"`
	Errors          []string `json:"errors,omitempty"`
}

// UnreadablePause is how long a check waits before reading again the
// chunks storage failed to hand over.
var UnreadablePause = 30 * time.Second

// OnlyUnreadable reports a failed check whose only problem is chunks that
// couldn't be read: nothing was found missing or damaged.
func (c CheckResult) OnlyUnreadable() bool {
	return c.UnreadableBlobs > 0 && c.MissingBlobs == 0 && c.CorruptBlobs == 0
}

func (c CheckResult) OK() bool {
	return c.MissingBlobs == 0 && c.CorruptBlobs == 0 && len(c.Errors) == 0
}

// checkReferences is References for a check: a snapshot whose file list
// storage won't hand over (errors, timeouts) is tried again after a pause,
// and if it still can't be read it counts as unreadable, not as damage.
func checkReferences(ctx context.Context, r *repo.Repo, snaps []snapshot.WithID, res *CheckResult, log Logger) (map[bpcrypto.ID]struct{}, error) {
	refs := map[bpcrypto.ID]struct{}{}
	add := func(s snapshot.WithID) error {
		for _, m := range s.Manifest {
			if id, err := bpcrypto.ParseID(m); err == nil {
				refs[id] = struct{}{}
			}
		}
		entries, err := snapshot.ReadManifest(ctx, r, s.Snapshot)
		if err != nil {
			return err
		}
		for _, e := range entries {
			for _, c := range e.Chunks {
				if id, err := bpcrypto.ParseID(c); err == nil {
					refs[id] = struct{}{}
				}
			}
		}
		return nil
	}
	var again []snapshot.WithID
	for _, s := range snaps {
		if err := add(s); err != nil {
			if retry, _, _ := transfer.Classify(err); !retry {
				countManifestError(res, s, err)
				continue
			}
			again = append(again, s)
		}
	}
	if len(again) == 0 {
		return refs, nil
	}
	log("structure: storage couldn't hand over the file lists of %d snapshots; trying them again in %s", len(again), UnreadablePause)
	t := time.NewTimer(UnreadablePause)
	select {
	case <-ctx.Done():
		t.Stop()
		return refs, ctx.Err()
	case <-t.C:
	}
	for _, s := range again {
		if err := add(s); err != nil {
			if retry, _, _ := transfer.Classify(err); retry {
				res.UnreadableBlobs++
				res.Errors = append(res.Errors, fmt.Sprintf("snapshot %s: file list %v: %v", s.ID.Short(), ErrUnreadable, err))
				continue
			}
			countManifestError(res, s, err)
		}
	}
	return refs, nil
}

// countManifestError records a snapshot file list that is missing or
// damaged (not just unreadable).
func countManifestError(res *CheckResult, s snapshot.WithID, err error) {
	if errors.Is(err, backend.ErrNotFound) {
		res.MissingBlobs++
	} else {
		res.CorruptBlobs++
	}
	res.Errors = append(res.Errors, fmt.Sprintf("snapshot %s: %v", s.ID.Short(), err))
}

// References returns every blob referenced by the given snapshots.
func References(ctx context.Context, r *repo.Repo, snaps []snapshot.WithID) (map[bpcrypto.ID]struct{}, []string) {
	refs := map[bpcrypto.ID]struct{}{}
	var errs []string
	for _, s := range snaps {
		for _, m := range s.Manifest {
			if id, err := bpcrypto.ParseID(m); err == nil {
				refs[id] = struct{}{}
			}
		}
		entries, err := snapshot.ReadManifest(ctx, r, s.Snapshot)
		if err != nil {
			errs = append(errs, fmt.Sprintf("snapshot %s: %v", s.ID.Short(), err))
			continue
		}
		for _, e := range entries {
			for _, c := range e.Chunks {
				if id, err := bpcrypto.ParseID(c); err == nil {
					refs[id] = struct{}{}
				}
			}
		}
	}
	return refs, errs
}

func storedBlobs(ctx context.Context, r *repo.Repo) (map[bpcrypto.ID]backend.ObjectInfo, error) {
	stored := map[bpcrypto.ID]backend.ObjectInfo{}
	err := r.Backend().List(ctx, "data/", func(o backend.ObjectInfo) error {
		name := o.Key[strings.LastIndex(o.Key, "/")+1:]
		if id, err := bpcrypto.ParseID(name); err == nil {
			stored[id] = o
		}
		return nil
	})
	return stored, err
}

// Check verifies repository structure and optionally reads back data.
func Check(ctx context.Context, r *repo.Repo, opts CheckOptions) (CheckResult, error) {
	if opts.Log == nil {
		opts.Log = nopLog
	}
	var res CheckResult
	snaps, err := snapshot.List(ctx, r)
	if err != nil {
		return res, err
	}
	res.Snapshots = len(snaps)
	refs, err := checkReferences(ctx, r, snaps, &res, opts.Log)
	if err != nil {
		return res, err
	}
	res.ReferencedBlobs = len(refs)
	stored, err := storedBlobs(ctx, r)
	if err != nil {
		return res, err
	}
	res.StoredBlobs = len(stored)
	for _, o := range stored {
		res.StoredBytes += o.Size
	}
	for id := range refs {
		if _, ok := stored[id]; !ok {
			res.MissingBlobs++
			res.Errors = append(res.Errors, "missing blob "+id.String())
		}
	}
	for id := range stored {
		if _, ok := refs[id]; !ok {
			res.UnusedBlobs++
		}
	}
	opts.Log("structure: %d snapshots, %d referenced blobs, %d missing", res.Snapshots, res.ReferencedBlobs, res.MissingBlobs)

	if opts.ReadDataPercent > 0 {
		var sample []bpcrypto.ID
		var wants []Want
		for _, id := range SampleIDs(refs, opts.ReadDataPercent, opts.Seed) {
			if o, ok := stored[id]; ok {
				sample = append(sample, id)
				wants = append(wants, Want{ID: id, Hint: max(o.Size, defaultHint)})
			}
		}
		// Checks use less concurrency, so a weekly check doesn't saturate a
		// small server, and keep going past bad chunks to count them all.
		pf := NewFetcher(ctx, r, wants, FetchOptions{MaxConcurrency: transfer.CheckConcurrency, KeepGoing: true})
		defer pf.Close()
		var unread []bpcrypto.ID
		for _, id := range sample {
			data, err := pf.Get(ctx, id)
			res.ReadBlobs++
			switch {
			case err == nil:
				res.ReadBytes += int64(len(data))
			case errors.Is(err, ErrUnreadable):
				unread = append(unread, id)
			case errors.Is(err, ErrChunkMissing):
				res.MissingBlobs++
				res.Errors = append(res.Errors, err.Error())
			case ctx.Err() != nil:
				return res, ctx.Err()
			default:
				res.CorruptBlobs++
				res.Errors = append(res.Errors, err.Error())
			}
		}
		pf.Close()
		// Storage errors say nothing about the data: give those chunks
		// another chance once the service has had time to recover.
		if len(unread) > 0 {
			opts.Log("read-data: %d blobs couldn't be read (storage errors, not damage); trying them again in %s", len(unread), UnreadablePause)
			t := time.NewTimer(UnreadablePause)
			select {
			case <-ctx.Done():
				t.Stop()
				return res, ctx.Err()
			case <-t.C:
			}
			for _, id := range unread {
				data, err := ReadChunk(ctx, r, id)
				switch {
				case err == nil:
					res.ReadBytes += int64(len(data))
				case errors.Is(err, ErrUnreadable):
					res.UnreadableBlobs++
					res.Errors = append(res.Errors, err.Error())
				case errors.Is(err, ErrChunkMissing):
					res.MissingBlobs++
					res.Errors = append(res.Errors, err.Error())
				default:
					res.CorruptBlobs++
					res.Errors = append(res.Errors, err.Error())
				}
			}
		}
		opts.Log("read-data: verified %d blobs (%d bytes), %d damaged, %d couldn't be read", res.ReadBlobs, res.ReadBytes, res.CorruptBlobs, res.UnreadableBlobs)
	}
	return res, nil
}

// SampleIDs deterministically selects percent% of ids ranked by
// BLAKE3(seed || id). With a seed fixed in advance by the ledger, the sample
// cannot be chosen to avoid known-bad data.
func SampleIDs(ids map[bpcrypto.ID]struct{}, percent float64, seed []byte) []bpcrypto.ID {
	type ranked struct {
		id   bpcrypto.ID
		rank uint64
	}
	all := make([]ranked, 0, len(ids))
	for id := range ids {
		h := bpcrypto.Hash(seed, id[:])
		all = append(all, ranked{id, binary.BigEndian.Uint64(h[:8])})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rank < all[j].rank })
	n := int(float64(len(all))*percent/100 + 0.999999)
	if percent >= 100 || n > len(all) {
		n = len(all)
	}
	out := make([]bpcrypto.ID, n)
	for i := range out {
		out[i] = all[i].id
	}
	return out
}
