package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chmuzamil/backupproof/internal/proof"
)

// An evidence pack from anyone is checked without panicking, and a changed
// pack never passes with the keys that signed the original.
func FuzzVerifyPack(f *testing.F) {
	dir := filepath.Join("testdata", "compat", "v0.2.23")
	pack, err := os.ReadFile(filepath.Join(dir, "evidence.json"))
	if err != nil {
		f.Fatal(err)
	}
	rawKeys, err := os.ReadFile(filepath.Join(dir, "keys.json"))
	if err != nil {
		f.Fatal(err)
	}
	var keyFile struct {
		BPKeys string `json:"bpkeys"`
	}
	if err := json.Unmarshal(rawKeys, &keyFile); err != nil {
		f.Fatal(err)
	}
	var keys []proof.PublicKey
	for _, line := range strings.Split(strings.TrimSpace(keyFile.BPKeys), "\n") {
		k, err := proof.ParsePublicKey(line)
		if err != nil {
			f.Fatal(err)
		}
		keys = append(keys, k)
	}
	f.Add(pack)
	f.Add([]byte("{}"))
	f.Add([]byte(`{"proofs":[{}],"ledger":[{}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var p EvidencePack
		if json.Unmarshal(data, &p) != nil {
			return
		}
		if len(VerifyPack(&p, keys)) > 0 {
			return
		}
		// A pack that passes has a valid signature on every proof it lists.
		for i, pr := range p.Proofs {
			if pr.Envelope == nil {
				t.Fatalf("proof %d has no envelope but the pack passed", i)
			}
			if _, err := pr.Envelope.Verify(keys); err != nil {
				t.Fatalf("proof %d's signature fails but the pack passed: %v", i, err)
			}
		}
	})
}
