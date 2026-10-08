package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// A second copy of a backup in another storage (the 3-2-1 rule). Each
// storage has its own keys, so every chunk is decrypted from the source and
// encrypted for the destination, where it gets a new name. Files and their
// content hashes don't change, so the copy has exactly the same content root
// as the original, which is what its proof records.
//
// To avoid reading chunks again on every copy, the destination keeps "copy
// maps" from source chunk names to its own, sealed with its keys like any
// other object, so nobody without them can forge one.

type CopyStats struct {
	Chunks     int   `json:"chunks"`
	NewChunks  int   `json:"newChunks"`
	Uploaded   int64 `json:"uploaded"`
	DurationMs int64 `json:"durationMs"`
}

type copyMap struct {
	FromRepo string            `json:"fromRepo"`
	Pairs    map[string]string `json:"pairs"` // source chunk ID → destination chunk ID
}

// CopySnapshot copies snapshot id from src into dst and returns the new snapshot.
func CopySnapshot(ctx context.Context, src, dst *repo.Repo, id bpcrypto.ID, expectedRoot string, log Logger) (snapshot.WithID, CopyStats, error) {
	if log == nil {
		log = nopLog
	}
	start := time.Now()
	var st CopyStats
	s, err := snapshot.Load(ctx, src, id)
	if err != nil {
		return snapshot.WithID{}, st, fmt.Errorf("reading the backup: %w", err)
	}
	if expectedRoot != "" && s.Root != expectedRoot {
		return snapshot.WithID{}, st, errors.New("this backup doesn't match its signed proof; refusing to copy it")
	}
	entries, err := snapshot.ReadManifest(ctx, src, s)
	if err != nil {
		return snapshot.WithID{}, st, err
	}
	if err := dst.LoadIndex(ctx); err != nil {
		return snapshot.WithID{}, st, err
	}
	known := map[string]string{}
	if ids, err := dst.ListIDs(ctx, "copymaps"); err == nil {
		for _, mid := range ids {
			var m copyMap
			if dst.GetJSON(ctx, "copymaps", mid, &m) == nil && m.FromRepo == src.Config().ID {
				for k, v := range m.Pairs {
					known[k] = v
				}
			}
		}
	}
	// Download ahead the chunks the destination doesn't have yet, in loop
	// order, and upload them in parallel.
	var need []Want
	listed := map[string]bool{}
	for _, e := range entries {
		hint := int64(0)
		if len(e.Chunks) > 0 {
			hint = e.Size / int64(len(e.Chunks))
		}
		for _, cs := range e.Chunks {
			if listed[cs] {
				continue
			}
			listed[cs] = true
			if d, ok := known[cs]; ok {
				if did, err := bpcrypto.ParseID(d); err == nil && dst.HasBlob(did) {
					continue
				}
			}
			if sid, err := bpcrypto.ParseID(cs); err == nil {
				need = append(need, Want{ID: sid, Hint: hint})
			}
		}
	}
	pf := NewFetcher(ctx, src, need, FetchOptions{})
	defer pf.Close()
	up := NewUploader(ctx)
	defer up.Close()
	var newChunks, uploaded atomic.Int64
	added := map[string]string{}
	out := make([]*snapshot.Entry, len(entries))
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return snapshot.WithID{}, st, err
		}
		c := *e
		if len(e.Chunks) > 0 {
			c.Chunks = make([]string, len(e.Chunks))
			for j, cs := range e.Chunks {
				st.Chunks++
				if d, ok := known[cs]; ok {
					if did, err := bpcrypto.ParseID(d); err == nil && dst.HasBlob(did) {
						c.Chunks[j] = d
						continue
					}
				}
				sid, err := bpcrypto.ParseID(cs)
				if err != nil {
					return snapshot.WithID{}, st, err
				}
				data, err := pf.Get(ctx, sid)
				if err != nil {
					return snapshot.WithID{}, st, fmt.Errorf("%s: %w", e.Path, err)
				}
				did := dst.ContentID(data)
				path := e.Path
				if err := up.Go(int64(len(data)), func(ctx context.Context) (int64, error) {
					_, n, err := dst.PutBlob(ctx, data)
					if err != nil {
						return 0, fmt.Errorf("%s: %w", path, err)
					}
					if n > 0 {
						newChunks.Add(1)
						uploaded.Add(int64(n))
					}
					return int64(n), nil
				}); err != nil {
					return snapshot.WithID{}, st, err
				}
				c.Chunks[j] = did.String()
				known[cs], added[cs] = did.String(), did.String()
			}
		}
		out[i] = &c
	}
	// Every chunk must be stored before the copy's snapshot is written.
	if err := up.Wait(); err != nil {
		return snapshot.WithID{}, st, err
	}
	st.NewChunks += int(newChunks.Load())
	st.Uploaded += uploaded.Load()
	if root := snapshot.Root(out).String(); root != s.Root {
		return snapshot.WithID{}, st, errors.New("the copy's content root doesn't match the original; nothing was recorded")
	}
	manifest, err := snapshot.WriteManifest(ctx, dst, out)
	if err != nil {
		return snapshot.WithID{}, st, err
	}
	if len(added) > 0 {
		if _, err := dst.PutJSON(ctx, "copymaps", copyMap{FromRepo: src.Config().ID, Pairs: added}); err != nil {
			log("warning: could not save the copy map (the next copy re-reads more): %v", err)
		}
	}
	cp := *s
	cp.Manifest, cp.Parent = manifest, ""
	st.DurationMs = time.Since(start).Milliseconds()
	cp.Stats.NewChunks, cp.Stats.Uploaded, cp.Stats.DurationMs = st.NewChunks, st.Uploaded, st.DurationMs
	nid, err := snapshot.Save(ctx, dst, &cp)
	if err != nil {
		return snapshot.WithID{}, st, err
	}
	log("copied backup %s as %s: %d chunks, %d new (%d bytes uploaded) in %dms", id.Short(), nid.Short(), st.Chunks, st.NewChunks, st.Uploaded, st.DurationMs)
	return snapshot.WithID{ID: nid, Snapshot: &cp}, st, nil
}
