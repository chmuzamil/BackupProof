package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/transparency"
)

// The ledger is a transparency log: a witness cosigns its checkpoints, and an
// auditor proves an entry is in it from tiles, without trusting the dashboard.
func TestTransparencyLogWithWitness(t *testing.T) {
	ctx := context.Background()
	srv, err := New(Config{DataDir: t.TempDir(), NoLocalAgent: true, PublicURL: "https://backup.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for i := range 300 {
		if err := srv.store.Audit("test", "event", fmt.Sprintf("entry %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if srv.logOrigin() != "backup.example.com/log" {
		t.Fatalf("origin %q", srv.logOrigin())
	}
	logKey, err := transparency.ParseVerifier(srv.LogKey())
	if err != nil {
		t.Fatal(err)
	}

	// A witness on another machine, trusting this log's key.
	_, wpriv, _ := ed25519.GenerateKey(nil)
	wsigner := transparency.Signer{Name: "witness.example", Key: wpriv}
	witness := &transparency.Witness{Signer: wsigner, Logs: map[string]transparency.Verifier{logKey.Name: logKey}, State: filepath.Join(t.TempDir(), "w.json")}
	ws := httptest.NewServer(witness)
	defer ws.Close()
	wkey := transparency.WitnessVerifierKey(wsigner.Name, wpriv.Public().(ed25519.PublicKey))
	wv, _ := transparency.ParseVerifier(wkey)
	raw, _ := json.Marshal([]WitnessConfig{{URL: ws.URL, Key: wkey}})
	if err := srv.store.SetSetting(settingTlogWitnesses, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := srv.refreshCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}

	// The log is served publicly here, as an auditor would fetch it.
	if err := srv.store.SetSetting(settingTlogPublic, "1"); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	get := func(_ context.Context, path string) ([]byte, error) {
		resp, err := http.Get(web.URL + "/log/" + path)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s: %s", path, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	note, err := get(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	cp, cosigned, err := transparency.VerifyNote(string(note), logKey, []transparency.Verifier{wv})
	if err != nil {
		t.Fatalf("checkpoint: %v\n%s", err, note)
	}
	if cp.Size != 300 || cosigned["witness.example"].IsZero() {
		t.Fatalf("checkpoint at %d, cosigned %v", cp.Size, cosigned)
	}
	rows, _ := srv.store.Ledger(0, 0)
	for _, i := range []int{0, 150, 299} {
		e := rows[i]
		if err := transparency.VerifyInclusion(ctx, get, cp, e.Seq-1, transparency.LeafData(e.Hash)); err != nil {
			t.Fatalf("ledger entry %d: %v", e.Seq, err)
		}
	}

	// The log grows; the witness follows with a consistency proof.
	for i := range 40 {
		_ = srv.store.Audit("test", "event", fmt.Sprintf("later %d", i))
	}
	if err := srv.refreshCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	note2, _ := get(ctx, "checkpoint")
	cp2, _, err := transparency.VerifyNote(string(note2), logKey, []transparency.Verifier{wv})
	if err != nil || cp2.Size != 340 {
		t.Fatalf("grown checkpoint: %+v %v", cp2, err)
	}
	if err := transparency.VerifyConsistency(ctx, get, cp, cp2); err != nil {
		t.Fatalf("consistency: %v", err)
	}
	st := srv.transparencySettings()
	if len(st.Witnesses) != 1 || st.Witnesses[0].Size != 340 || st.Witnesses[0].LastError != "" {
		t.Errorf("witness state: %+v", st.Witnesses)
	}

	// Private by default: no sign-in, no log.
	if err := srv.store.SetSetting(settingTlogPublic, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := get(ctx, "checkpoint"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a private log answered without a sign-in: %v", err)
	}

	// The witness goes away. A short outage is tolerated; one longer than
	// witnessGrace raises an alert and fails the health endpoint, which
	// needs no sign-in although the log is private.
	health := func() int {
		resp, err := http.Get(web.URL + "/healthz/witnesses")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := health(); code != 200 {
		t.Errorf("health with a working witness: %d", code)
	}
	ws.Close()
	_ = srv.store.Audit("test", "event", "while the witness is down")
	if err := srv.refreshCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if code := health(); code != 200 {
		t.Errorf("health right after the witness went down: %d, want 200 until the grace ends", code)
	}
	down := srv.witnesses()
	if down[0].LastError == "" || down[0].FailingSince == "" {
		t.Fatalf("a failed cosign request wasn't recorded: %+v", down[0])
	}
	down[0].FailingSince = time.Now().Add(-witnessGrace - time.Minute).UTC().Format(time.RFC3339)
	raw, _ = json.Marshal(down)
	_ = srv.store.SetSetting(settingTlogWitnesses, string(raw))
	if err := srv.refreshCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if code := health(); code != 503 {
		t.Errorf("health after the witness failed for longer than the grace: %d, want 503", code)
	}
	if n := openAlerts(t, srv, "witness-failed"); n != 1 {
		t.Errorf("%d witness-failed alerts open, want 1", n)
	}

	// Back up: the next round cosigns, and the alert resolves.
	ws2 := httptest.NewServer(witness)
	defer ws2.Close()
	back := srv.witnesses()
	back[0].URL = ws2.URL
	raw, _ = json.Marshal(back)
	_ = srv.store.SetSetting(settingTlogWitnesses, string(raw))
	if err := srv.refreshCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if code := health(); code != 200 {
		t.Errorf("health after the witness came back: %d", code)
	}
	if n := openAlerts(t, srv, "witness-failed"); n != 0 {
		t.Errorf("%d witness-failed alerts still open after it came back", n)
	}
}

func openAlerts(t *testing.T, srv *Server, kind string) int {
	t.Helper()
	var n int
	if err := srv.store.db.QueryRow("SELECT COUNT(*) FROM alerts WHERE kind=? AND resolved IS NULL", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A dashboard upgraded from a version without the log adds its existing
// ledger to the log when it starts.
func TestTransparencyLogBackfill(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Config{DataDir: dir, NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		_ = srv.store.Audit("test", "event", fmt.Sprintf("entry %d", i))
	}
	// Forget the log, as a database from 0.4 would have none.
	if _, err := srv.store.db.Exec("DROP TABLE tlog_hashes"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.db.Exec("DELETE FROM settings WHERE key=?", settingTlogSize); err != nil {
		t.Fatal(err)
	}
	root := func(s *Server) string { r, _ := s.store.TlogRoot(s.store.TlogSize()); return fmt.Sprintf("%x", r) }
	srv.Close()
	again, err := New(Config{DataDir: dir, NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.store.TlogSize() != 20 {
		t.Fatalf("log size %d after backfill, want 20", again.store.TlogSize())
	}
	// Same entries, same root as a log built as they were written.
	m := &transparency.MemoryLog{}
	rows, _ := again.store.Ledger(0, 0)
	for _, e := range rows {
		_ = m.Append(transparency.LeafData(e.Hash))
	}
	c, _ := m.Checkpoint("x")
	if root(again) != fmt.Sprintf("%x", c.Root) {
		t.Error("the backfilled log's root differs")
	}
	// It's append-only.
	if _, err := again.store.db.Exec("DELETE FROM tlog_hashes"); err == nil {
		t.Error("deleted from the log")
	}
}
