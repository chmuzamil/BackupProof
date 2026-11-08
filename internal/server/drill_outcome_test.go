package server

import (
	"testing"

	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
)

// Only a restore test that failed because storage wouldn't hand data over
// gets the "nothing was found damaged" alert and an early retry.
func TestStorageOnlyFailure(t *testing.T) {
	unreadable := proof.Check{Name: "restore-from-storage", Detail: "0 files: chunk abc " + engine.ErrUnreadable.Error() + ": HTTP 503"}
	damaged := proof.Check{Name: "restore-from-storage", Detail: "chunk abc " + engine.ErrCorrupt.Error()}
	for _, c := range []struct {
		name   string
		checks []proof.Check
		want   bool
	}{
		{"storage refused", []proof.Check{unreadable}, true},
		{"damaged chunk", []proof.Check{damaged}, false},
		{"restore fine, verify failed", []proof.Check{{Name: "restore-from-storage", Passed: true}, {Name: "custom-verify-command"}}, false},
		{"storage refused and another check failed", []proof.Check{unreadable, {Name: "content-root"}}, false},
		{"all passed", []proof.Check{{Name: "restore-from-storage", Passed: true}}, false},
	} {
		if got := storageOnlyFailure(c.checks); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
