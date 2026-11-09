package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/retention"
	"github.com/chmuzamil/backupproof/internal/source"
)

func (f *recoveryFixture) failDrillJob(t *testing.T, msg string) {
	t.Helper()
	id, err := f.srv.store.EnqueueJob("drill", f.source.ID, f.agent, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.db.Exec("UPDATE jobs SET state='running' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.FinishJob(id, f.agent, false, msg, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreTestThatCouldNotFinishIsNotGreen(t *testing.T) {
	f := newRecoveryFixture(t, 0, 0)
	f.addDrill(t, true, 1000, time.Now())
	time.Sleep(10 * time.Millisecond)
	f.failDrillJob(t, "storage is down")
	st := f.srv.sourceStatus(f.source, nil, nil)
	if st.Status != "at-risk" || !strings.Contains(st.Reason, "storage is down") {
		t.Errorf("status %s, reason %q", st.Status, st.Reason)
	}
	time.Sleep(10 * time.Millisecond)
	f.addDrill(t, true, 1000, time.Now())
	if st := f.srv.sourceStatus(f.source, nil, nil); st.Status != "proven" {
		t.Errorf("after a passing test: %s %q", st.Status, st.Reason)
	}
}

func TestRealFailureReplacesHarmlessAlert(t *testing.T) {
	f := newRecoveryFixture(t, 0, 0)
	id := f.source.ID
	st := f.srv.store
	if !st.RaiseAlert("drill-failed", &id, nil, "shop: storage errors. "+alertNothingDamaged+"; it runs again.") {
		t.Fatal("first alert not new")
	}
	if st.RaiseAlert("drill-failed", &id, nil, "shop: storage errors again. "+alertNothingDamaged+".") {
		t.Error("a second harmless alert notified")
	}
	if !st.RaiseAlert("drill-failed", &id, nil, "shop: the restore test FAILED.") {
		t.Error("a real failure was hidden")
	}
	if st.RaiseAlert("drill-failed", &id, nil, "shop: the restore test failed: details") {
		t.Error("a second wording of the same failure notified")
	}
	as, _ := st.Alerts(true, 10)
	if len(as) != 1 || !strings.Contains(as[0].Message, "FAILED") {
		t.Errorf("alerts: %+v", as)
	}
}

func TestOperatorCannotUpdateThroughPost(t *testing.T) {
	f := newRecoveryFixture(t, 0, 0)
	res, err := f.srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created,last_seen) VALUES('other','key2','hash2',?,?)", now(), now())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	body, _ := json.Marshal(map[string]any{"id": f.source.ID, "name": "shop", "agentId": other, "repoId": f.source.RepoID, "enabled": true,
		"backupCron": "0 2 * * *", "drillCron": "0 4 * * 0", "spec": source.Spec{Name: "shop", Kind: "files", Paths: []string{"/a"}}})
	r := httptest.NewRequest(http.MethodPost, "/api/sources", strings.NewReader(string(body)))
	r = r.WithContext(context.WithValue(r.Context(), userKey, &User{Username: "op", Role: "operator"}))
	f.srv.handleSaveSource(httptest.NewRecorder(), r)
	src, err := f.srv.store.Source(f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.AgentID != f.agent {
		t.Error("POST moved an existing item to another server")
	}
}

func TestOperatorLimits(t *testing.T) {
	spec := source.Spec{Name: "x", Kind: "files", Paths: []string{"/a"}}
	old := &Source{Spec: spec, Retention: retention.Policy{KeepDaily: 7, KeepLastVerified: 1}}
	image := &Source{Spec: spec, Retention: old.Retention}
	image.Spec.Drill.Image = "evil:1"
	if operatorMayChange(old, image) == nil {
		t.Error("an operator chose the restore-test image")
	}
	fewer := &Source{Spec: spec, Retention: retention.Policy{KeepDaily: 1, KeepLastVerified: 1}}
	if operatorMayChange(old, fewer) == nil {
		t.Error("an operator reduced how many backups are kept")
	}
	more := &Source{Spec: spec, Retention: retention.Policy{KeepDaily: 14, KeepLastVerified: 1}}
	if err := operatorMayChange(old, more); err != nil {
		t.Error(err)
	}
}

func TestDataLossLimitSurvivesAlongRunOfFailures(t *testing.T) {
	f := newRecoveryFixture(t, 1, 0)
	f.addDrill(t, true, 1000, time.Now().Add(-48*time.Hour))
	for range recoveryTests {
		f.addDrill(t, false, 1000, time.Now())
	}
	f.srv.watchRecovery(f.source, time.Now())
	if !f.open(t, "rpo-missed") {
		t.Fatal("the limit was forgotten after a long run of failures")
	}
}

func TestDataLossLimitAppliesToItemsNeverTested(t *testing.T) {
	f := newRecoveryFixture(t, 1, 0)
	f.srv.watchRecovery(f.source, time.Now())
	if f.open(t, "rpo-missed") {
		t.Fatal("a brand new item is over its limit")
	}
	f.srv.watchRecovery(f.source, time.Now().Add(2*time.Hour))
	if !f.open(t, "rpo-missed") {
		t.Error("an item never restore-tested didn't raise the alert")
	}
}

