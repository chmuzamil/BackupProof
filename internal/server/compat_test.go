package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chmuzamil/backupproof/internal/proof"
)

// Every proof a dashboard and its agents made in an earlier release must still
// verify: each bundle, its timestamp when it has one, and the evidence pack.
// The fixtures in testdata/compat/<tag>/ were made in CI by tools/compatgen
// against that release (see docs/COMPATIBILITY.md).
func TestDashboardProofsFromEarlierReleasesStillVerify(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "compat", "v*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Skip("no compatibility fixtures yet")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var keys struct {
				Server proof.PublicKey
				Agents []proof.PublicKey
			}
			readJSONFile(t, filepath.Join(dir, "keys.json"), &keys)
			trusted := append([]proof.PublicKey{keys.Server}, keys.Agents...)

			bundles, _ := filepath.Glob(filepath.Join(dir, "*.bundle.json"))
			if len(bundles) == 0 {
				t.Fatal("no proofs in this release's fixtures")
			}
			for _, f := range bundles {
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				b, err := proof.ParseBundle(data)
				if err != nil {
					t.Fatalf("%s: %v", filepath.Base(f), err)
				}
				opts := proof.VerifyOptions{TrustedKeys: trusted, RequireTimestamp: b.Timestamp != nil}
				if rep := proof.VerifyBundle(b, opts); !rep.Valid {
					t.Errorf("%s no longer verifies: %+v", filepath.Base(f), rep)
				}
			}

			var pack EvidencePack
			readJSONFile(t, filepath.Join(dir, "evidence.json"), &pack)
			if len(pack.Proofs) == 0 {
				t.Fatal("the evidence pack holds no proofs")
			}
			if errs := VerifyPack(&pack, trusted); len(errs) > 0 {
				t.Errorf("evidence pack no longer verifies: %v", errs)
			}
		})
	}
}

func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
}
