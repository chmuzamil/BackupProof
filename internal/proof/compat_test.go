package proof

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every proof made by an earlier release must still verify. The fixtures in
// testdata/compat/<tag>/ were made by that release's own command-line tool
// (see docs/COMPATIBILITY.md); key.txt holds the public key that signed them.
func TestProofsFromEarlierReleasesStillVerify(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "compat", "v*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Skip("no compatibility fixtures yet")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "key.txt"))
			if err != nil {
				t.Fatal(err)
			}
			key, err := ParsePublicKey(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			bundles, _ := filepath.Glob(filepath.Join(dir, "*.bundle.json"))
			if len(bundles) == 0 {
				t.Fatal("no proofs in this release's fixtures")
			}
			for _, f := range bundles {
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				b, err := ParseBundle(data)
				if err != nil {
					t.Fatalf("%s: %v", filepath.Base(f), err)
				}
				if rep := VerifyBundle(b, VerifyOptions{TrustedKeys: []PublicKey{key}}); !rep.Valid {
					t.Errorf("%s no longer verifies: %+v", filepath.Base(f), rep)
				}
			}
		})
	}
}
