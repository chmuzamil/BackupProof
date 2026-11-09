package server

import (
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/discover"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestAppTestFailed(t *testing.T) {
	ok := proof.Check{Name: "restore-from-storage", Passed: true}
	for _, c := range []struct {
		name   string
		checks []proof.Check
		want   bool
	}{
		{"app did not answer", []proof.Check{ok, {Name: "app-sandbox-start", Passed: true}, {Name: "app-http-status"}}, true},
		{"restore failed", []proof.Check{{Name: "restore-from-storage"}}, false},
		{"all passed", []proof.Check{ok, {Name: "app-http-status", Passed: true}}, false},
		{"no checks", nil, false},
	} {
		if got := appTestFailed(c.checks); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A failed app test raises its own alert and leaves the backup restore-tested.
func TestAppTestFailureIsSeparate(t *testing.T) {
	f := newPlanFixture(t)
	failed := []proof.Check{{Name: "content-root", Passed: true}, {Name: "app-http-status", Detail: "HTTP 500"}}
	f.addProof(t, f.source, "drill", "snap1", true, time.Now(), failed)
	f.srv.appTestOutcome(&f.source, failed)
	if !f.open(t, "app-test-failed") || f.open(t, "drill-failed") {
		t.Fatal("a failed app test should raise only its own alert")
	}
	st := f.srv.sourceStatus(f.source, nil, nil)
	if st.Status != "proven" || st.LastPassingDrill == nil || !strings.Contains(st.Reason, "app didn't come back up") {
		t.Errorf("status %s, reason %q, last passing %+v", st.Status, st.Reason, st.LastPassingDrill)
	}
	f.srv.appTestOutcome(&f.source, []proof.Check{{Name: "content-root", Passed: true}, {Name: "app-http-status", Passed: true}})
	if f.open(t, "app-test-failed") {
		t.Error("a passing app test left the alert open")
	}
}

// A template's values get the same validation as anything typed by hand.
func TestTemplatesPassItemValidation(t *testing.T) {
	for _, tpl := range discover.Templates() {
		at := tpl.Test
		if at.App.Image == "" {
			at.App.Image = "example/app:1"
		}
		at.App.Mounts[0].From = "docker/volumes/data"
		if at.Database != nil {
			at.Database.Mounts[0].From = "docker/volumes/db"
		}
		kind := tpl.Kinds[0]
		spec := source.Spec{Name: "x", Kind: kind, Paths: []string{"/a"}, Docker: &source.DockerSpec{Volumes: []string{"data"}}}
		spec.Drill.AppTest = &at
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", tpl.Name, err)
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
