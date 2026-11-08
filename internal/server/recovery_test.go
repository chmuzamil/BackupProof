package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestTargetState(t *testing.T) {
	limit := 10 * time.Hour
	for _, c := range []struct {
		measured, limit time.Duration
		want            string
	}{
		{time.Hour, 0, goalUnset},
		{0, limit, goalMet},
		{8*time.Hour - time.Second, limit, goalMet},
		{8 * time.Hour, limit, goalNear},
		{limit, limit, goalNear},
		{limit + time.Second, limit, goalMissed},
	} {
		if got := targetState(c.measured, c.limit); got != c.want {
			t.Errorf("%v against %v: %s, want %s", c.measured, c.limit, got, c.want)
		}
	}
}

func drillRow(t *testing.T, passed bool, rtoMs int64, snap time.Time) ProofRow {
	t.Helper()
	raw, err := json.Marshal(proof.DrillPredicate{SnapshotTime: snap})
	if err != nil {
		t.Fatal(err)
	}
	return ProofRow{Passed: passed, RTOMs: &rtoMs, Predicate: raw}
}

func TestRecoveryFrom(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	src := Source{RPOTargetHours: 24, RTOTargetMinutes: 10}

	if r := recoveryFrom(src, nil, now); r.RPOState != goalUnknown || r.RTOState != goalUnknown || r.VerifiedAgeMs != nil || r.RestoreMs != nil {
		t.Errorf("no tests must read as unknown, not zero: %+v", r)
	}
	if r := recoveryFrom(Source{}, nil, now); r.RPOState != goalUnset || r.RTOState != goalUnset {
		t.Errorf("no limits: %+v", r)
	}

	// A failed test, however new, proves nothing.
	r := recoveryFrom(src, []ProofRow{
		drillRow(t, false, 1000, now.Add(-time.Hour)),
		drillRow(t, true, 15*60000, now.Add(-30*time.Hour)),
	}, now)
	if r.RPOState != goalMissed || *r.VerifiedAgeMs != (30*time.Hour).Milliseconds() {
		t.Errorf("failed test counted towards the recovery point: %+v", r)
	}
	if r.RTOState != goalMissed || *r.RestoreMs != 15*60000 {
		t.Errorf("restore time should come from the last passed test: %+v", r)
	}

	// A manual test of an old backup doesn't move the recovery point back,
	// and the restore time is still the newest test's.
	r = recoveryFrom(src, []ProofRow{
		drillRow(t, true, 60000, now.Add(-90*24*time.Hour)),
		drillRow(t, true, 9*60000, now.Add(-2*time.Hour)),
	}, now)
	if r.RPOState != goalMet || r.VerifiedPoint == nil || !r.VerifiedPoint.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("recovery point: %+v", r)
	}
	if r.RTOState != goalMet || *r.RestoreMs != 60000 {
		t.Errorf("restore time: %+v", r)
	}

	// The instant counts, not the zone it was written in.
	karachi := time.FixedZone("PKT", 5*3600)
	r = recoveryFrom(src, []ProofRow{drillRow(t, true, 1, now.Add(-3*time.Hour).In(karachi))}, now)
	if *r.VerifiedAgeMs != (3 * time.Hour).Milliseconds() {
		t.Errorf("age across zones: %d", *r.VerifiedAgeMs)
	}

	// An agent whose clock runs ahead can't produce a negative age.
	r = recoveryFrom(src, []ProofRow{drillRow(t, true, 1, now.Add(time.Hour))}, now)
	if *r.VerifiedAgeMs != 0 {
		t.Errorf("future snapshot: %d", *r.VerifiedAgeMs)
	}

	// An old proof without a snapshot time stays unknown.
	r = recoveryFrom(src, []ProofRow{{Passed: true, Predicate: json.RawMessage(`{}`)}}, now)
	if r.VerifiedPoint != nil || r.RPOState != goalUnknown || r.RTOState != goalUnknown {
		t.Errorf("missing measurements: %+v", r)
	}
}

type recoveryFixture struct {
	srv    *Server
	source Source
	agent  int64
}

