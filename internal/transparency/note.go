// Package transparency publishes the proof ledger as a C2SP transparency log
// (c2sp.org/tlog-tiles, tlog-checkpoint) and has its checkpoints cosigned by
// independent witnesses (c2sp.org/tlog-witness, tlog-cosignature). A witness
// only cosigns a checkpoint that extends the last one it saw, so a dashboard
// can't show one auditor a history and another a different one, or quietly
// rewrite the past, without the witnesses noticing.
//
// The Merkle tree and its proofs are RFC 6962, from golang.org/x/mod/sumdb/tlog.
package transparency

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/sumdb/tlog"
)

const (
	sigEd25519      byte = 0x01 // a log's own signature (c2sp.org/signed-note)
	sigCosignature  byte = 0x04 // a timestamped Ed25519 checkpoint cosignature
	dash                 = "— " // U+2014 and a space start every signature line
	maxSignatureLen      = 100
)

// Signer is an Ed25519 key with the name it signs under: the log's origin,
// or a witness's name.
type Signer struct {
	Name string
	Key  ed25519.PrivateKey
}

// Verifier is the public half, as a C2SP verifier key: name+keyid+base64(type||key).
type Verifier struct {
	Name string
	Type byte
	Key  ed25519.PublicKey
}

func keyID(name string, typ byte, pub ed25519.PublicKey) uint32 {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{'\n', typ})
	h.Write(pub)
	return binary.BigEndian.Uint32(h.Sum(nil)[:4])
}