func TestWeekendsAreNotOverdue(t *testing.T) {
	f := newRecoveryFixture(t, 0, 0)
	f.source.BackupCron = "0 2 * * 1-5"
	f.addDrill(t, true, 1000, time.Now())
	if _, err := f.srv.store.db.Exec(`INSERT INTO proofs(kind,source_id,source_name,snapshot_id,passed,envelope,ledger_seq,signer,created) VALUES('backup',?,?,'b',1,'{}',999,'t','2026-11-06T02:05:00Z')`, f.source.ID, f.source.Name); err != nil {
		t.Fatal(err)
	}
	src := []Source{f.source}
	at := func(s string) time.Time {
		x, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	f.srv.watchdog(at("2026-11-07T15:00:00Z"), src) // Saturday
	f.srv.watchdog(at("2026-11-08T15:00:00Z"), src) // Sunday
	if f.open(t, "backup-overdue") {
		t.Fatal("a weekend counted as overdue")
	}
	f.srv.watchdog(at("2026-11-09T15:00:00Z"), src) // Monday, past that day's run
	if !f.open(t, "backup-overdue") {
		t.Error("a missed Monday backup wasn't noticed")
	}
}

func TestPausedAndManualItemsCloseTheirAlerts(t *testing.T) {
	for _, change := range []func(*Source){func(s *Source) { s.Enabled = false }, func(s *Source) { s.BackupCron = "manual" }} {
		f := newRecoveryFixture(t, 0, 0)
		id := f.source.ID
		f.srv.store.RaiseAlert("backup-overdue", &id, nil, "late")
		change(&f.source)
		f.srv.watchdog(time.Now(), []Source{f.source})
		if f.open(t, "backup-overdue") {
			t.Errorf("alert still open for %+v", f.source.BackupCron)
		}
	}
}

func TestLostLeaseIsDistinct(t *testing.T) {
	f := newRecoveryFixture(t, 0, 0)
	if err := f.srv.store.ExtendLease(424242, f.agent, time.Minute, ""); err != errLeaseLost {
		t.Errorf("err = %v", err)
	}
}

func TestVerifierDrillGetsNoItemSecrets(t *testing.T) {
	f := newPlanFixture(t)
	if _, err := f.srv.store.SaveSource(&Source{ID: f.source.ID, Name: f.source.Name, AgentID: f.agent, RepoID: f.source.RepoID,
		Spec: source.Spec{Name: "shop", Kind: "postgres", Host: "db", User: "u", Database: "d"}, BackupCron: "0 2 * * *", DrillCron: "0 4 * * 0", Enabled: true},
		&SourceSecret{Password: "the-database-password"}); err != nil {
		t.Fatal(err)
	}
	f.addProof(t, f.source, "backup", "snap1", true, time.Now(), nil)
	for _, c := range []struct {
		kind string
		want string
	}{{"backup", "the-database-password"}, {"drill", ""}} {
		lease, err := f.srv.buildLease(&Job{Kind: c.kind, SourceID: f.source.ID, AgentID: f.agent})
		if err != nil {
			t.Fatal(err)
		}
		if lease.Source.Password != c.want {
			t.Errorf("%s lease password = %q", c.kind, lease.Source.Password)
		}
	}
}

func TestSigninLimiter(t *testing.T) {
	var l signinLimiter
	now := time.Now()
	for range signinFailures {
		if !l.allowed("a", now) {
			t.Fatal("limited too early")
		}
		l.failed("a", now)
	}
	if l.allowed("a", now) {
		t.Error("not limited after repeated failures")
	}
	if !l.allowed("b", now) {
		t.Error("another address was limited")
	}
	if !l.allowed("a", now.Add(signinWindow+time.Second)) {
		t.Error("still limited after the window")
	}
	l.clear("a")
	if !l.allowed("a", now) {
		t.Error("a successful sign-in didn't clear the count")
	}
}

func TestClientAddrOnlyTrustsALocalProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := clientAddr(r); got != "203.0.113.9" {
		t.Errorf("direct: %s", got)
	}
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 198.51.100.7")
	if got := clientAddr(r); got != "198.51.100.7" {
		t.Errorf("proxied: %s", got)
	}
}

func TestPlanForUntestedItemHasUsableSteps(t *testing.T) {
	f := newPlanFixture(t)
	src := f.item(t, "site", source.Spec{Kind: "files", Paths: []string{"/a"}}, nil)
	_ = src
	it := findItem(t, f.plan(t), "site")
	all := strings.Join(append(it.UISteps, it.CLISteps...), "\n")
	if strings.Contains(all, "((") || strings.Contains(all, "(newest") {
		t.Errorf("steps: %s", all)
	}
}

func TestPlanGivesTheSFTPSettings(t *testing.T) {
	env := planCLIEnv(backend.Config{Type: "sftp", HostKey: "ssh-ed25519 AAAA"})
	if !strings.Contains(env, "BP_SFTP_HOSTKEY='ssh-ed25519 AAAA'") || !strings.Contains(env, "BP_SFTP_KEY_FILE") {
		t.Errorf("env: %s", env)
	}
}
