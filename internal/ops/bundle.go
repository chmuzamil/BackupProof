package ops

import (
	"errors"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
)

type backendInfo = backend.ObjectInfo

// BuildBundle assembles an offline-verifiable bundle for rec: the ledger
// segment from rec's entry to the current head, plus a fresh checkpoint over
// that head signed by ledgerKey.
func BuildBundle(rec *Record, ledger []proof.LedgerEntry, ledgerKey *proof.Key, origin string, signerKeys []proof.PublicKey) (*proof.Bundle, error) {
	if rec.Ledger == nil {
		return nil, errors.New("proof was not recorded in a ledger")
	}
	var seg []proof.LedgerEntry
	for _, e := range ledger {
		if e.Seq >= rec.Ledger.Seq {
			seg = append(seg, e)
		}
	}
	if len(seg) == 0 || seg[0].Hash != rec.Ledger.Hash {
		return nil, errors.New("ledger does not contain this proof's entry")
	}
	cp := proof.SignCheckpoint(ledgerKey, origin, seg[len(seg)-1], time.Now())
	keys := append([]proof.PublicKey{}, signerKeys...)
	if lk := ledgerKey.Public(); !containsKey(keys, lk.KeyID) {
		keys = append(keys, lk)
	}
	return &proof.Bundle{
		Format: proof.BundleFormat, Envelope: rec.Envelope, Timestamp: rec.Timestamp,
		Ledger: seg, Checkpoint: &cp, Keys: keys,
	}, nil
}

func containsKey(keys []proof.PublicKey, id string) bool {
	for _, k := range keys {
		if k.KeyID == id {
			return true
		}
	}
	return false
}
