package transparency

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/sumdb/tlog"
)

func newSigner(name string) Signer {
	_, priv, _ := ed25519.GenerateKey(nil)
	return Signer{Name: name, Key: priv}
}

func pub(s Signer) ed25519.PublicKey { return s.Key.Public().(ed25519.PublicKey) }

func memLog(t *testing.T, n int) *MemoryLog {
	t.Helper()
	m := &MemoryLog{}
	for i := range n {
		if err := m.Append(LeafData(fmt.Sprintf("%064x", i))); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func signed(t *testing.T, m *MemoryLog, log Signer) (Checkpoint, string) {
	t.Helper()
	c, err := m.Checkpoint(log.Name)
	if err != nil {
		t.Fatal(err)
	}
	return c, Note{Text: c.Body(), Sigs: []string{log.Sign(c.Body())}}.String()
}

func TestVerifierKeys(t *testing.T) {
	s := newSigner("backup.example.com/log")
	vk := LogVerifierKey(s.Name, pub(s))
	v, err := ParseVerifier(vk)
	if err != nil || v.String() != vk || v.Type != sigEd25519 {
		t.Fatalf("round trip: %v %v", v, err)
	}
	// The example from c2sp.org/signed-note parses and its key ID checks out.
	if _, err := ParseVerifier("example.com/foo+530d903a+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k"); err != nil {
		t.Errorf("the spec's example key: %v", err)
	}
	for _, bad := range []string{"", "x", "a b+12345678+AA==", strings.Replace(vk, "+", "+0", 1), vk[:len(vk)-4] + "AAAA"} {
		if _, err := ParseVerifier(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCheckpointNotes(t *testing.T) {
	log, w1, w2 := newSigner("backup.example.com/log"), newSigner("witness-1.example"), newSigner("witness-2.example")
	m := memLog(t, 1000)
	c, note := signed(t, m, log)
	n, _ := ParseNote(note)
	n.Sigs = append(n.Sigs, w1.Cosign(n.Text, time.Unix(1700000000, 0)), w2.Cosign(n.Text, time.Unix(1700000005, 0)))
	full := n.String()

	lv, _ := ParseVerifier(LogVerifierKey(log.Name, pub(log)))
	wv1, _ := ParseVerifier(WitnessVerifierKey(w1.Name, pub(w1)))
	wv2, _ := ParseVerifier(WitnessVerifierKey(w2.Name, pub(w2)))
	got, times, err := VerifyNote(full, lv, []Verifier{wv1, wv2})
	if err != nil || got != c || times[w1.Name].Unix() != 1700000000 {
		t.Fatalf("verify: %+v %v %v", got, times, err)
	}
	// A changed size or root breaks every signature.
	for _, tamper := range []string{strings.Replace(full, "\n1000\n", "\n999\n", 1)} {
		if _, _, err := VerifyNote(tamper, lv, nil); err == nil {
			t.Error("a changed checkpoint verified")
		}
	}
	// A witness that didn't cosign, or another key with the log's name, fails.
	if _, _, err := VerifyNote(note, lv, []Verifier{wv1}); err == nil {
		t.Error("passed without the required cosignature")
	}
	imposter := newSigner(log.Name)
	iv, _ := ParseVerifier(LogVerifierKey(imposter.Name, pub(imposter)))
	if _, _, err := VerifyNote(full, iv, nil); err == nil {
		t.Error("verified with a different key of the same name")
	}
}

func TestInclusionAndConsistencyFromTiles(t *testing.T) {
	log := newSigner("backup.example.com/log")
	for _, size := range []int{1, 7, 256, 257, 70000} {
		m := memLog(t, size)
		c, _ := signed(t, m, log)
		fetch := func(_ context.Context, p string) ([]byte, error) { return m.Tile(p) }
		for _, i := range []int64{0, int64(size) / 2, int64(size) - 1} {
			if err := VerifyInclusion(context.Background(), fetch, c, i, LeafData(fmt.Sprintf("%064x", i))); err != nil {
				t.Fatalf("size %d, entry %d: %v", size, i, err)
			}
		}
		if err := VerifyInclusion(context.Background(), fetch, c, 0, LeafData("not in the log")); err == nil {
			t.Errorf("size %d: proved an entry that isn't there", size)
		}
		if size > 1 {
			older := memLog(t, size/2)
			oc, _ := signed(t, older, log)
			if err := VerifyConsistency(context.Background(), fetch, oc, c); err != nil {
				t.Errorf("size %d from %d: %v", size, size/2, err)
			}
		}
	}
}

// A tile request beyond the log, or a tile the log doesn't serve correctly,
// fails verification rather than misleading it.
func TestTilesCheckedAgainstRoot(t *testing.T) {
	m := memLog(t, 600)
	c, _ := signed(t, m, newSigner("l"))
	if _, err := m.Tile("tile/0/003"); err == nil {
		t.Error("served a tile beyond the log")
	}
	bad := func(_ context.Context, p string) ([]byte, error) {
		b, err := m.Tile(p)
		if err == nil && len(b) > 0 {
			b = append([]byte(nil), b...)
			b[0] ^= 1
		}
		return b, err
	}
	if err := VerifyInclusion(context.Background(), bad, c, 5, LeafData(fmt.Sprintf("%064x", 5))); err == nil {
		t.Error("a damaged tile wasn't noticed")
	}
}

func TestWitness(t *testing.T) {
	ctx := context.Background()
	log, wsig := newSigner("backup.example.com/log"), newSigner("witness.example")
	lv, _ := ParseVerifier(LogVerifierKey(log.Name, pub(log)))
	state := filepath.Join(t.TempDir(), "witness.json")
	w := &Witness{Signer: wsig, Logs: map[string]Verifier{log.Name: lv}, State: state}
	srv := httptest.NewServer(w)
	defer srv.Close()
	wv, err := ParseVerifier(WitnessVerifierKey(wsig.Name, pub(wsig)))
	if err != nil {
		t.Fatal(err)
	}

	m := memLog(t, 10)
	_, note := signed(t, m, log)
	sigs, err := AddCheckpoint(ctx, srv.Client(), srv.URL, 0, nil, note)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := ParseNote(note)
	n.Sigs = append(n.Sigs, sigs...)
	if _, _, err := VerifyNote(n.String(), lv, []Verifier{wv}); err != nil {
		t.Fatalf("the witness's cosignature: %v", err)
	}

	// Growing the log: needs the consistency proof from size 10.
	for i := range 90 {
		m.Append(LeafData(fmt.Sprintf("more %d", i)))
	}
	_, note2 := signed(t, m, log)
	if _, err := AddCheckpoint(ctx, srv.Client(), srv.URL, 0, nil, note2); err == nil {
		t.Fatal("cosigned without knowing where the witness was")
	} else if c, ok := err.(ErrConflict); !ok || c.Size != 10 {
		t.Fatalf("want a conflict at 10, got %v", err)
	}
	proof, _ := m.ConsistencyProof(10)
	if _, err := AddCheckpoint(ctx, srv.Client(), srv.URL, 10, proof, note2); err != nil {
		t.Fatalf("a proper extension: %v", err)
	}

	// A fork: a different history at a larger size can't prove it extends.
	fork := memLog(t, 5)
	for i := range 200 {
		fork.Append(LeafData(fmt.Sprintf("rewritten %d", i)))
	}
	_, forkNote := signed(t, fork, log)
	fp, _ := fork.ConsistencyProof(100)
	if _, err := AddCheckpoint(ctx, srv.Client(), srv.URL, 100, fp, forkNote); err == nil {
		t.Fatal("the witness cosigned a rewritten history")
	}
	// Same size, different root.
	same := memLog(t, 100)
	_, sameNote := signed(t, same, log)
	if _, err := AddCheckpoint(ctx, srv.Client(), srv.URL, 100, nil, sameNote); err == nil {
		t.Fatal("the witness cosigned a second root at the same size")
	}
	// Signed by someone else.
	_, otherNote := signed(t, m, newSigner(log.Name))
	if _, err := AddCheckpoint(ctx, srv.Client(), srv.URL, 100, nil, otherNote); err == nil {
		t.Fatal("the witness cosigned a checkpoint the log didn't sign")
	}

	// It remembers across restarts, and shows its latest cosigned checkpoint.
	w2 := &Witness{Signer: wsig, Logs: w.Logs, State: state}
	srv2 := httptest.NewServer(w2)
	defer srv2.Close()
	if _, err := AddCheckpoint(ctx, srv2.Client(), srv2.URL, 0, nil, note2); err == nil {
		t.Error("a restarted witness forgot where it was")
	}
	resp, err := http.Get(srv2.URL + "/" + OriginHash(log.Name) + "/checkpoint")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("monitoring: %v %v", resp, err)
	}
	resp.Body.Close()
}

func TestAddCheckpointBody(t *testing.T) {
	var h tlog.Hash
	h[0] = 7
	body := FormatAddCheckpoint(5, tlog.TreeProof{h}, "origin\n6\nroot\n\n— x y\n")
	old, proof, note, err := ParseAddCheckpoint(body)
	if err != nil || old != 5 || len(proof) != 1 || proof[0] != h || note != "origin\n6\nroot\n\n— x y\n" {
		t.Fatalf("%d %v %q %v", old, proof, note, err)
	}
	for _, bad := range []string{"", "old 01\n\nx", "old -1\n\nx", "old 1\nnot-base64\n\nx", "old 1\n"} {
		if _, _, _, err := ParseAddCheckpoint(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	a, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(p)
	if err != nil || string(a) != string(b) || len(a) != 32 {
		t.Fatalf("reloaded key differs: %v", err)
	}
}
