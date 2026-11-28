package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/ops"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/source"
)

// The dashboard backs up its own state — accounts, items, schedules, the proof
// ledger and its keys — to a storage the administrator picks, so a lost
// dashboard server can be rebuilt with `backupproof server restore-dashboard`.
const (
	// DashboardSourceName names the dashboard's own backups in a storage.
	DashboardSourceName = "BackupProof dashboard"
	dashboardKeep       = 14 // newest dashboard backups kept in the storage
	dashboardEvery      = 24 * time.Hour

	settingDashRepo  = "dashboard-backup-repo"
	settingDashLast  = "dashboard-backup-last"  // time of the last good backup
	settingDashError = "dashboard-backup-error" // why the last try failed, or ""
	settingDashNext  = "dashboard-backup-next"
)

// DashboardInfo is info.json inside a dashboard backup.
type DashboardInfo struct {
	Version   string    `json:"version"`
	Time      time.Time `json:"time"`
	PublicURL string    `json:"publicUrl,omitempty"`
	// SecretFromEnv: the server's secret key came from BP_SECRET_KEY, so it
	// isn't in the backup and must be set again on the new server.
	SecretFromEnv bool `json:"secretFromEnv,omitempty"`
}

var dashboardRunning atomic.Bool

// dashboardLocalOK lets the tests back up to a folder; a real dashboard must
// use storage that survives losing its own server.
var dashboardLocalOK = false

// scheduleDashboardBackup starts the daily dashboard backup when it's due.
func (s *Server) scheduleDashboardBackup(now time.Time) {
	if s.dashboardRepoID() == 0 || s.store.RepositoryPaused(s.dashboardRepoID()) {
		return // a storage being upgraded waits; the backup runs after
	}
	next, err := time.Parse(time.RFC3339, s.store.Setting(settingDashNext))
	if err == nil && now.Before(next) {
		return
	}
	_ = s.store.SetSetting(settingDashNext, now.Add(dashboardEvery).UTC().Format(time.RFC3339))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		_ = s.backupDashboard(ctx, "schedule")
	}()
}

func (s *Server) dashboardRepoID() int64 {
	id, _ := strconv.ParseInt(s.store.Setting(settingDashRepo), 10, 64)
	return id
}

// backupDashboard backs up the dashboard's state once and records a signed proof.
func (s *Server) backupDashboard(ctx context.Context, actor string) error {
	if !dashboardRunning.CompareAndSwap(false, true) {
		return errors.New("a dashboard backup is already running")
	}
	defer dashboardRunning.Store(false)
	if s.store.RepositoryPaused(s.dashboardRepoID()) {
		return errors.New("its storage is being upgraded; try again when that finishes")
	}
	snap, err := s.runDashboardBackup(ctx)
	if err != nil {
		_ = s.store.SetSetting(settingDashError, err.Error())
		s.alert("dashboard-backup-failed", nil, nil, "The dashboard's own backup failed: "+err.Error()+". Without it, a lost dashboard server means setting up accounts, items and schedules again.")
		s.audit(actor, "dashboard-backup", "failed: "+err.Error())
		return err
	}
	_ = s.store.SetSetting(settingDashError, "")
	_ = s.store.SetSetting(settingDashLast, time.Now().UTC().Format(time.RFC3339))
	s.resolve("dashboard-backup-failed", nil, nil)
	s.audit(actor, "dashboard-backup", "backed up the dashboard as "+snap.ID.Short())
	return nil
}

func (s *Server) runDashboardBackup(ctx context.Context) (snapshot.WithID, error) {
	repoID := s.dashboardRepoID()
	r, sec, err := s.store.Repository(repoID)
	if err != nil {
		return snapshot.WithID{}, errors.New("the storage chosen for the dashboard's backup no longer exists; choose another under Settings")
	}
	if r.Backend.Type == "local" && !dashboardLocalOK {
		return snapshot.WithID{}, errors.New("the dashboard can't back itself up to a folder on its own server, which would be lost with it; choose cloud storage or another server")
	}
	dir, err := os.MkdirTemp("", "bp-dashboard-*")
	if err != nil {
		return snapshot.WithID{}, err
	}
	defer os.RemoveAll(dir)
	if err := s.collectDashboardState(dir); err != nil {
		return snapshot.WithID{}, err
	}
	be, err := backend.Open(ctx, r.Backend, sec.Credentials)
	if err != nil {
		return snapshot.WithID{}, fmt.Errorf("opening the storage: %s", friendlyStorageError(err))
	}
	rp, err := repo.Open(ctx, be, []byte(sec.Password))
	if err != nil {
		be.Close()
		return snapshot.WithID{}, err
	}
	defer rp.Close()
	env := &ops.Env{Repo: rp, Storage: proof.StorageInfo{Location: be.Location()}, Signer: s.key,
		Ledger: &dashboardLedger{s: s}, TSAs: s.tsaURLs(), Log: func(format string, a ...any) { s.log.Printf("dashboard backup: "+format, a...) }}
	spec := source.Spec{Name: DashboardSourceName, Kind: "files", Paths: []string{dir}}
	snap, _, err := ops.Backup(ctx, env, spec, []string{"dashboard"})
	if err != nil {
		return snapshot.WithID{}, err
	}
	for _, w := range env.Warnings {
		s.log.Printf("dashboard backup: warning: %s", w)
	}
	s.forgetOldDashboardBackups(ctx, rp)
	return snap, nil
}

