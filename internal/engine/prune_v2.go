package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
)

const (
	// repackShare: a pack is rewritten once at least this share of its bytes
	// is no longer needed by any backup.
	repackShare = 0.4
	// consolidateAt: this many index files are folded into one even when
	// nothing is deleted, so loading the index stays quick.
	consolidateAt = 32
)

type packState struct {
	blobs       []repo.PackEntry
	used        []repo.PackEntry
	usedBytes   int64
	unusedBytes int64
	info        backend.ObjectInfo
	listed      bool
	action      string // "", "delete" or "repack"
	markedReady bool
}

// pruneV2 is Prune for packed storage. It works on whole packs: a pack no
// backup needs is deleted, one mostly unneeded is rewritten with only what is
// still needed, and packs no index lists (left by backups that didn't finish)
// are deleted. As in v1, everything is marked first and only acted on in a
// later run, and nothing newer than the grace period is touched. The index is
// rewritten before anything is deleted, so it never points at a deleted pack.
func pruneV2(ctx context.Context, r *repo.Repo, opts PruneOptions, refs map[bpcrypto.ID]struct{}) (PruneResult, error) {
	var res PruneResult
	files, err := r.ReadIndexes(ctx)
	if err != nil {
		return res, err
	}
	listed, err := r.Packs(ctx)
	if err != nil {
		return res, err
	}
	marks, err := r.PruneMarks(ctx)
	if err != nil {
		return res, err
	}
	now := time.Now()
	cutoff := now.Add(-opts.Grace)
	nextMarks := map[string]int64{}
	var zero bpcrypto.ID

	packs := map[bpcrypto.ID]*packState{}
	var packOrder []bpcrypto.ID
	loose := map[bpcrypto.ID]repo.PackEntry{}
	for _, f := range files {
		for _, p := range f.Packs {
			if p.ID == zero {
				for _, b := range p.Blobs {
					loose[b.ID] = b
				}
				continue
			}
			if _, ok := packs[p.ID]; ok {
				continue // the same pack in two index files
			}
			packs[p.ID] = &packState{blobs: p.Blobs}
			packOrder = append(packOrder, p.ID)
		}
	}
	// A blob kept once is enough: copies of it in other packs (two writers
	// stored it at the same time) count as unneeded.
	kept := map[bpcrypto.ID]bool{}
	for _, id := range packOrder {
		st := packs[id]
		st.info, st.listed = listed[id]
		for _, b := range st.blobs {
			if _, ok := refs[b.ID]; ok && !kept[b.ID] {
				kept[b.ID] = true
				st.used = append(st.used, b)
				st.usedBytes += b.Len
			} else {
				st.unusedBytes += b.Len
			}
		}
	}
	for id := range loose {
		if _, ok := refs[id]; ok {
			kept[id] = true
		}
	}

	// ready marks a candidate and says whether it was marked long enough ago.
	ready := func(key string) bool {
		marked, ok := marks[key]
		if !ok {
			marked = now.Unix()
		}
		if opts.MarkAge > 0 && now.Sub(time.Unix(marked, 0)) < opts.MarkAge {
			nextMarks[key] = marked
			res.Marked++
			return false
		}
		nextMarks[key] = marked // kept until the delete is done
		return true
	}

	for _, id := range packOrder {
		st := packs[id]
		if !st.listed {
			continue // missing: check reports it; nothing to do here
		}
		total := st.usedBytes + st.unusedBytes
		switch {
		case st.info.Modified.After(cutoff):
			if st.unusedBytes > 0 {
				res.KeptRecent++
			}
		case len(st.used) == 0:
			st.action = "delete"
		case total > 0 && float64(st.unusedBytes) >= repackShare*float64(total):
			st.action = "repack"
		}
		if st.action != "" {
			st.markedReady = ready("p:" + id.String())
		}
	}
	var orphans []bpcrypto.ID
	for id, o := range listed {
		if _, indexed := packs[id]; indexed {
			continue
		}
		if o.Modified.After(cutoff) {
			res.KeptRecent++
			continue
		}
		if ready("p:" + id.String()) {
			orphans = append(orphans, id)
		}
	}
	var looseGone []bpcrypto.ID
	for id := range loose {
		if kept[id] {
			continue
		}
		if ready(id.String()) {
			looseGone = append(looseGone, id)
		}
	}

	if opts.DryRun {
		for _, st := range packs {
			if st.markedReady {
				res.Deleted++
				res.DeletedBytes += st.unusedBytes
			}
		}
		res.Deleted += len(orphans) + len(looseGone)
		return res, nil
	}

	// 1. Copy what is still needed out of the packs being rewritten.
	for _, id := range packOrder {
		st := packs[id]
		if st.action != "repack" || !st.markedReady {
			continue
		}
		for _, b := range st.used {
			sealed, err := r.FetchSealed(ctx, id, b)
			if err != nil {
				return res, fmt.Errorf("prune: reading blob %s from pack %s to keep it: %w", b.ID.Short(), id.Short(), err)
			}
			if err := r.RepackBlob(ctx, b.ID, sealed); err != nil {
				return res, fmt.Errorf("prune: keeping blob %s from pack %s: %w", b.ID.Short(), id.Short(), err)
			}
		}
		res.Repacked++
	}
	written, err := r.TakeWritten(ctx)
	if err != nil {
		return res, fmt.Errorf("prune: writing repacked blobs: %w", err)
	}

	// 2. Write one index for everything that stays, then remove the index
	// files it replaces. Until the old ones are gone both list the same
	// blobs, which is harmless.
	gone := func(st *packState) bool { return st.markedReady && st.action != "" }
	changed := len(written) > 0 || len(looseGone) > 0
	var records []repo.PackRecord
	for _, id := range packOrder {
		st := packs[id]
		if gone(st) {
			changed = true
			continue
		}
		records = append(records, repo.PackRecord{ID: id, Blobs: st.blobs})
	}
	records = append(records, written...)
	var keptLoose []repo.PackEntry
	goneLoose := map[bpcrypto.ID]bool{}
	for _, id := range looseGone {
		goneLoose[id] = true
	}
	for id, b := range loose {
		if !goneLoose[id] {
			keptLoose = append(keptLoose, b)
		}
	}
	if len(keptLoose) > 0 {
		records = append(records, repo.PackRecord{Blobs: keptLoose})
	}
	if changed || len(files) >= consolidateAt {
		newID, err := r.WriteIndex(ctx, repo.IndexFile{Packs: records})
		if err != nil {
			return res, fmt.Errorf("prune: writing the new index: %w", err)
		}
		for _, f := range files {
			if f.ID == newID {
				continue
			}
			if err := r.Backend().Delete(ctx, "index/"+f.ID.String()); err != nil {
				// Stop before deleting packs an old index file still lists.
				return res, fmt.Errorf("prune: removing an old index file: %w", err)
			}
		}
		res.IndexFiles = len(files)
	}

	// 3. Only now delete packs and v1 blob objects.
	del := func(key, mark string, size int64) {
		if err := r.Backend().Delete(ctx, key); err != nil {
			res.Locked++ // Object Lock: kept until its retention ends
			return
		}
		delete(nextMarks, mark)
		res.Deleted++
		res.DeletedBytes += size
	}
	for _, id := range packOrder {
		if st := packs[id]; gone(st) {
			del(repo.PackKey(id), "p:"+id.String(), st.info.Size)
		}
	}
	for _, id := range orphans {
		del(repo.PackKey(id), "p:"+id.String(), listed[id].Size)
	}
	for _, id := range looseGone {
		del(repo.DataKey(id), id.String(), loose[id].Len)
	}
	if len(marks) > 0 || len(nextMarks) > 0 {
		if err := r.SavePruneMarks(ctx, nextMarks); err != nil {
			return res, err
		}
	}
	opts.Log("prune: deleted %d objects (%d bytes), rewrote %d packs, %d waiting to be deleted, %d too recent, %d locked",
		res.Deleted, res.DeletedBytes, res.Repacked, res.Marked, res.KeptRecent, res.Locked)
	return res, nil
}
