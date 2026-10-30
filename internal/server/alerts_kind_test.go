package server

import (
	"path/filepath"
	"testing"
)

// A failed storage check (or copy, or restore test) stays open until the
// same kind of job succeeds; a successful backup doesn't close it.
func TestJobAlertsCloseOnlyForTheSameKind(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	const sid = int64(7)
	open := func(kind string) bool {
		t.Helper()
		as, err := srv.store.Alerts(true, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Kind == kind && a.SourceID != nil && *a.SourceID == sid {
				return true
			}
		}
		return false
	}
	for _, k := range []struct{ job, alert string }{{"check", "check-failed"}, {"copy", "copy-failed"}, {"drill", "drill-failed"}} {
		srv.jobOutcome(k.job, sid, false, "storage returned HTTP 503")
		if !open(k.alert) {
			t.Fatalf("failed %s raised no %s alert", k.job, k.alert)
		}
		srv.jobOutcome("backup", sid, true, "")
		if !open(k.alert) {
			t.Errorf("a successful backup closed the %s alert", k.alert)
		}
		srv.jobOutcome(k.job, sid, true, "")
		if open(k.alert) {
			t.Errorf("a successful %s left its alert open", k.job)
		}
	}
	srv.jobOutcome("backup", sid, false, "disk full")
	srv.jobOutcome("backup", sid, true, "")
	if open("job-failed") {
		t.Error("a successful backup left the backup's alert open")
	}
}
