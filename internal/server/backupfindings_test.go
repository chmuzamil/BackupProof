package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

func TestAnomaly(t *testing.T) {
	normal := snapshot.Stats{Files: 1000, Unchanged: 950, Bytes: 500 << 20}
	cases := []struct {
		name      string
		prev, cur snapshot.Stats
		want      string // substring, or "" for no anomaly
	}{
		{"steady", normal, snapshot.Stats{Files: 1010, Unchanged: 980, Bytes: 505 << 20}, ""},
		{"shrank", normal, snapshot.Stats{Files: 1000, Unchanged: 990, Bytes: 100 << 20}, "down from 500.0 MB"},
		{"files gone", normal, snapshot.Stats{Files: 300, Unchanged: 300, Bytes: 480 << 20}, "300 files, down from 1000"},
		{"all changed", normal, snapshot.Stats{Files: 1000, Unchanged: 20, Bytes: 510 << 20}, "980 of its 1000 files changed"},
		// An item that always changes (a database dump folder) isn't flagged.
		{"always changes", snapshot.Stats{Files: 1000, Unchanged: 10, Bytes: 500 << 20}, snapshot.Stats{Files: 1000, Unchanged: 0, Bytes: 500 << 20}, ""},
		// Small items are left alone.
		{"small", snapshot.Stats{Files: 50, Unchanged: 50, Bytes: 4 << 20}, snapshot.Stats{Files: 10, Bytes: 1 << 20}, ""},
	}
	for _, c := range cases {
		got := anomaly(c.prev, c.cur)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// newFindingsServer is a server with one item, for the alert tests.
func newFindingsServer(t *testing.T) (*Server, int64) {
	t.Helper()
	dir := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(dir, "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	res, err := srv.store.db.Exec("INSERT INTO agents(name,public_key,token_hash,created) VALUES('web','key','hash',?)", now())
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := res.LastInsertId()
	repoID, err := srv.store.CreateRepository("disk", backend.Config{Type: "local", Path: dir}, RepoSecret{Password: "a-long-repository-password"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.store.SaveSource(&Source{Name: "site", AgentID: agentID, RepoID: repoID, Spec: source.Spec{Kind: "files", Paths: []string{dir}},
		BackupCron: "@daily", DrillCron: "@weekly", Enabled: true}, &SourceSecret{})
	if err != nil {
		t.Fatal(err)
	}
	return srv, id
}

func openAlert(t *testing.T, srv *Server, kind string) *Alert {
	t.Helper()
	all, err := srv.store.Alerts(true, 50)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].Kind == kind {
			return &all[i]
		}
	}
	return nil
}

// A backup that was saved but skipped files, or couldn't clean up, raises an
// alert that the next clean backup closes. An unusual backup stays open until
// someone dismisses it.
func TestBackupFindings(t *testing.T) {
	srv, id := newFindingsServer(t)
	result := func(st snapshot.Stats, warnings []string, merr string) any {
		return map[string]any{"stats": st, "warnings": warnings, "maintenanceError": merr}
	}
	normal := snapshot.Stats{Files: 1000, Unchanged: 950, Bytes: 500 << 20}
	srv.backupFindings(id, nil, result(normal, []string{"3 files or folders couldn't be read and aren't in this backup, such as /a."}, "refusing to prune: 1 snapshot(s) unreadable"))
	if a := openAlert(t, srv, "backup-warnings"); a == nil || !strings.Contains(a.Message, "3 files or folders couldn't be read") {
		t.Fatalf("no warnings alert: %+v", a)
	}
	if a := openAlert(t, srv, "maintenance-failed"); a == nil || !strings.Contains(a.Message, "refusing to prune") {
		t.Fatalf("no maintenance alert: %+v", a)
	}
	raw, _ := json.Marshal(result(normal, nil, ""))
	prev := &Job{Result: raw}
	srv.backupFindings(id, prev, result(snapshot.Stats{Files: 1000, Unchanged: 5, Bytes: 500 << 20}, nil, ""))
	if openAlert(t, srv, "backup-warnings") != nil || openAlert(t, srv, "maintenance-failed") != nil {
		t.Fatal("a clean backup should close the warnings and maintenance alerts")
	}
	a := openAlert(t, srv, "backup-anomaly")
	if a == nil || !strings.Contains(a.Message, "995 of its 1000 files changed") {
		t.Fatalf("no anomaly alert: %+v", a)
	}
	// The next backup looks like this one, so it isn't an anomaly, but the
	// alert stays: encrypted files look normal next to encrypted files.
	raw, _ = json.Marshal(result(snapshot.Stats{Files: 1000, Unchanged: 5, Bytes: 500 << 20}, nil, ""))
	srv.backupFindings(id, &Job{Result: raw}, result(snapshot.Stats{Files: 1000, Unchanged: 990, Bytes: 500 << 20}, nil, ""))
	if openAlert(t, srv, "backup-anomaly") == nil {
		t.Fatal("the anomaly alert closed by itself")
	}

	// Dismissing: only an administrator, and only alerts nothing else closes.
	h := srv.Handler()
	tok := adminToken(t, srv)
	req := httptest.NewRequest("POST", "/api/alerts/"+itoa(a.ID)+"/dismiss", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if openAlert(t, srv, "backup-anomaly") != nil {
		t.Fatal("the anomaly alert is still open after dismissing it")
	}
	srv.store.RaiseAlert("backup-failed", &id, nil, "site: the backup failed")
	other := openAlert(t, srv, "backup-failed")
	req = httptest.NewRequest("POST", "/api/alerts/"+itoa(other.ID)+"/dismiss", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("dismissing a failure should be refused: %d", rec.Code)
	}
}

// A storage bigger than the size an administrator set raises an alert.
func TestStorageSizeWarning(t *testing.T) {
	srv, id := newFindingsServer(t)
	src, _ := srv.store.Source(id)
	srv.store.SetSetting("storage-size-warnings", `{"`+itoa(src.RepoID)+`": 1000000000}`)
	srv.checkSize(src.RepoID, src, 2_000_000_000)
	if a := openAlert(t, srv, "storage-large"); a == nil || !strings.Contains(a.Message, "1.9 GB") {
		t.Fatalf("no size alert: %+v", a)
	}
	srv.checkSize(src.RepoID, src, 500_000_000)
	if openAlert(t, srv, "storage-large") != nil {
		t.Fatal("the size alert should close when the storage is below the limit")
	}
}

// A failed alert is kept and retried until the channel takes it, the
// channel's failure shows on the home page meanwhile, and a problem still
// open a day later is sent again.
func TestNotifyRetryAndReminder(t *testing.T) {
	var fail atomic.Bool
	var got atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		got.Add(1)
	}))
	defer fake.Close()
	srv, id := newFindingsServer(t)
	b, _ := json.Marshal(NotifySettings{WebhookURL: fake.URL + "/hook", HeartbeatURL: fake.URL + "/hb"})
	srv.store.SetSetting("notify", string(b))

	fail.Store(true)
	srv.notifyOrQueue(event{Subject: "BackupProof: backup failed", Body: "site", Urgent: true})
	var queued int
	srv.store.db.QueryRow("SELECT count(*) FROM notify_outbox").Scan(&queued)
	if queued != 1 {
		t.Fatalf("%d notifications queued, want 1", queued)
	}
	if ns := srv.notices(); len(ns) != 1 || !strings.Contains(ns[0].Text, "aren't reaching you by webhook") {
		t.Fatalf("notices: %+v", ns)
	}
	// Not due yet: nothing is sent.
	srv.retryNotifications(time.Now())
	fail.Store(false)
	srv.retryNotifications(time.Now().Add(2 * time.Minute))
	srv.store.db.QueryRow("SELECT count(*) FROM notify_outbox").Scan(&queued)
	if queued != 0 || got.Load() != 1 {
		t.Fatalf("after the retry: %d queued, %d delivered", queued, got.Load())
	}
	if ns := srv.notices(); len(ns) != 0 {
		t.Fatalf("notices after delivery works again: %+v", ns)
	}

	srv.store.RaiseAlert("backup-failed", &id, nil, "site: the backup failed")
	srv.remindOpenAlerts(time.Now())
	time.Sleep(100 * time.Millisecond)
	if got.Load() != 1 {
		t.Fatal("a reminder was sent for an alert that is less than a day old")
	}
	later := time.Now().Add(25 * time.Hour)
	srv.remindOpenAlerts(later)
	srv.remindOpenAlerts(later) // once a day, not every tick
	deadline := time.Now().Add(2 * time.Second)
	for got.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got.Load() != 2 {
		t.Fatalf("%d deliveries, want 2 (one reminder)", got.Load())
	}
}

// Without any alert channel or heartbeat, the home page says so.
func TestSetupNotices(t *testing.T) {
	srv, _ := newFindingsServer(t)
	ns := srv.notices()
	if len(ns) != 2 || !strings.Contains(ns[0].Text, "No alert channel") || !strings.Contains(ns[1].Text, "heartbeat") {
		t.Fatalf("notices: %+v", ns)
	}
}

func adminToken(t *testing.T, srv *Server) string {
	t.Helper()
	uid, err := srv.store.CreateUser("owner", "a-long-enough-password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := srv.store.CreateAPIToken(uid, "test", "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
