package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPutBackRefusesMoreStartupPlaces(t *testing.T) {
	for _, p := range []string{"/etc/logrotate.d/x", "/etc/rc.local", "/etc/environment", "/etc/apt/apt.conf.d/x", "/home/me/.bash_logout", "/home/me/.config/systemd/user/x.service", "/home/me/.config/autostart/x.desktop"} {
		if PutBackAllowed(p) {
			t.Errorf("%s was allowed", p)
		}
	}
	if !PutBackAllowed("/srv/app/.config/settings.json") {
		t.Error("an ordinary app folder was refused")
	}
}

func TestSwapInReplacesEverything(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"old.txt", "keep-name.txt", "sub/old.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := filepath.Join(dir, ".bp-restore-1")
	if err := os.MkdirAll(filepath.Join(staging, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"keep-name.txt", "sub/new.txt"} {
		if err := os.WriteFile(filepath.Join(staging, filepath.FromSlash(p)), []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := swapIn(dir, staging); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "keep-name.txt")); err != nil || string(b) != "new" {
		t.Errorf("keep-name.txt: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "new.txt")); err != nil {
		t.Error(err)
	}
	for _, gone := range []string{"old.txt", "sub/old.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}
}