func newRecoveryFixture(t *testing.T, rpoHours, rtoMinutes int) *recoveryFixture {
	t.Helper()
	dir := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(dir, "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	res, err := srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created,last_seen) VALUES('web','key','hash',?,?)", now(), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := res.LastInsertId()
	repoID, err := srv.store.CreateRepository("disk", backend.Config{Type: "local", Path: dir}, RepoSecret{Password: "a-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.store.SaveSource(&Source{Name: "shop", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}},
		BackupCron: "0 2 * * *", DrillCron: "0 4 * * 0", Enabled: true, RPOTargetHours: rpoHours, RTOTargetMinutes: rtoMinutes}, &SourceSecret{Password: "a-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	src, err := srv.store.Source(id)
	if err != nil {
		t.Fatal(err)
	}
	return &recoveryFixture{srv: srv, source: *src, agent: agentID}
}

func (f *recoveryFixture) addDrill(t *testing.T, passed bool, rtoMs int64, snap time.Time) {
	t.Helper()
	pred, _ := json.Marshal(proof.DrillPredicate{SnapshotTime: snap})
	stmt, _ := json.Marshal(proof.Statement{Type: proof.StatementType, Subject: []proof.Subject{{Name: "snap"}}, PredicateType: proof.PredicateDrill, Predicate: pred})
	env, _ := json.Marshal(proof.Envelope{PayloadType: proof.PayloadType, Payload: base64.StdEncoding.EncodeToString(stmt)})
	var seq int
	_ = f.srv.store.db.QueryRow("SELECT COUNT(*)+1 FROM proofs").Scan(&seq)
	if _, err := f.srv.store.db.Exec(`INSERT INTO proofs(kind,source_id,source_name,snapshot_id,passed,rto_ms,envelope,ledger_seq,signer,created) VALUES('drill',?,?,?,?,?,?,?,'t',?)`,
		f.source.ID, f.source.Name, "snap", passed, rtoMs, string(env), seq, now()); err != nil {
		t.Fatal(err)
	}
}

func (f *recoveryFixture) open(t *testing.T, kind string) bool {
	t.Helper()
	as, err := f.srv.store.Alerts(true, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func TestTargetsAreSavedAndChecked(t *testing.T) {
	f := newRecoveryFixture(t, 24, 15)
	if f.source.RPOTargetHours != 24 || f.source.RTOTargetMinutes != 15 {
		t.Fatalf("targets not saved: %+v", f.source)
	}
	src := f.source
	src.RPOTargetHours, src.RTOTargetMinutes = 48, 0
	if _, err := f.srv.store.SaveSource(&src, &SourceSecret{}); err != nil {
		t.Fatal(err)
	}
	got, err := f.srv.store.Source(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RPOTargetHours != 48 || got.RTOTargetMinutes != 0 {
		t.Fatalf("targets not updated: %+v", got)
	}
	for _, bad := range []Source{{RPOTargetHours: -1}, {RTOTargetMinutes: -1}, {RPOTargetHours: maxRPOTargetHours + 1}, {RTOTargetMinutes: maxRTOTargetMinutes + 1}} {
		if validateTargets(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestPassingALimitRaisesAnAlertUntilItIsBack(t *testing.T) {
	f := newRecoveryFixture(t, 24, 10)
	now := time.Now()

	// Nothing measured: unknown, which the "restore test overdue" alert covers.
	f.srv.watchRecovery(f.source, now)
	if f.open(t, "rpo-missed") || f.open(t, "rto-missed") {
		t.Fatal("alerted with nothing measured")
	}

	f.addDrill(t, true, 20*60000, now.Add(-30*time.Hour))
	f.srv.watchRecovery(f.source, now)
	if !f.open(t, "rpo-missed") || !f.open(t, "rto-missed") {
		t.Fatal("past both limits, but not alerted")
	}

	f.addDrill(t, true, 5*60000, now.Add(-time.Hour))
	f.srv.watchRecovery(f.source, now)
	if f.open(t, "rpo-missed") || f.open(t, "rto-missed") {
		t.Fatal("back within both limits, but the alerts stayed open")
	}

	// Clearing a limit closes its alert too.
	f.addDrill(t, true, 20*60000, now.Add(-time.Hour))
	f.srv.watchRecovery(f.source, now)
	if !f.open(t, "rto-missed") {
		t.Fatal("past the restore-time limit, but not alerted")
	}
	noLimit := f.source
	noLimit.RTOTargetMinutes = 0
	f.srv.watchRecovery(noLimit, now)
	if f.open(t, "rto-missed") {
		t.Fatal("removing the limit left its alert open")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := newRecoveryFixture(t, 24, 10)
	snap := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	f.addDrill(t, false, 1000, snap)
	f.addDrill(t, true, 90500, snap)
	// A second server with the same name must not repeat a series.
	if _, err := f.srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created) VALUES('web','key2','hash2',?)", now()); err != nil {
		t.Fatal(err)
	}
	uid, err := f.srv.store.CreateUser("ops", "a-long-enough-password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.srv.store.CreateAPIToken(uid, "prometheus", "auditor", 0)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(f.srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), text)
	}
	for _, want := range []string{
		`backupproof_source_status{source="shop",status="proven"} 1`,
		`backupproof_source_status{source="shop",status="failing"} 0`,
		`backupproof_last_restore_test_success{source="shop"} 1`,
		`backupproof_last_restore_duration_seconds{source="shop"} 90.5`,
		`backupproof_rpo_target_seconds{source="shop"} 86400`,
		`backupproof_rto_target_seconds{source="shop"} 600`,
		`backupproof_restore_tests_total{source="shop",result="passed"} 1`,
		`backupproof_restore_tests_total{source="shop",result="failed"} 1`,
		`backupproof_agent_up{agent="web",id="1"} 1`,
		`backupproof_agent_up{agent="web",id="2"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "backupproof_last_backup_timestamp_seconds") {
		t.Errorf("no backup has run, so it must have no sample:\n%s", text)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		key := line
		if !strings.HasPrefix(line, "#") {
			key = line[:strings.LastIndex(line, " ")]
		}
		if seen[key] {
			t.Errorf("repeated line: %s", line)
		}
		seen[key] = true
	}
}

func TestMetricLabelsAreEscaped(t *testing.T) {
	m := metric{name: "x"}
	m.add(1, "source", "a\"b\\c\nd")
	if want := `x{source="a\"b\\c\nd"} 1`; m.samples[0] != want {
		t.Errorf("%s, want %s", m.samples[0], want)
	}
}
