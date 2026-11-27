package transparency

import (
	"context"
	"fmt"

	"golang.org/x/mod/sumdb/tlog"
)

// LeafData is what the log records for a ledger entry: its hash, as the hex
// text the ledger stores. Nothing else about the entry is published.
func LeafData(entryHash string) []byte { return []byte(entryHash) }

// TileFetcher gets a tile or entry bundle by its C2SP path below the log's prefix.
type TileFetcher func(ctx context.Context, path string) ([]byte, error)

type tileReader struct {
	ctx   context.Context
	fetch TileFetcher
}

func (t tileReader) Height() int { return TileHeight }

func (t tileReader) ReadTiles(tiles []tlog.Tile) ([][]byte, error) {
	out := make([][]byte, len(tiles))
	for i, tile := range tiles {
		b, err := t.fetch(t.ctx, TilePath(tile))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", TilePath(tile), err)
		}
		out[i] = b
	}
	return out, nil
}

func (t tileReader) SaveTiles([]tlog.Tile, [][]byte) {}

// VerifyInclusion proves that leaf is entry number index of the log at a
// checkpoint, from tiles fetched from the log. Every tile is checked against
// the checkpoint's root, so a log can't serve tiles that don't add up.
func VerifyInclusion(ctx context.Context, fetch TileFetcher, c Checkpoint, index int64, leaf []byte) error {
	if index < 0 || index >= c.Size {
		return fmt.Errorf("entry %d isn't in a log of %d entries", index, c.Size)
	}
	hr := tlog.TileHashReader(tlog.Tree{N: c.Size, Hash: c.Root}, tileReader{ctx: ctx, fetch: fetch})
	proof, err := tlog.ProveRecord(c.Size, index, hr)
	if err != nil {
		return err
	}
	return tlog.CheckRecord(proof, c.Size, c.Root, index, tlog.RecordHash(leaf))
}

// VerifyConsistency proves a newer checkpoint extends an older one.
func VerifyConsistency(ctx context.Context, fetch TileFetcher, older, newer Checkpoint) error {
	if older.Origin != newer.Origin || older.Size > newer.Size {
		return fmt.Errorf("checkpoint at %d can't extend one at %d", newer.Size, older.Size)
	}
	if older.Size == newer.Size {
		if older.Root != newer.Root {
			return fmt.Errorf("two different roots at size %d: the log forked", older.Size)
		}
		return nil
	}
	hr := tlog.TileHashReader(tlog.Tree{N: newer.Size, Hash: newer.Root}, tileReader{ctx: ctx, fetch: fetch})
	proof, err := tlog.ProveTree(newer.Size, older.Size, hr)
	if err != nil {
		return err
	}
	return tlog.CheckTree(proof, newer.Size, newer.Root, older.Size, older.Root)
}

// MemoryLog is a whole log in memory: the tests' log, and the shape of the
// dashboard's, which keeps the same stored hashes in its database.
type MemoryLog struct {
	leaves [][]byte
	hashes []tlog.Hash // tlog stored hashes, by StoredHashIndex
}

func (m *MemoryLog) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	out := make([]tlog.Hash, len(indexes))
	for i, x := range indexes {
		if x < 0 || x >= int64(len(m.hashes)) {
			return nil, fmt.Errorf("no stored hash %d", x)
		}
		out[i] = m.hashes[x]
	}
	return out, nil
}

// Append adds an entry.
func (m *MemoryLog) Append(leaf []byte) error {
	hs, err := tlog.StoredHashes(int64(len(m.leaves)), leaf, m)
	if err != nil {
		return err
	}
	m.leaves = append(m.leaves, leaf)
	m.hashes = append(m.hashes, hs...)
	return nil
}

func (m *MemoryLog) Size() int64 { return int64(len(m.leaves)) }

// Checkpoint is the unsigned checkpoint at the current size.
func (m *MemoryLog) Checkpoint(origin string) (Checkpoint, error) {
	root, err := tlog.TreeHash(m.Size(), m)
	return Checkpoint{Origin: origin, Size: m.Size(), Root: root}, err
}

// Tile serves a C2SP path, as the dashboard does.
func (m *MemoryLog) Tile(path string) ([]byte, error) {
	return ServeTile(path, m.Size(), m, func(i int64) []byte { return m.leaves[i] })
}

// ServeTile answers a tile or entry bundle request for a log of size n.
// A tile beyond the log, or wider than it has grown, is refused.
func ServeTile(path string, n int64, hr tlog.HashReader, entry func(int64) []byte) ([]byte, error) {
	t, err := ParseTilePath(path)
	if err != nil {
		return nil, err
	}
	if t.L == -1 {
		start := t.N << TileHeight
		if start+int64(t.W) > n {
			return nil, fmt.Errorf("entry bundle %s is beyond the log", path)
		}
		entries := make([][]byte, t.W)
		for i := range entries {
			entries[i] = entry(start + int64(i))
		}
		return EntryBundle(entries), nil
	}
	// The tile's last hash covers this many leaves.
	span := int64(1) << (TileHeight * t.L)
	if (t.N<<TileHeight+int64(t.W))*span > n {
		return nil, fmt.Errorf("tile %s is beyond the log", path)
	}
	return tlog.ReadTileData(t, hr)
}

// ConsistencyProof proves size old is a prefix of the log now.
func (m *MemoryLog) ConsistencyProof(old int64) (tlog.TreeProof, error) {
	return tlog.ProveTree(m.Size(), old, m)
}
