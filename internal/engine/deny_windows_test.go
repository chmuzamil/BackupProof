//go:build windows

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// On Windows a folder can also be named by its 8.3 short name (as the TEMP
// folder of CI runners is: C:\Users\RUNNER~1\...). A protected folder must be
// matched whichever form is used, including for paths that don't exist yet.
func TestIsDeniedWithShortNames(t *testing.T) {
	long := filepath.Join(t.TempDir(), "protected server data")
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(windows.StringToUTF16Ptr(long), &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skip("short names unavailable on this volume")
	}
	short := windows.UTF16ToString(buf[:n])
	if short == long {
		t.Skip("8.3 short names are disabled on this volume")
	}
	for _, p := range []string{short, filepath.Join(short, "repo"), filepath.Join(short, "new", "deeper")} {
		if !IsDenied(p, []string{long}) {
			t.Errorf("%s not recognised as inside %s", p, long)
		}
		if !IsDenied(filepath.Join(long, "x"), []string{short}) {
			t.Errorf("long path not recognised as inside short-named %s", short)
		}
	}
}
