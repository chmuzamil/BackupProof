package importer

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
	"golang.org/x/crypto/pbkdf2"
)

// Decryption of backups that other tools or scripts encrypted before
// uploading them. Supported, detected from the file content:
//
//	gpg      OpenPGP (gpg -c, gpg -e, duplicity volumes): passphrase or private key
//	openssl  `openssl enc -aes-256-cbc [-pbkdf2]` ("Salted__" header)
//	age      age / rage, passphrase or AGE-SECRET-KEY identity
//
// Server-side encryption (SSE-S3, SSE-KMS, B2 SSE) needs nothing: the
// storage decrypts for anyone holding the access keys.

type Decryptor struct {
	Mode        string // auto | none | gpg | openssl | age
	Password    string
	PrivateKey  string // armored OpenPGP private key or AGE-SECRET-KEY lines
	OpenSSLIter int    // PBKDF2 iterations if not the openssl default (10000)
}

// Detect names the encryption of a file from its first bytes.
func Detect(head []byte, name string) string {
	switch {
	case bytes.HasPrefix(head, []byte("Salted__")):
		return "openssl"
	case bytes.HasPrefix(head, []byte("age-encryption.org/v1")), bytes.HasPrefix(head, []byte("-----BEGIN AGE ENCRYPTED FILE")):
		return "age"
	case bytes.HasPrefix(head, []byte("-----BEGIN PGP MESSAGE")):
		return "gpg"
	case len(head) > 0 && head[0]&0x80 != 0:
		tag := head[0] & 0x3f
		if head[0]&0x40 == 0 {
			tag = (head[0] >> 2) & 0x0f
		}
		// Public-key (1) or symmetric (3) session key packet.
		if tag == 1 || tag == 3 {
			return "gpg"
		}
	}
	return "none"
}

