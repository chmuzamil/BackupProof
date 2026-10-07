// Package proof produces and verifies recoverability evidence:
//
//   - in-toto Statement v1 attestations wrapped in DSSE envelopes and signed
//     with Ed25519 (backup attestations by the agent that made the backup,
//     drill attestations by the verifier that restored it);
//   - an append-only, hash-chained ledger with signed checkpoints, so evidence
//     cannot be silently removed, reordered or backdated after the fact;
//   - optional RFC 3161 timestamps from independent TSAs;
//   - self-contained bundles that a third party verifies offline, without
//     the repository password and without trusting BackupProof's database.
package proof

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const PayloadType = "application/vnd.in-toto+json"

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// Envelope is a DSSE v1 envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// PAE is the DSSE pre-authentication encoding.
func PAE(payloadType string, payload []byte) []byte {
	return []byte("DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " " + string(payload))
}

// Digest identifies an envelope (SHA-256 over its canonical JSON).
func (e *Envelope) Digest() string {
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (e *Envelope) DecodePayload() ([]byte, error) {
	return base64.StdEncoding.DecodeString(e.Payload)
}

// Key is an Ed25519 signing identity.
type Key struct {
	Name    string             `json:"name"`
	Private ed25519.PrivateKey `json:"private"`
}

// PublicKey is what verifiers need; it is safe to publish.
type PublicKey struct {
	Name   string            `json:"name"`
	KeyID  string            `json:"keyid"`
	Public ed25519.PublicKey `json:"public"`
}

func GenerateKey(name string) (*Key, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{Name: name, Private: priv}, nil
}

func KeyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "bp:" + hex.EncodeToString(sum[:8])
}

func (k *Key) Public() PublicKey {
	pub := k.Private.Public().(ed25519.PublicKey)
	return PublicKey{Name: k.Name, KeyID: KeyIDOf(pub), Public: pub}
}

// String encodes a public key compactly: "bpkey1:<name>:<base64>".
func (p PublicKey) String() string {
	return "bpkey1:" + p.Name + ":" + base64.StdEncoding.EncodeToString(p.Public)
}

func ParsePublicKey(s string) (PublicKey, error) {
	s = strings.TrimSpace(s)
	// "bpkey1:<name>:<base64>" — names may contain colons; base64 never does.
	var parts []string
	if rest, ok := strings.CutPrefix(s, "bpkey1:"); ok {
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			parts = []string{"bpkey1", rest[:i], rest[i+1:]}
		}
	}
	if len(parts) != 3 {
		var pk PublicKey
		if json.Unmarshal([]byte(s), &pk) == nil && len(pk.Public) == ed25519.PublicKeySize {
			pk.KeyID = KeyIDOf(pk.Public)
			return pk, nil
		}
		return PublicKey{}, errors.New("not a BackupProof public key")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return PublicKey{}, errors.New("malformed public key")
	}
	pub := ed25519.PublicKey(raw)
	return PublicKey{Name: parts[1], KeyID: KeyIDOf(pub), Public: pub}, nil
}

func LoadOrCreateKey(path, name string) (*Key, error) {
	if b, err := os.ReadFile(path); err == nil {
		var k Key
		if err := json.Unmarshal(b, &k); err != nil || len(k.Private) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("signing key %s is malformed", path)
		}
		return &k, nil
	}
	k, err := GenerateKey(name)
	if err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(k, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// Sign wraps payload in a signed DSSE envelope.
func (k *Key) Sign(payload []byte) *Envelope {
	sig := ed25519.Sign(k.Private, PAE(PayloadType, payload))
	return &Envelope{
		PayloadType: PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []Signature{{KeyID: k.Public().KeyID, Sig: base64.StdEncoding.EncodeToString(sig)}},
	}
}

// Verify checks that at least one signature is valid for a trusted key and
// returns the matching key.
func (e *Envelope) Verify(trusted []PublicKey) (PublicKey, error) {
	payload, err := e.DecodePayload()
	if err != nil {
		return PublicKey{}, fmt.Errorf("payload: %w", err)
	}
	msg := PAE(e.PayloadType, payload)
	for _, s := range e.Signatures {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		for _, k := range trusted {
			if k.KeyID == s.KeyID && ed25519.Verify(k.Public, msg, sig) {
				return k, nil
			}
		}
	}
	return PublicKey{}, errors.New("no valid signature from a trusted key")
}
