package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chmuzamil/backupproof/internal/engine"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// RestoredDashboard describes a dashboard rebuilt from its own backup.
type RestoredDashboard struct {
	SnapshotID    string
	Info          DashboardInfo
	Users         int
	Items         int
	LedgerEntries int
	SecretFromEnv bool
}

// RestoreDashboard rebuilds a dashboard's data folder from its newest backup
// in r (or the backup ref names), then checks the proof ledger it brought
// back. dataDir must not already hold a dashboard.
func RestoreDashboard(ctx context.Context, r *repo.Repo, dataDir, ref string) (*RestoredDashboard, error) {
	if _, err := os.Stat(filepath.Join(dataDir, "backupproof.db")); err == nil {
		return nil, fmt.Errorf("%s already holds a dashboard; restore into an empty folder (and stop the old one first)", dataDir)
	}
	snap, err := findDashboardBackup(ctx, r, ref)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "bp-dashboard-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if _, err := engine.Restore(ctx, r, snap.Snapshot, tmp, engine.RestoreOptions{}); err != nil {
		return nil, fmt.Errorf("restoring the dashboard backup: %w", err)
	}
	// The backup holds one folder with info.json at its top.
	var top string
	_ = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "info.json" && top == "" {
			top = filepath.Dir(p)
		}
		return nil
	})
	if top == "" {
		return nil, errors.New("this backup isn't a dashboard backup (no info.json)")
	}
	var info DashboardInfo
	raw, err := os.ReadFile(filepath.Join(top, "info.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("reading info.json: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	for _, f := range []string{"backupproof.db", "server.key", "secret.key", filepath.Join("builtin-agent", "agent.json"), filepath.Join("builtin-agent", "agent.key")} {
		data, err := os.ReadFile(filepath.Join(top, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(dataDir, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
	}
	out := &RestoredDashboard{SnapshotID: snap.ID.String(), Info: info, SecretFromEnv: info.SecretFromEnv}
	if info.SecretFromEnv && os.Getenv("BP_SECRET_KEY") == "" {
		// Without the secret the database's stored passwords can't be read,
		// but the files are in place: say what's needed, don't undo it.
		return out, nil
	}
	st, err := openExistingStore(dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	rows, err := st.Ledger(0, 0)
	if err != nil {
		return nil, err
	}
	entries := make([]proof.LedgerEntry, len(rows))
	for i, row := range rows {
		entries[i] = row.LedgerEntry
	}
	if err := proof.VerifyChain(entries); err != nil {
		return nil, fmt.Errorf("the restored proof ledger doesn't check out: %w", err)
	}
	out.LedgerEntries = len(entries)
	if users, err := st.Users(); err == nil {
		out.Users = len(users)
	}
	if items, err := st.Sources(); err == nil {
		out.Items = len(items)
	}
	return out, nil
}

func findDashboardBackup(ctx context.Context, r *repo.Repo, ref string) (snapshot.WithID, error) {
	if ref != "" && ref != "latest" {
		s, err := snapshot.Resolve(ctx, r, ref)
		if err != nil {
			return s, err
		}
		if s.Source.Name != DashboardSourceName {
			return s, fmt.Errorf("backup %s is of %q, not the dashboard", ref, s.Source.Name)
		}
		return s, nil
	}
	all, err := snapshot.List(ctx, r) // newest first
	if err != nil {
		return snapshot.WithID{}, err
	}
	for _, s := range all {
		if s.Source.Name == DashboardSourceName {
			return s, nil
		}
	}
	return snapshot.WithID{}, errors.New("this storage has no dashboard backup (turn it on under Settings → Dashboard backup)")
}

// DashboardBackups lists the dashboard backups in r, newest first.
func DashboardBackups(ctx context.Context, r *repo.Repo) ([]snapshot.WithID, error) {
	all, err := snapshot.List(ctx, r)
	if err != nil {
		return nil, err
	}
	var out []snapshot.WithID
	for _, s := range all {
		if strings.EqualFold(s.Source.Name, DashboardSourceName) {
			out = append(out, s)
		}
	}
	return out, nil
}
