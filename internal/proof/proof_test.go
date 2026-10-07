package proof

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitorus/timestamp"
)

func makeBundle(t *testing.T, k *Key, passed bool) *Bundle {
	t.Helper()
	pred := DrillPredicate{SnapshotID: "abc", ExpectedRoot: "root1", RestoredRoot: "root1", Passed: passed}
	if !passed {
		pred.RestoredRoot = "other"
	}
	payload, err := NewStatement(SnapshotSubject("repo", "abc", "root1"), PredicateDrill, pred)
	if err != nil {
		t.Fatal(err)
	}
	env := k.Sign(payload)
	l, _ := OpenFileLedger(filepath.Join(t.TempDir(), "ledger.jsonl"))
	l.Append("event", "x", "00")
	e, _ := l.Append("drill", "abc", env.Digest())
	e2, _ := l.Append("backup", "def", "11")
	cp := SignCheckpoint(k, "test", e2, time.Now())
	return &Bundle{Format: BundleFormat, Envelope: env, Ledger: []LedgerEntry{e, e2}, Checkpoint: &cp, Keys: []PublicKey{k.Public()}}
}

func TestBundleRoundTrip(t *testing.T) {
	k, _ := GenerateKey("verifier")
	b := makeBundle(t, k, true)
	rep := VerifyBundle(b, VerifyOptions{TrustedKeys: []PublicKey{k.Public()}})
	if !rep.Valid || rep.Passed == nil || !*rep.Passed {
		t.Fatalf("expected valid passing bundle: %+v", rep)
	}
}

func TestBundleRejectsUntrustedKey(t *testing.T) {
	k, _ := GenerateKey("verifier")
	other, _ := GenerateKey("attacker")
	b := makeBundle(t, other, true)
	if rep := VerifyBundle(b, VerifyOptions{TrustedKeys: []PublicKey{k.Public()}}); rep.Valid {
		t.Fatal("bundle signed by untrusted key accepted")
	}
}

func TestBundleDetectsTampering(t *testing.T) {
	k, _ := GenerateKey("verifier")
	trusted := VerifyOptions{TrustedKeys: []PublicKey{k.Public()}}

	b := makeBundle(t, k, true)
	payload, _ := b.Envelope.DecodePayload()
	payload[len(payload)-3] ^= 1
	b.Envelope.Payload = base64.StdEncoding.EncodeToString(payload)
	if VerifyBundle(b, trusted).Valid {
		t.Fatal("modified payload accepted")
	}

	b = makeBundle(t, k, true)
	b.Ledger[1].Subject = "rewritten"
	if VerifyBundle(b, trusted).Valid {
		t.Fatal("modified ledger accepted")
	}
}

func TestLedgerDetectsRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.jsonl")
	l, _ := OpenFileLedger(path)
	for i := 0; i < 5; i++ {
		l.Append("backup", "s", "d")
	}
	all, _ := l.All()
	broken := append(append([]LedgerEntry{}, all[:2]...), all[3:]...)
	if VerifyChain(broken) == nil {
		t.Fatal("removed entry not detected")
	}
	if VerifyChain(all) != nil {
		t.Fatal("valid chain rejected")
	}
}

func TestPublicKeyEncoding(t *testing.T) {
	k, _ := GenerateKey("agent-1")
	pk, err := ParsePublicKey(k.Public().String())
	if err != nil || pk.KeyID != k.Public().KeyID {
		t.Fatalf("round trip failed: %v", err)
	}
}

// TestTimestamp runs a throwaway RFC 3161 TSA to exercise request, parse and verify.
func TestTimestamp(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ekuExt, _ := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}})
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test tsa"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:        x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 37}, Critical: true, Value: ekuExt}},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	cert, _ := x509.ParseCertificate(der)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := timestamp.ParseRequest(body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		ts := timestamp.Timestamp{HashAlgorithm: req.HashAlgorithm, HashedMessage: req.HashedMessage, Time: time.Now(),
			Policy: asn1.ObjectIdentifier{1, 2, 3}, SerialNumber: big.NewInt(7), AddTSACertificate: true, Nonce: req.Nonce}
		resp, err := ts.CreateResponseWithOpts(cert, priv, crypto.SHA256)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(resp)
	}))
	defer srv.Close()

	data := []byte("envelope bytes")
	tok, err := RequestTimestamp(context.Background(), data, []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	if _, err := tok.Verify(data, roots); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := tok.Verify([]byte("other"), nil); err == nil {
		t.Fatal("timestamp accepted for different data")
	}
}

func TestPublicKeyNameWithColon(t *testing.T) {
	k, _ := GenerateKey("agent:web-01")
	pk, err := ParsePublicKey(k.Public().String())
	if err != nil || pk.Name != "agent:web-01" || pk.KeyID != k.Public().KeyID {
		t.Fatalf("got %+v, %v", pk, err)
	}
}
