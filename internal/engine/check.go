package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

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
	Snapshots       int      `json:"snapshots"`
	ReferencedBlobs int      `json:"referencedBlobs"`
	StoredBlobs     int      `json:"storedBlobs"`
	StoredBytes     int64    `json:"storedBytes"` // size of all stored data
	MissingBlobs    int      `json:"missingBlobs"`
	UnusedBlobs     int      `json:"unusedBlobs"`
	ReadBlobs       int      `json:"readBlobs"`
	ReadBytes       int64    `json:"readBytes"`
	CorruptBlobs    int      `json:"corruptBlobs"`
	Errors          []string `json:"errors,omitempty"`
}

func (c CheckResult) OK() bool {
	return c.MissingBlobs == 0 && c.CorruptBlobs == 0 && len(c.Errors) == 0
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
	refs, errs := References(ctx, r, snaps)
	res.Errors = append(res.Errors, errs...)
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
		for _, id := range sample {
			data, err := pf.Get(ctx, id)
			res.ReadBlobs++
			if err != nil {
				res.CorruptBlobs++
				res.Errors = append(res.Errors, err.Error())
				continue
			}
			res.ReadBytes += int64(len(data))
		}
		opts.Log("read-data: verified %d blobs (%d bytes), %d corrupt", res.ReadBlobs, res.ReadBytes, res.CorruptBlobs)
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