// collectDashboardState writes a consistent copy of the database and the keys into dir.
func (s *Server) collectDashboardState(dir string) error {
	// VACUUM INTO writes a consistent copy while the dashboard keeps running.
	if _, err := s.store.db.Exec("VACUUM INTO ?", filepath.Join(dir, "backupproof.db")); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	files := []string{"server.key", "secret.key", filepath.Join("builtin-agent", "agent.json"), filepath.Join("builtin-agent", "agent.key")}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(s.cfg.DataDir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
			return err
		}
	}
	info := DashboardInfo{Version: engine.Version, Time: time.Now().UTC(), PublicURL: s.cfg.PublicURL, SecretFromEnv: os.Getenv("BP_SECRET_KEY") != ""}
	raw, _ := json.MarshalIndent(info, "", "  ")
	return os.WriteFile(filepath.Join(dir, "info.json"), raw, 0o600)
}

// forgetOldDashboardBackups keeps the newest dashboardKeep dashboard backups.
// The space is given back by the storage's next clean-up.
func (s *Server) forgetOldDashboardBackups(ctx context.Context, rp *repo.Repo) {
	all, err := snapshot.List(ctx, rp) // newest first
	if err != nil {
		return
	}
	var old []string
	n := 0
	for _, sn := range all {
		if sn.Source.Name != DashboardSourceName {
			continue
		}
		if n++; n > dashboardKeep {
			old = append(old, sn.ID.String())
		}
	}
	if len(old) > 0 {
		if err := engine.Forget(ctx, rp, old); err != nil {
			s.log.Printf("dashboard backup: removing old copies: %v", err)
		}
	}
}

// dashboardLedger records the dashboard's own backups in its proof ledger.
type dashboardLedger struct{ s *Server }

func (l *dashboardLedger) Append(string, string, string) (proof.LedgerEntry, error) {
	return proof.LedgerEntry{}, errors.New("dashboard ledger requires AppendRecord")
}

func (l *dashboardLedger) AppendRecord(_ context.Context, rec *ops.Record) (proof.LedgerEntry, error) {
	row := &ProofRow{Kind: rec.Kind, SourceName: rec.Source, SnapshotID: rec.SnapshotID, Passed: rec.Passed,
		Envelope: rec.Envelope, Timestamp: rec.Timestamp, Signer: "dashboard (" + l.s.key.Public().KeyID + ")"}
	return l.s.store.AppendLedger(rec.Kind, rec.SnapshotID, rec.Envelope.Digest(), rec.Source, func(tx *sql.Tx, e proof.LedgerEntry) error {
		row.LedgerSeq = e.Seq
		return insertProof(tx, row)
	})
}

// DashboardBackupStatus is what the Settings page shows.
type DashboardBackupStatus struct {
	RepoID    int64  `json:"repoId"`
	Last      string `json:"last,omitempty"`
	LastError string `json:"lastError,omitempty"`
	Next      string `json:"next,omitempty"`
	Running   bool   `json:"running"`
}

func (s *Server) dashboardBackupStatus() DashboardBackupStatus {
	st := DashboardBackupStatus{RepoID: s.dashboardRepoID(), Last: s.store.Setting(settingDashLast),
		LastError: s.store.Setting(settingDashError), Running: dashboardRunning.Load()}
	if st.RepoID != 0 {
		st.Next = s.store.Setting(settingDashNext)
	}
	return st
}

func (s *Server) handleGetDashboardBackup(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.dashboardBackupStatus())
}

// handleSetDashboardBackup chooses the storage (0 turns it off).
func (s *Server) handleSetDashboardBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoID int64 `json:"repoId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.RepoID != 0 {
		rp, _, err := s.store.Repository(req.RepoID)
		if err != nil {
			writeErr(w, 404, errors.New("no such storage"))
			return
		}
		if rp.Backend.Type == "local" && !dashboardLocalOK {
			writeErr(w, 400, errors.New("choose storage that isn't on this server: a folder here would be lost with the dashboard"))
			return
		}
	}
	if err := s.store.SetSetting(settingDashRepo, strconv.FormatInt(req.RepoID, 10)); err != nil {
		writeErr(w, 500, err)
		return
	}
	// Run soon after it's turned on or moved.
	_ = s.store.SetSetting(settingDashNext, "")
	if req.RepoID == 0 {
		s.resolve("dashboard-backup-failed", nil, nil)
	}
	s.audit(s.actor(r), "dashboard-backup-storage", fmt.Sprintf("dashboard backups go to storage #%d (0 = off)", req.RepoID))
	writeJSON(w, 200, s.dashboardBackupStatus())
}

func (s *Server) handleRunDashboardBackup(w http.ResponseWriter, r *http.Request) {
	if s.dashboardRepoID() == 0 {
		writeErr(w, 400, errors.New("choose a storage for the dashboard's backup first"))
		return
	}
	if dashboardRunning.Load() {
		writeErr(w, 409, errors.New("a dashboard backup is already running"))
		return
	}
	actor := s.actor(r)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		_ = s.backupDashboard(ctx, actor)
	}()
	writeJSON(w, 202, map[string]bool{"started": true})
}