// PlainName strips encryption suffixes ("db.sql.gz.gpg" -> "db.sql.gz").
func PlainName(name string) string {
	low := strings.ToLower(name)
	for _, ext := range []string{".gpg", ".pgp", ".asc", ".age", ".enc", ".aes"} {
		if strings.HasSuffix(low, ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

// Open returns the decrypted content of the file at path and the detected
// encryption. Integrity is verified by the format itself (GPG MDC/AEAD, age
// MAC); OpenSSL CBC has no MAC, so the PKCS#7 padding is checked and the
// result should be validated by a restore test.
func (d *Decryptor) Open(path, name string) (io.ReadCloser, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, "", err
	}
	mode := d.Mode
	if mode == "" || mode == "auto" {
		mode = Detect(head, name)
	}
	var r io.Reader
	switch mode {
	case "none":
		return f, "none", nil
	case "gpg":
		r, err = d.gpg(f)
	case "age":
		r, err = d.age(f, head)
	case "openssl":
		r, err = d.openssl(f)
	default:
		err = fmt.Errorf("unknown decryption %q", mode)
	}
	if err != nil {
		f.Close()
		return nil, mode, err
	}
	return struct {
		io.Reader
		io.Closer
	}{r, f}, mode, nil
}

var errNeedSecret = errors.New("this backup is encrypted: enter its password or private key")

func (d *Decryptor) gpg(f io.Reader) (io.Reader, error) {
	var keyring openpgp.EntityList
	if strings.Contains(d.PrivateKey, "BEGIN PGP PRIVATE KEY") {
		kr, err := openpgp.ReadArmoredKeyRing(strings.NewReader(d.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("private key: %w", err)
		}
		keyring = kr
	}
	tries := 0
	prompt := func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		tries++
		if tries > 1 {
			return nil, errors.New("wrong password")
		}
		if symmetric {
			if d.Password == "" {
				return nil, errNeedSecret
			}
			return []byte(d.Password), nil
		}
		for _, k := range keys {
			if k.PrivateKey != nil && k.PrivateKey.Encrypted {
				if err := k.PrivateKey.Decrypt([]byte(d.Password)); err != nil {
					// Leave this key locked: if none unlocks, openpgp asks
					// again and the next prompt reports "wrong password".
					continue
				}
			}
		}
		return nil, nil
	}
	in := bufio.NewReader(f)
	var body io.Reader = in
	if peek, _ := in.Peek(30); bytes.HasPrefix(peek, []byte("-----BEGIN PGP")) {
		blk, err := pgparmor.Decode(in)
		if err != nil {
			return nil, fmt.Errorf("armored GPG: %w", err)
		}
		body = blk.Body
	}
	md, err := openpgp.ReadMessage(body, keyring, prompt, nil)
	if err != nil {
		if keyring == nil && d.Password == "" {
			return nil, errNeedSecret
		}
		return nil, fmt.Errorf("cannot decrypt (wrong password or key): %w", err)
	}
	return md.UnverifiedBody, nil
}

func (d *Decryptor) age(f io.Reader, head []byte) (io.Reader, error) {
	var ids []age.Identity
	if strings.Contains(d.PrivateKey, "AGE-SECRET-KEY") {
		parsed, err := age.ParseIdentities(strings.NewReader(d.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("age identity: %w", err)
		}
		ids = append(ids, parsed...)
	}
	if d.Password != "" {
		id, err := age.NewScryptIdentity(d.Password)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errNeedSecret
	}
	src := f
	if bytes.HasPrefix(head, []byte("-----BEGIN AGE")) {
		src = armor.NewReader(f)
	}
	r, err := age.Decrypt(src, ids...)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt (wrong password or key): %w", err)
	}
	return r, nil
}

// --- openssl enc -------------------------------------------------------------

type opensslKDF struct {
	name string
	kdf  func(pass, salt []byte) (key, iv []byte)
}

func evpBytesToKey(h func() hash.Hash) func(pass, salt []byte) ([]byte, []byte) {
	return func(pass, salt []byte) ([]byte, []byte) {
		var out, prev []byte
		for len(out) < 48 {
			hh := h()
			hh.Write(prev)
			hh.Write(pass)
			hh.Write(salt)
			prev = hh.Sum(nil)
			out = append(out, prev...)
		}
		return out[:32], out[32:48]
	}
}

func (d *Decryptor) opensslKDFs() []opensslKDF {
	iter := d.OpenSSLIter
	if iter == 0 {
		iter = 10000
	}
	pb := func(pass, salt []byte) ([]byte, []byte) {
		k := pbkdf2.Key(pass, salt, iter, 48, sha256.New)
		return k[:32], k[32:]
	}
	return []opensslKDF{
		{fmt.Sprintf("pbkdf2-sha256-%d", iter), pb},
		{"evp-sha256", evpBytesToKey(sha256.New)},
		{"evp-md5", evpBytesToKey(md5.New)},
	}
}

func validPadding(block []byte) bool {
	if len(block) != aes.BlockSize {
		return false
	}
	p := int(block[len(block)-1])
	if p < 1 || p > aes.BlockSize {
		return false
	}
	for _, b := range block[len(block)-p:] {
		if int(b) != p {
			return false
		}
	}
	return true
}

// openssl decrypts AES-256-CBC "Salted__" files. The key derivation used by
// the original command isn't stored, so each candidate is tried against the
// last block (CBC allows decrypting it alone) and the first whose PKCS#7
// padding is valid wins.
func (d *Decryptor) openssl(f *os.File) (io.Reader, error) {
	if d.Password == "" {
		return nil, errNeedSecret
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	ctLen := size - 16
	if ctLen <= 0 || ctLen%aes.BlockSize != 0 {
		return nil, errors.New("not a valid openssl aes-256-cbc file (wrong length)")
	}
	hdr := make([]byte, 16)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return nil, err
	}
	salt := hdr[8:16]
	tail := make([]byte, 32)
	off := size - 32
	if off < 16 {
		off = 16
		tail = tail[16:]
	}
	if _, err := f.ReadAt(tail, off); err != nil {
		return nil, err
	}
	for _, c := range d.opensslKDFs() {
		key, iv := c.kdf([]byte(d.Password), salt)
		prev := iv
		last := tail
		if len(tail) == 32 {
			prev, last = tail[:16], tail[16:]
		}
		block, _ := aes.NewCipher(key)
		out := make([]byte, 16)
		block.Decrypt(out, last)
		for i := range out {
			out[i] ^= prev[i]
		}
		if !validPadding(out) {
			continue
		}
		if _, err := f.Seek(16, io.SeekStart); err != nil {
			return nil, err
		}
		return &cbcReader{src: bufio.NewReaderSize(f, 1<<20), mode: cipher.NewCBCDecrypter(block, iv), remaining: ctLen}, nil
	}
	return nil, errors.New("cannot decrypt: wrong password, or it was encrypted with a cipher other than aes-256-cbc")
}

// cbcReader streams CBC decryption and strips PKCS#7 padding at the end.
type cbcReader struct {
	src       io.Reader
	mode      cipher.BlockMode
	remaining int64
	buf       []byte
	held      []byte // last decrypted block, released once more data follows
	done      bool
}

func (c *cbcReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.done {
			return 0, io.EOF
		}
		n := int64(64 << 10)
		if n > c.remaining {
			n = c.remaining
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(c.src, chunk); err != nil {
			return 0, err
		}
		c.remaining -= n
		c.mode.CryptBlocks(chunk, chunk)
		all := append(c.held, chunk...)
		if c.remaining == 0 {
			if !validPadding(all[len(all)-16:]) {
				return 0, errors.New("decryption produced invalid padding (damaged file)")
			}
			c.buf = all[:len(all)-int(all[len(all)-1])]
			c.done = true
			break
		}
		c.held = append([]byte{}, all[len(all)-16:]...)
		c.buf = all[:len(all)-16]
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
