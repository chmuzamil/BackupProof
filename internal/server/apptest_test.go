package server

import (
	"testing"

	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestAppOnlyFailure(t *testing.T) {
	ok := proof.Check{Name: "restore-from-storage", Passed: true}
	for _, c := range []struct {
		name   string
		checks []proof.Check
		want   bool
	}{
		{"app did not answer", []proof.Check{ok, {Name: "app-sandbox-start", Passed: true}, {Name: "app-http-status"}}, true},
		{"restore failed", []proof.Check{{Name: "restore-from-storage"}}, false},
		{"restore and app failed", []proof.Check{{Name: "content-root"}, {Name: "app-running"}}, false},
		{"all passed", []proof.Check{ok, {Name: "app-http-status", Passed: true}}, false},
		{"no checks", nil, false},
	} {
		if got := appOnlyFailure(c.checks); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOperatorCannotChangeAppTest(t *testing.T) {
	at := &source.AppTest{Port: 80, App: source.AppContainer{Image: "nginx:1.27"}}
	base := func() source.Spec { return source.Spec{Name: "x", Kind: "files", Paths: []string{"/a"}} }
	old := &Source{Spec: base()}
	old.Spec.Drill.AppTest = at

	// Saving what an administrator already chose is fine.
	same := &Source{Spec: base()}
	same.Spec.Drill.AppTest = &source.AppTest{Port: 80, App: source.AppContainer{Image: "nginx:1.27"}}
	if err := operatorMayChange(old, same); err != nil {
		t.Error(err)
	}
	// A new image, and turning it on or off, are not.
	changed := &Source{Spec: base()}
	changed.Spec.Drill.AppTest = &source.AppTest{Port: 80, App: source.AppContainer{Image: "evil:1"}}
	if operatorMayChange(old, changed) == nil {
		t.Error("an operator changed the image")
	}
	if operatorMayChange(old, &Source{Spec: base()}) == nil {
		t.Error("an operator turned the app test off")
	}
	if operatorMayChange(nil, changed) == nil {
		t.Error("an operator set up an app test on a new item")
	}
	if err := operatorMayChange(&Source{Spec: base()}, &Source{Spec: base()}); err != nil {
		t.Errorf("items without an app test are unaffected: %v", err)
	}
}