// ValidName reports whether s can name a log or a cosigner: non-empty, at
// most 255 bytes, valid UTF-8, no spaces, no "+" and no control characters.
func ValidName(s string) bool {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '+' || unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// VerifierKey formats the verifier key of a log (typ 0x01) or a witness (0x04).
func VerifierKey(name string, typ byte, pub ed25519.PublicKey) string {
	return fmt.Sprintf("%s+%08x+%s", name, keyID(name, typ, pub), base64.StdEncoding.EncodeToString(append([]byte{typ}, pub...)))
}

// LogVerifierKey and WitnessVerifierKey are VerifierKey for each role.
func LogVerifierKey(name string, pub ed25519.PublicKey) string {
	return VerifierKey(name, sigEd25519, pub)
}
func WitnessVerifierKey(name string, pub ed25519.PublicKey) string {
	return VerifierKey(name, sigCosignature, pub)
}

// ParseVerifier reads a verifier key and checks its key ID.
func ParseVerifier(vkey string) (Verifier, error) {
	parts := strings.SplitN(strings.TrimSpace(vkey), "+", 3) // base64 can hold "+"
	if len(parts) != 3 || !ValidName(parts[0]) {
		return Verifier{}, errors.New("a verifier key looks like name+1234abcd+base64")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(raw) != 1+ed25519.PublicKeySize || (raw[0] != sigEd25519 && raw[0] != sigCosignature) {
		return Verifier{}, errors.New("the verifier key isn't an Ed25519 log or witness key")
	}
	v := Verifier{Name: parts[0], Type: raw[0], Key: ed25519.PublicKey(raw[1:])}
	if id, err := strconv.ParseUint(parts[1], 16, 32); err != nil || uint32(id) != keyID(v.Name, v.Type, v.Key) || len(parts[1]) != 8 {
		return Verifier{}, errors.New("the verifier key's ID doesn't match its key")
	}
	return v, nil
}

func (v Verifier) id() uint32 { return keyID(v.Name, v.Type, v.Key) }

// String is the verifier key.
func (v Verifier) String() string { return VerifierKey(v.Name, v.Type, v.Key) }

// --- checkpoints ---------------------------------------------------------------

// Checkpoint is the body of a checkpoint note: origin, size and root hash.
type Checkpoint struct {
	Origin string
	Size   int64
	Root   tlog.Hash
}

// Body is the checkpoint's note text, which signatures cover.
func (c Checkpoint) Body() string {
	return fmt.Sprintf("%s\n%d\n%s\n", c.Origin, c.Size, base64.StdEncoding.EncodeToString(c.Root[:]))
}

// ParseCheckpoint reads a checkpoint's note text (without signatures).
// Extension lines are refused: this log doesn't write any.
func ParseCheckpoint(body string) (Checkpoint, error) {
	lines := strings.Split(body, "\n")
	if len(lines) != 4 || lines[3] != "" {
		return Checkpoint{}, errors.New("a checkpoint is three lines: origin, size, root")
	}
	var c Checkpoint
	c.Origin = lines[0]
	if !ValidName(c.Origin) {
		return c, errors.New("the checkpoint's origin isn't valid")
	}
	n, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != lines[1] {
		return c, errors.New("the checkpoint's size isn't a plain number")
	}
	c.Size = n
	root, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil || len(root) != tlog.HashSize {
		return c, errors.New("the checkpoint's root hash isn't valid")
	}
	copy(c.Root[:], root)
	return c, nil
}

// --- notes ----------------------------------------------------------------------

// Note is note text and its signature lines.
type Note struct {
	Text string
	Sigs []string // each without the leading "— " and trailing newline: "name base64"
}

// String formats the note.
func (n Note) String() string {
	var b strings.Builder
	b.WriteString(n.Text)
	b.WriteString("\n")
	for _, s := range n.Sigs {
		b.WriteString(dash + s + "\n")
	}
	return b.String()
}

// ParseNote splits a signed note into its text and signature lines.
func ParseNote(raw string) (Note, error) {
	if !utf8.ValidString(raw) {
		return Note{}, errors.New("a note must be UTF-8")
	}
	i := strings.LastIndex(raw, "\n\n")
	if i < 0 {
		return Note{}, errors.New("a note has its text, a blank line, then signatures")
	}
	n := Note{Text: raw[:i+1]}
	for _, r := range n.Text {
		if r < 0x20 && r != '\n' {
			return Note{}, errors.New("a note's text can't hold control characters")
		}
	}
	rest := raw[i+2:]
	if !strings.HasSuffix(rest, "\n") {
		return Note{}, errors.New("a note's last signature line must end with a newline")
	}
	for _, line := range strings.Split(strings.TrimSuffix(rest, "\n"), "\n") {
		s, ok := strings.CutPrefix(line, dash)
		if !ok {
			return Note{}, errors.New("a signature line starts with an em dash and a space")
		}
		n.Sigs = append(n.Sigs, s)
	}
	if len(n.Sigs) == 0 || len(n.Sigs) > maxSignatureLen {
		return Note{}, errors.New("a note needs between 1 and 100 signatures")
	}
	return n, nil
}

func sigLine(name string, blob []byte) string {
	return name + " " + base64.StdEncoding.EncodeToString(blob)
}

// splitSig returns a signature line's name, key ID and the bytes after it.
func splitSig(line string) (string, uint32, []byte, bool) {
	name, b64, ok := strings.Cut(line, " ")
	if !ok || !ValidName(name) {
		return "", 0, nil, false
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(blob) < 5 {
		return "", 0, nil, false
	}
	return name, binary.BigEndian.Uint32(blob[:4]), blob[4:], true
}

// Sign adds the log's own Ed25519 signature (type 0x01) to a note text.
func (s Signer) Sign(text string) string {
	id := keyID(s.Name, sigEd25519, s.Key.Public().(ed25519.PublicKey))
	blob := binary.BigEndian.AppendUint32(nil, id)
	blob = append(blob, ed25519.Sign(s.Key, []byte(text))...)
	return sigLine(s.Name, blob)
}

// cosignMessage is what a timestamped cosignature covers (c2sp.org/tlog-cosignature).
func cosignMessage(t int64, checkpointBody string) []byte {
	return []byte("cosignature/v1\ntime " + strconv.FormatInt(t, 10) + "\n" + checkpointBody)
}

// Cosign makes a witness's timestamped cosignature (type 0x04) of a checkpoint.
func (s Signer) Cosign(checkpointBody string, at time.Time) string {
	t := at.Unix()
	id := keyID(s.Name, sigCosignature, s.Key.Public().(ed25519.PublicKey))
	blob := binary.BigEndian.AppendUint32(nil, id)
	blob = binary.BigEndian.AppendUint64(blob, uint64(t))
	blob = append(blob, ed25519.Sign(s.Key, cosignMessage(t, checkpointBody))...)
	return sigLine(s.Name, blob)
}

// Verify checks a signature line by this verifier. ok is false when the line
// is by another key; err is set when it is by this key and doesn't verify.
// For a cosignature it returns the time the witness gave.
func (v Verifier) Verify(text, line string) (ok bool, at time.Time, err error) {
	name, id, rest, valid := splitSig(line)
	if !valid || name != v.Name || id != v.id() {
		return false, time.Time{}, nil
	}
	switch v.Type {
	case sigEd25519:
		if len(rest) != ed25519.SignatureSize || !ed25519.Verify(v.Key, []byte(text), rest) {
			return true, time.Time{}, fmt.Errorf("%s's signature doesn't verify", v.Name)
		}
		return true, time.Time{}, nil
	case sigCosignature:
		if len(rest) != 8+ed25519.SignatureSize {
			return true, time.Time{}, fmt.Errorf("%s's cosignature is malformed", v.Name)
		}
		t := int64(binary.BigEndian.Uint64(rest[:8]))
		if !ed25519.Verify(v.Key, cosignMessage(t, text), rest[8:]) {
			return true, time.Time{}, fmt.Errorf("%s's cosignature doesn't verify", v.Name)
		}
		return true, time.Unix(t, 0).UTC(), nil
	}
	return false, time.Time{}, nil
}

// VerifyNote checks a signed checkpoint: the log's signature must be there
// and verify, and so must a cosignature from every witness given. Signatures
// from keys not given are ignored.
func VerifyNote(raw string, log Verifier, witnesses []Verifier) (Checkpoint, map[string]time.Time, error) {
	n, err := ParseNote(raw)
	if err != nil {
		return Checkpoint{}, nil, err
	}
	c, err := ParseCheckpoint(n.Text)
	if err != nil {
		return c, nil, err
	}
	if c.Origin != log.Name {
		return c, nil, fmt.Errorf("the checkpoint is for %q, not %q", c.Origin, log.Name)
	}
	logOK := false
	cosigned := map[string]time.Time{}
	for _, line := range n.Sigs {
		if ok, _, err := log.Verify(n.Text, line); ok {
			if err != nil {
				return c, nil, err
			}
			logOK = true
		}
		for _, w := range witnesses {
			if ok, at, err := w.Verify(n.Text, line); ok {
				if err != nil {
					return c, nil, err
				}
				cosigned[w.Name] = at
			}
		}
	}
	if !logOK {
		return c, nil, errors.New("the checkpoint isn't signed by the log's key")
	}
	for _, w := range witnesses {
		if _, ok := cosigned[w.Name]; !ok {
			return c, cosigned, fmt.Errorf("the witness %s hasn't cosigned this checkpoint", w.Name)
		}
	}
	return c, cosigned, nil
}

// --- tiles ---------------------------------------------------------------------

// TileHeight is the C2SP tile height: 256 hashes per full tile.
const TileHeight = 8

// TilePath is a tile's path below the log's URL prefix: tile/<L>/<N>[.p/<W>],
// or tile/entries/<N>[.p/<W>] for an entry bundle (L = -1).
func TilePath(t tlog.Tile) string {
	p := t.Path() // tile/8/<L>/<N>[.p/<W>] in x/mod's form, with the height
	p = strings.Replace(p, "tile/"+strconv.Itoa(t.H)+"/", "tile/", 1)
	return strings.Replace(p, "tile/data/", "tile/entries/", 1)
}

// ParseTilePath reads a C2SP tile path back into a tile.
func ParseTilePath(p string) (tlog.Tile, error) {
	rest, ok := strings.CutPrefix(p, "tile/")
	if !ok {
		return tlog.Tile{}, errors.New("not a tile path")
	}
	if e, ok := strings.CutPrefix(rest, "entries/"); ok {
		rest = "data/" + e
	}
	return tlog.ParseTilePath("tile/" + strconv.Itoa(TileHeight) + "/" + rest)
}

// EntryBundle encodes entries as C2SP entry bundles: each with a big-endian
// uint16 length.
func EntryBundle(entries [][]byte) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(e))))
		b.Write(e)
	}
	return b.Bytes()
}

// ParseEntryBundle splits an entry bundle into its entries.
func ParseEntryBundle(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, errors.New("truncated entry bundle")
		}
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			return nil, errors.New("truncated entry bundle")
		}
		out = append(out, b[2:2+n])
		b = b[2+n:]
	}
	return out, nil
}

// OriginHash names a log in a witness's monitoring URL: hex SHA-256 of its origin.
func OriginHash(origin string) string {
	h := sha256.Sum256([]byte(origin))
	return hex.EncodeToString(h[:])
}
