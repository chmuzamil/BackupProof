package snapshot

import (
	"fmt"
	"testing"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
)

func TestInclusionProofsAllSizes(t *testing.T) {
	for n := 1; n <= 33; n++ {
		leaves := make([]bpcrypto.ID, n)
		for i := range leaves {
			leaves[i] = LeafHash(&Entry{Path: fmt.Sprintf("f%03d", i), Type: TypeFile, Size: int64(i)})
		}
		root := RootOfLeaves(leaves)
		for i := 0; i < n; i++ {
			proof, err := InclusionProof(leaves, i)
			if err != nil {
				t.Fatal(err)
			}
			if !VerifyInclusion(leaves[i], i, n, proof, root) {
				t.Fatalf("n=%d i=%d: valid proof rejected", n, i)
			}
			if n > 1 && VerifyInclusion(leaves[(i+1)%n], i, n, proof, root) {
				t.Fatalf("n=%d i=%d: wrong leaf accepted", n, i)
			}
		}
	}
}

func TestSafeRelPath(t *testing.T) {
	for _, p := range []string{"../x", "a/../../b", "/etc/passwd", "C:/x", "a\\b", ""} {
		if SafeRelPath(p) {
			t.Fatalf("%q must be unsafe", p)
		}
	}
	if !SafeRelPath("var/www/index.php") {
		t.Fatal("normal path rejected")
	}
	if CleanPath(`C:\Users\x\..\y`) != "C/Users/y" {
		t.Fatalf("CleanPath = %q", CleanPath(`C:\Users\x\..\y`))
	}
}
