package proof

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const BundleFormat = "backupproof-bundle/v1"

// Bundle is everything a third party needs to verify one attestation
// offline: the signed envelope, its trusted timestamp, the ledger segment
// linking it to a signed checkpoint, and the public keys involved.
type Bundle struct {
	Format     string        `json:"format"`
	Envelope   *Envelope     `json:"envelope"`
	Timestamp  *Timestamp    `json:"timestamp,omitempty"`
	Ledger     []LedgerEntry `json:"ledger"`
	Checkpoint *Checkpoint   `json:"checkpoint,omitempty"`
	Keys       []PublicKey   `json:"keys"`
}

type VerifyOptions struct {
	// TrustedKeys pins the signer and checkpoint keys. When empty, keys
	// embedded in the bundle are used and the report carries a warning.
	TrustedKeys []PublicKey
	// TSARoots, when set, requires timestamp certificates to chain to them.
	TSARoots *x509.CertPool
	// RequireTimestamp fails verification when no RFC 3161 token is present.
	RequireTimestamp bool
}

type Report struct {
	Valid         bool            `json:"valid"`
	PredicateType string          `json:"predicateType"`
	Subject       Subject         `json:"subject"`
	Signer        string          `json:"signer"`
	LedgerSeq     int64           `json:"ledgerSeq"`
	CheckpointSeq int64           `json:"checkpointSeq,omitempty"`
	TimestampedAt *time.Time      `json:"timestampedAt,omitempty"`
	TimestampTSA  string          `json:"timestampTsa,omitempty"`
	Predicate     json.RawMessage `json:"predicate"`
	Passed        *bool           `json:"passed,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
	Errors        []string        `json:"errors,omitempty"`
}

func VerifyBundle(b *Bundle, opts VerifyOptions) *Report {
	r := &Report{}
	fail := func(format string, a ...any) { r.Errors = append(r.Errors, fmt.Sprintf(format, a...)) }
	if b.Format != BundleFormat {
		fail("unknown bundle format %q", b.Format)
		return r
	}
	if b.Envelope == nil {
		fail("bundle has no envelope")
		return r
	}
	trusted := opts.TrustedKeys
	if len(trusted) == 0 {
		trusted = b.Keys
		r.Warnings = append(r.Warnings, "no trusted keys supplied: verified against keys embedded in the bundle; pin keys with --key for independent verification")
	}

	// 1. Signature.
	signer, err := b.Envelope.Verify(trusted)
	if err != nil {
		fail("signature: %v", err)
	} else {
		r.Signer = signer.Name + " (" + signer.KeyID + ")"
	}

	// 2. Statement.
	st, err := ParseStatement(b.Envelope)
	if err != nil {
		fail("statement: %v", err)
		return r
	}
	r.PredicateType, r.Subject, r.Predicate = st.PredicateType, st.Subject[0], st.Predicate
	if st.PredicateType == PredicateDrill {
		var p DrillPredicate
		if json.Unmarshal(st.Predicate, &p) == nil {
			passed := p.Passed && p.RestoredRoot == p.ExpectedRoot && p.ExpectedRoot == st.Subject[0].Digest["blake3"]
			r.Passed = &passed
			if p.Passed && !passed {
				fail("drill claims success but restored root does not match the subject digest")
			}
		}
	}

	// 3. Trusted timestamp.
	envBytes, _ := json.Marshal(b.Envelope)
	if b.Timestamp != nil {
		t, err := b.Timestamp.Verify(envBytes, opts.TSARoots)
		if err != nil {
			fail("timestamp: %v", err)
		} else {
			r.TimestampedAt, r.TimestampTSA = &t, b.Timestamp.TSA
			if opts.TSARoots == nil {
				r.Warnings = append(r.Warnings, "timestamp signature checked but TSA certificate chain not pinned (use --tsa-root)")
			}
		}
	} else if opts.RequireTimestamp {
		fail("no RFC 3161 timestamp present")
	}

	// 4. Ledger linkage: first entry must be this envelope; chain must be
	// unbroken up to the signed checkpoint.
	if len(b.Ledger) == 0 {
		r.Warnings = append(r.Warnings, "no ledger segment: completeness and ordering not proven")
	} else {
		first := b.Ledger[0]
		r.LedgerSeq = first.Seq
		if first.EnvelopeDigest != b.Envelope.Digest() {
			fail("ledger entry %d does not reference this envelope", first.Seq)
		}
		if err := VerifyChain(b.Ledger); err != nil {
			fail("ledger: %v", err)
		}
		if b.Checkpoint == nil {
			r.Warnings = append(r.Warnings, "no signed checkpoint")
		} else {
			last := b.Ledger[len(b.Ledger)-1]
			if err := b.Checkpoint.Verify(trusted); err != nil {
				fail("checkpoint: %v", err)
			} else if b.Checkpoint.Seq != last.Seq || b.Checkpoint.Head != last.Hash {
				fail("ledger segment does not end at the signed checkpoint")
			} else {
				r.CheckpointSeq = b.Checkpoint.Seq
			}
		}
	}
	r.Valid = len(r.Errors) == 0
	return r
}

func ParseBundle(data []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	if b.Format != BundleFormat {
		return nil, errors.New("not a BackupProof proof bundle")
	}
	return &b, nil
}
