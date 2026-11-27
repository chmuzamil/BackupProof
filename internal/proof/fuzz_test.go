package proof

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A proof bundle from anyone is parsed and verified without panicking, and a
// changed bundle never verifies with the key that signed the original.
func FuzzVerifyBundle(f *testing.F) {
	raw, err := os.ReadFile(filepath.Join("testdata", "compat", "v0.2.23", "key.txt"))
	if err != nil {
		f.Fatal(err)
	}
	key, err := ParsePublicKey(strings.TrimSpace(string(raw)))
	if err != nil {
		f.Fatal(err)
	}
	originals := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join("testdata", "compat", "v0.2.23", "*.bundle.json"))
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		originals[string(data)] = true
		f.Add(data)
	}
	f.Add([]byte("{}"))
	f.Add([]byte(`{"envelope":{"payload":"","signatures":[]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := ParseBundle(data)
		if err != nil {
			return
		}
		rep := VerifyBundle(b, VerifyOptions{TrustedKeys: []PublicKey{key}})
		if rep.Valid && !originals[string(data)] {
			// Formatting may differ; what's signed may not.
			if b.Envelope == nil {
				t.Fatal("a bundle without an envelope verified")
			}
			if _, err := b.Envelope.Verify([]PublicKey{key}); err != nil {
				t.Fatalf("a bundle whose signature fails verified: %v", err)
			}
		}
	})
}

func FuzzParsePublicKey(f *testing.F) {
	raw, _ := os.ReadFile(filepath.Join("testdata", "compat", "v0.2.23", "key.txt"))
	f.Add(strings.TrimSpace(string(raw)))
	f.Add("bpkey1:")
	f.Add("bpkey1:x:y")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := ParsePublicKey(s)
		if err == nil && len(k.Public) != 32 {
			t.Fatalf("accepted a %d-byte key", len(k.Public))
		}
	})
}
