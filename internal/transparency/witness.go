package transparency

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/sumdb/tlog"
)

// Witness cosigns checkpoints of the logs it trusts, but only checkpoints
// that extend the last one it cosigned for that log, checked with a
// consistency proof (c2sp.org/tlog-witness). It keeps that last checkpoint
// in a file, saved before it answers.
type Witness struct {
	Signer Signer
	Logs   map[string]Verifier // origin → the log's key
	State  string              // file holding the last cosigned checkpoint per log
	Now    func() time.Time

	mu    sync.Mutex
	state map[string]witnessed
}

type witnessed struct {
	Size int64  `json:"size"`
	Root string `json:"root"` // base64
	Note string `json:"note"` // the checkpoint with this witness's cosignature
}

const maxWitnessBody = 64 << 10

func (w *Witness) load() error {
	if w.state != nil {
		return nil
	}
	w.state = map[string]witnessed{}
	raw, err := os.ReadFile(w.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &w.state)
}

func (w *Witness) save() error {
	raw, err := json.MarshalIndent(w.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := w.State + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.State)
}

// emptyRoot is the RFC 6962 root of an empty tree: SHA-256 of nothing.
var emptyRoot = tlog.Hash(sha256.Sum256(nil))

func (w *Witness) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/add-checkpoint":
		w.addCheckpoint(rw, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/checkpoint"):
		w.mu.Lock()
		defer w.mu.Unlock()
		if err := w.load(); err != nil {
			http.Error(rw, "state unreadable", http.StatusInternalServerError)
			return
		}
		hash := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/checkpoint")
		for origin, st := range w.state {
			if OriginHash(origin) == hash && st.Note != "" {
				rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = io.WriteString(rw, st.Note)
				return
			}
		}
		http.NotFound(rw, r)
	default:
		http.NotFound(rw, r)
	}
}

func (w *Witness) addCheckpoint(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWitnessBody+1))
	if err != nil || len(body) > maxWitnessBody {
		http.Error(rw, "request too large", http.StatusBadRequest)
		return
	}
	old, proof, raw, err := ParseAddCheckpoint(string(body))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	n, err := ParseNote(raw)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := ParseCheckpoint(n.Text)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	log, known := w.Logs[c.Origin]
	if !known {
		http.Error(rw, "unknown log", http.StatusNotFound)
		return
	}
	signed := false
	for _, line := range n.Sigs {
		if ok, _, err := log.Verify(n.Text, line); ok {
			if err != nil {
				http.Error(rw, err.Error(), http.StatusForbidden)
				return
			}
			signed = true
		}
	}
	if !signed {
		http.Error(rw, "not signed by the log's key", http.StatusForbidden)
		return
	}
	if old > c.Size {
		http.Error(rw, "old size is larger than the checkpoint", http.StatusBadRequest)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.load(); err != nil {
		http.Error(rw, "state unreadable", http.StatusInternalServerError)
		return
	}
	last := w.state[c.Origin]
	if old != last.Size {
		rw.Header().Set("Content-Type", "text/x.tlog.size")
		rw.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(rw, "%d\n", last.Size)
		return
	}
	root := base64.StdEncoding.EncodeToString(c.Root[:])
	switch {
	case c.Size == 0 && c.Root != emptyRoot:
		http.Error(rw, "an empty tree has the empty root", http.StatusUnprocessableEntity)
		return
	case old == 0 && len(proof) > 0:
		http.Error(rw, "no consistency proof from an empty tree", http.StatusUnprocessableEntity)
		return
	case old == c.Size && last.Root != "" && root != last.Root:
		http.Error(rw, "same size, different root: the log forked", http.StatusUnprocessableEntity)
		return
	case old > 0 && old < c.Size:
		var oldRoot tlog.Hash
		b, _ := base64.StdEncoding.DecodeString(last.Root)
		copy(oldRoot[:], b)
		if err := tlog.CheckTree(proof, c.Size, c.Root, old, oldRoot); err != nil {
			http.Error(rw, "the checkpoint doesn't extend the last one: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	cosig := w.Signer.Cosign(n.Text, now())
	n.Sigs = append(n.Sigs, cosig)
	w.state[c.Origin] = witnessed{Size: c.Size, Root: root, Note: n.String()}
	if err := w.save(); err != nil {
		http.Error(rw, "couldn't save state", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(rw, dash+cosig+"\n")
}

// FormatAddCheckpoint builds an add-checkpoint request body.
func FormatAddCheckpoint(old int64, proof tlog.TreeProof, note string) string {
	var b strings.Builder
	b.WriteString("old " + strconv.FormatInt(old, 10) + "\n")
	for _, h := range proof {
		b.WriteString(base64.StdEncoding.EncodeToString(h[:]) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(note)
	return b.String()
}

// ParseAddCheckpoint reads an add-checkpoint request body.
func ParseAddCheckpoint(body string) (old int64, proof tlog.TreeProof, note string, err error) {
	sc := bufio.NewScanner(strings.NewReader(body))
	if !sc.Scan() {
		return 0, nil, "", errors.New("empty request")
	}
	v, ok := strings.CutPrefix(sc.Text(), "old ")
	n, perr := strconv.ParseInt(v, 10, 64)
	if !ok || perr != nil || n < 0 || strconv.FormatInt(n, 10) != v {
		return 0, nil, "", errors.New(`the first line is "old <size>"`)
	}
	consumed := len(sc.Text()) + 1
	for sc.Scan() {
		line := sc.Text()
		consumed += len(line) + 1
		if line == "" {
			return n, proof, body[consumed:], nil
		}
		b, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(b) != tlog.HashSize || len(proof) >= 63 {
			return 0, nil, "", errors.New("bad consistency proof line")
		}
		var h tlog.Hash
		copy(h[:], b)
		proof = append(proof, h)
	}
	return 0, nil, "", errors.New("no blank line before the checkpoint")
}

// ErrConflict says the witness last saw another size; AddCheckpoint returns it
// with that size so the caller can send a proof from there.
type ErrConflict struct{ Size int64 }

func (e ErrConflict) Error() string { return fmt.Sprintf("the witness is at size %d", e.Size) }

// AddCheckpoint asks a witness to cosign a checkpoint. url is the witness's
// submission prefix. It returns the witness's signature lines (without "— ").
func AddCheckpoint(ctx context.Context, client *http.Client, url string, old int64, proof tlog.TreeProof, note string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(url, "/")+"/add-checkpoint",
		bytes.NewReader([]byte(FormatAddCheckpoint(old, proof, note))))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusConflict:
		n, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("the witness answered 409 without a size")
		}
		return nil, ErrConflict{Size: n}
	default:
		return nil, fmt.Errorf("the witness answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var sigs []string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if s, ok := strings.CutPrefix(line, dash); ok {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return nil, errors.New("the witness answered without a cosignature")
	}
	return sigs, nil
}

// SaveKey and LoadKey keep an Ed25519 key as a hex seed in a file.
func LoadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		return decodeSeed(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seed := make([]byte, 32)
	if _, err := randRead(seed); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%x\n", seed)), 0o600); err != nil {
		return nil, err
	}
	return seed, nil
}

func decodeSeed(b []byte) ([]byte, error) {
	s := strings.TrimSpace(string(b))
	seed := make([]byte, 32)
	if n, err := fmt.Sscanf(s, "%x", &seed); err != nil || n != 1 || len(seed) != 32 {
		return nil, errors.New("the key file is malformed")
	}
	return seed, nil
}

var randRead = cryptorand.Read
