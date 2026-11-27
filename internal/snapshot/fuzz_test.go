package snapshot

import (
	"path/filepath"
	"strings"
	"testing"
)

// A path from a backup's file list that SafeRelPath accepts can never lead
// outside the folder a restore writes into.
func FuzzSafeRelPath(f *testing.F) {
	for _, p := range []string{"a/b.txt", "../etc/passwd", "a/../../b", "/abs", "C:/x", `a\..\b`, "a//b", "./a", "a/./b", "..", ".", "", "a/..b", "..a/b"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if !SafeRelPath(p) {
			return
		}
		base := filepath.FromSlash("/restore/target")
		joined := filepath.Join(base, filepath.FromSlash(p))
		rel, err := filepath.Rel(base, joined)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("SafeRelPath(%q) is true but it lands at %q", p, joined)
		}
	})
}

// CleanPath turns any backed-up path, including Windows ones, into a path
// SafeRelPath accepts, or into nothing.
func FuzzCleanPath(f *testing.F) {
	for _, p := range []string{`C:\Users\x\file.txt`, "/var/www/../etc", "a/b", "", "/", `\\server\share\f`, "C:", "../../x"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		c := CleanPath(p)
		if c == "" {
			return
		}
		if strings.Contains(c, "..") && !SafeRelPath(c) {
			for _, seg := range strings.Split(c, "/") {
				if seg == ".." {
					t.Fatalf("CleanPath(%q) = %q climbs out", p, c)
				}
			}
		}
	})
}
