// Package crypto holds the repository key hierarchy and blob encryption.
//
//	password --argon2id--> KEK --XChaCha20-Poly1305--> MasterKeys
//	MasterKeys.Enc  encrypts every blob (XChaCha20-Poly1305, random 24-byte nonce)
//	MasterKeys.MAC  derives content IDs: BLAKE3-keyed(MAC, plaintext)
//	MasterKeys.Seed derives the chunker gear table
//
// Content IDs are keyed so that storage observers cannot test whether a known
// file is present in the repository.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"lukechampine.com/blake3"
)

type MasterKeys struct {
	Enc  []byte `json:"enc"`
	MAC  []byte `json:"mac"`
	Seed []byte `json:"seed"`
}

func NewMasterKeys() (*MasterKeys, error) {
	k := &MasterKeys{Enc: make([]byte, 32), MAC: make([]byte, 32), Seed: make([]byte, 32)}
	for _, b := range [][]byte{k.Enc, k.MAC, k.Seed} {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// KDFParams are stored next to each wrapped key so they can be raised later.
type KDFParams struct {
	Algo    string `json:"algo"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memoryKiB"`
	Threads uint8  `json:"threads"`
	Salt    []byte `json:"salt"`
}

func DefaultKDF() (KDFParams, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return KDFParams{}, err
	}
	return KDFParams{Algo: "argon2id", Time: 3, Memory: 64 * 1024, Threads: 4, Salt: salt}, nil
}

func (p KDFParams) Derive(password []byte) ([]byte, error) {
	if p.Algo != "argon2id" {
		return nil, fmt.Errorf("unsupported kdf %q", p.Algo)
	}
	return argon2.IDKey(password, p.Salt, p.Time, p.Memory, p.Threads, 32), nil
}

// KeyFile is the on-repo representation of one password slot.
type KeyFile struct {
	Version int       `json:"version"`
	KDF     KDFParams `json:"kdf"`
	Wrapped []byte    `json:"wrapped"`
	Created string    `json:"created"`
	Host    string    `json:"host,omitempty"`
}

var ErrWrongPassword = errors.New("wrong repository password")

func WrapKeys(mk *MasterKeys, password []byte, kdf KDFParams) (*KeyFile, error) {
	kek, err := kdf.Derive(password)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(mk)
	if err != nil {
		return nil, err
	}
	wrapped, err := seal(kek, plain, []byte("backupproof/key/v1"))
	if err != nil {
		return nil, err
	}
	return &KeyFile{Version: 1, KDF: kdf, Wrapped: wrapped}, nil
}

func UnwrapKeys(kf *KeyFile, password []byte) (*MasterKeys, error) {
	kek, err := kf.KDF.Derive(password)
	if err != nil {
		return nil, err
	}
	plain, err := open(kek, kf.Wrapped, []byte("backupproof/key/v1"))
	if err != nil {
		return nil, ErrWrongPassword
	}
	var mk MasterKeys
	if err := json.Unmarshal(plain, &mk); err != nil {
		return nil, err
	}
	if len(mk.Enc) != 32 || len(mk.MAC) != 32 || len(mk.Seed) != 32 {
		return nil, errors.New("key file is malformed")
	}
	return &mk, nil
}

// ID is a 32-byte content identifier.
type ID [32]byte

func (id ID) String() string { return hex.EncodeToString(id[:]) }
func (id ID) Short() string  { return hex.EncodeToString(id[:4]) }

func ParseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return id, fmt.Errorf("invalid id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

// ContentID is the keyed hash used to name blobs.
func (k *MasterKeys) ContentID(plain []byte) ID {
	var id ID
	h := blake3.New(32, k.MAC)
	_, _ = h.Write(plain) // hash writes never fail
	copy(id[:], h.Sum(nil))
	return id
}

// Seal encrypts data, binding it to aad (blob type + id) so blobs cannot be
// swapped between names without detection.
func (k *MasterKeys) Seal(plain, aad []byte) ([]byte, error) { return seal(k.Enc, plain, aad) }
func (k *MasterKeys) Open(ct, aad []byte) ([]byte, error)    { return open(k.Enc, ct, aad) }

func seal(key, plain, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, chacha20poly1305.NonceSizeX, chacha20poly1305.NonceSizeX+len(plain)+aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[:chacha20poly1305.NonceSizeX], plain, aad), nil
}

func open(key, ct, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(ct) < chacha20poly1305.NonceSizeX+aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, ct[:chacha20poly1305.NonceSizeX], ct[chacha20poly1305.NonceSizeX:], aad)
}

// Hash is the unkeyed BLAKE3-256 used for file content hashes and Merkle trees.
func Hash(parts ...[]byte) ID {
	h := blake3.New(32, nil)
	for _, p := range parts {
		_, _ = h.Write(p) // hash writes never fail
	}
	var id ID
	copy(id[:], h.Sum(nil))
	return id
}

// SealWithKey / OpenWithKey encrypt small secrets (e.g. stored credentials)
// under a 32-byte key with XChaCha20-Poly1305.
func SealWithKey(key, plain, aad []byte) ([]byte, error) { return seal(key, plain, aad) }
func OpenWithKey(key, ct, aad []byte) ([]byte, error)    { return open(key, ct, aad) }

// HashPassword returns an encoded argon2id hash for user logins.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(h)), nil
}

func CheckPassword(encoded, password string) bool {
	var m, t uint32
	var p uint8
	var saltHex, hashHex string
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	saltHex, hashHex = parts[4], parts[5]
	salt, err1 := hex.DecodeString(saltHex)
	want, err2 := hex.DecodeString(hashHex)
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// RandomToken returns a URL-safe random token with the given prefix.
func RandomToken(prefix string) string {
	b := make([]byte, 24)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// TokenHash is how bearer tokens are stored (never in plaintext).
func TokenHash(token string) string {
	h := Hash([]byte("backupproof/token"), []byte(token))
	return h.String()
}
