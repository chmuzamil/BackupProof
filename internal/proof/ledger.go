package proof

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// GenesisHash is the "previous hash" of the first ledger entry.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// LedgerEntry is one link in the hash chain. Each entry commits to the
// envelope digest and to the previous entry, so removing, reordering or
// editing any past entry changes every later hash.
type LedgerEntry struct {
	Seq            int64     `json:"seq"`
	Time           time.Time `json:"time"`
	Kind           string    `json:"kind"` // backup | drill | check | event
	Subject        string    `json:"subject"`
	EnvelopeDigest string    `json:"envelopeDigest"`
	Prev           string    `json:"prev"`
	Hash           string    `json:"hash"`
}

func EntryHash(e LedgerEntry) string {
	h := sha256.New()
	for _, part := range []string{"bpledger/v1", strconv.FormatInt(e.Seq, 10), e.Time.UTC().Format(time.RFC3339Nano), e.Kind, e.Subject, e.EnvelopeDigest, e.Prev} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NextEntry creates the entry following prev (nil for the first entry).
func NextEntry(prev *LedgerEntry, kind, subject, envelopeDigest string, now time.Time) LedgerEntry {
	e := LedgerEntry{Seq: 1, Time: now.UTC(), Kind: kind, Subject: subject, EnvelopeDigest: envelopeDigest, Prev: GenesisHash}
	if prev != nil {
		e.Seq = prev.Seq + 1
		e.Prev = prev.Hash
	}
	e.Hash = EntryHash(e)
	return e
}

// VerifyChain checks that entries form an unbroken chain. The first entry is
// trusted as an anchor unless it has Seq 1, in which case it must start at genesis.
func VerifyChain(entries []LedgerEntry) error {
	for i, e := range entries {
		if EntryHash(e) != e.Hash {
			return fmt.Errorf("ledger entry %d: hash mismatch (entry was modified)", e.Seq)
		}
		if i == 0 {
			if e.Seq == 1 && e.Prev != GenesisHash {
				return errors.New("ledger entry 1 does not start at genesis")
			}
			continue
		}
		p := entries[i-1]
		if e.Seq != p.Seq+1 || e.Prev != p.Hash {
			return fmt.Errorf("ledger broken between entries %d and %d (entry removed or reordered)", p.Seq, e.Seq)
		}
	}
	return nil
}

// Checkpoint is a signed statement of the ledger head.
type Checkpoint struct {
	Origin string    `json:"origin"`
	Seq    int64     `json:"seq"`
	Head   string    `json:"head"`
	Time   time.Time `json:"time"`
	KeyID  string    `json:"keyid"`
	Sig    string    `json:"sig"`
	// Timestamp is an optional RFC 3161 token over the checkpoint body.
	Timestamp *Timestamp `json:"timestamp,omitempty"`
}

func (c *Checkpoint) body() []byte {
	return []byte(fmt.Sprintf("backupproof-checkpoint/v1\n%s\n%d\n%s\n%s\n", c.Origin, c.Seq, c.Head, c.Time.UTC().Format(time.RFC3339Nano)))
}

func SignCheckpoint(k *Key, origin string, head LedgerEntry, now time.Time) Checkpoint {
	c := Checkpoint{Origin: origin, Seq: head.Seq, Head: head.Hash, Time: now.UTC(), KeyID: k.Public().KeyID}
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(k.Private, c.body()))
	return c
}

func (c *Checkpoint) Verify(trusted []PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(c.Sig)
	if err != nil {
		return err
	}
	for _, k := range trusted {
		if k.KeyID == c.KeyID && ed25519.Verify(k.Public, c.body(), sig) {
			return nil
		}
	}
	return errors.New("checkpoint signature is not from a trusted key")
}

// FileLedger is an append-only JSONL ledger used by the standalone CLI. The
// server keeps the same chain in SQLite guarded by append-only triggers.
type FileLedger struct {
	mu   sync.Mutex
	path string
	head *LedgerEntry
}

func OpenFileLedger(path string) (*FileLedger, error) {
	l := &FileLedger{path: path}
	entries, err := l.All()
	if err != nil {
		return nil, err
	}
	if err := VerifyChain(entries); err != nil {
		return nil, fmt.Errorf("ledger %s is corrupt: %w", path, err)
	}
	if n := len(entries); n > 0 {
		l.head = &entries[n-1]
	}
	return l, nil
}

func (l *FileLedger) All() ([]LedgerEntry, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []LedgerEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func (l *FileLedger) Head() *LedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.head
}

func (l *FileLedger) Append(kind, subject, envelopeDigest string) (LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := NextEntry(l.head, kind, subject, envelopeDigest, time.Now())
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return e, err
	}
	b, _ := json.Marshal(e)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return e, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return e, err
	}
	if err := f.Close(); err != nil {
		return e, err
	}
	l.head = &e
	return e, nil
}
