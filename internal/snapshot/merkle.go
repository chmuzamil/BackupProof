package snapshot

import (
	"errors"
	"sort"
	"strconv"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
)

// The content root is an RFC 6962-style Merkle tree over the sorted
// manifest entries, using unkeyed BLAKE3. Unlike blob IDs it does not depend
// on any repository secret, so anyone holding restored data can recompute it
// and compare it with the root in a signed attestation, and a single file can
// be proven to be part of a snapshot with an inclusion proof.

var (
	leafPrefix  = []byte{0x00}
	nodePrefix  = []byte{0x01}
	emptyPrefix = []byte{0x02}
)

// LeafHash commits to the fields that survive any restore: path, type, size,
// content hash and symlink target. Mode, owner and mtime are excluded because
// they legitimately differ across platforms and restore targets.
func LeafHash(e *Entry) bpcrypto.ID {
	sep := []byte{0}
	t := e.Type
	if t == TypeStream {
		t = TypeFile // streams restore as regular files
	}
	return bpcrypto.Hash(leafPrefix,
		[]byte(t), sep,
		[]byte(e.Path), sep,
		[]byte(strconv.FormatInt(e.Size, 10)), sep,
		[]byte(e.Hash), sep,
		[]byte(e.Link))
}

func nodeHash(l, r bpcrypto.ID) bpcrypto.ID { return bpcrypto.Hash(nodePrefix, l[:], r[:]) }

// SortEntries orders entries canonically (byte-wise path order).
func SortEntries(entries []*Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
}

// Root computes the Merkle root of entries, which must already be sorted.
func Root(entries []*Entry) bpcrypto.ID {
	leaves := make([]bpcrypto.ID, len(entries))
	for i, e := range entries {
		leaves[i] = LeafHash(e)
	}
	return RootOfLeaves(leaves)
}

func RootOfLeaves(leaves []bpcrypto.ID) bpcrypto.ID {
	if len(leaves) == 0 {
		return bpcrypto.Hash(emptyPrefix)
	}
	return mth(leaves)
}

func mth(l []bpcrypto.ID) bpcrypto.ID {
	if len(l) == 1 {
		return l[0]
	}
	k := split(len(l))
	return nodeHash(mth(l[:k]), mth(l[k:]))
}

// split returns the largest power of two smaller than n.
func split(n int) int {
	k := 1
	for k<<1 < n {
		k <<= 1
	}
	return k
}

// InclusionProof returns the audit path for leaf index in leaves.
func InclusionProof(leaves []bpcrypto.ID, index int) ([]bpcrypto.ID, error) {
	if index < 0 || index >= len(leaves) {
		return nil, errors.New("index out of range")
	}
	return auditPath(index, leaves), nil
}

func auditPath(m int, l []bpcrypto.ID) []bpcrypto.ID {
	if len(l) <= 1 {
		return nil
	}
	k := split(len(l))
	if m < k {
		return append(auditPath(m, l[:k]), mth(l[k:]))
	}
	return append(auditPath(m-k, l[k:]), mth(l[:k]))
}

// VerifyInclusion checks an audit path (RFC 9162 §2.1.3.2).
func VerifyInclusion(leaf bpcrypto.ID, index, size int, proof []bpcrypto.ID, root bpcrypto.ID) bool {
	if index < 0 || index >= size {
		return false
	}
	fn, sn := index, size-1
	r := leaf
	for _, p := range proof {
		if sn == 0 {
			return false
		}
		if fn&1 == 1 || fn == sn {
			r = nodeHash(p, r)
			if fn&1 == 0 {
				for fn&1 == 0 && fn != 0 {
					fn >>= 1
					sn >>= 1
				}
			}
		} else {
			r = nodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	return sn == 0 && r == root
}
