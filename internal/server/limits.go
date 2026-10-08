package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// Per-server limits: how fast backups may upload and restores download, and
// a daily time window for scheduled jobs (manual runs always start at once).

var limitMigrations = []string{
	"ALTER TABLE agents ADD COLUMN upload_kbps INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE agents ADD COLUMN download_kbps INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE agents ADD COLUMN window_start TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE agents ADD COLUMN window_end TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE agents ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE agents ADD COLUMN max_inflight_mb INTEGER NOT NULL DEFAULT 0",
	// A removed server is hidden from the dashboard; its key is kept so the
	// proofs it signed can still be verified.
	"ALTER TABLE agents ADD COLUMN removed INTEGER NOT NULL DEFAULT 0",
}

// Limits are kilobytes per second (0 = no limit) and "HH:MM" times in the
// dashboard server's time zone ("" = any time).
type Limits struct {
	UploadKBps   int    `json:"uploadKBps"`
	DownloadKBps int    `json:"downloadKBps"`
	WindowStart  string `json:"windowStart"`
	WindowEnd    string `json:"windowEnd"`
	// Advanced: storage requests at once (0 adapts, 1 is one at a time) and
	// MiB of chunk data held in memory per transfer (0 is 64).
	Concurrency   int `json:"concurrency"`
	MaxInflightMB int `json:"maxInflightMB"`
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (l Limits) validate() error {
	if l.UploadKBps < 0 || l.DownloadKBps < 0 || l.UploadKBps > 10_000_000 || l.DownloadKBps > 10_000_000 {
		return errors.New("speed limits must be between 0 (no limit) and 10,000,000 KB/s")
	}
	if l.Concurrency < 0 || l.Concurrency > 64 {
		return errors.New("parallel transfers must be between 0 (automatic) and 64")
	}
	if l.MaxInflightMB != 0 && (l.MaxInflightMB < 16 || l.MaxInflightMB > 4096) {
		return errors.New("memory for transfers must be between 16 and 4096 MB, or 0 for the default")
	}
	if (l.WindowStart == "") != (l.WindowEnd == "") {
		return errors.New("give both a start and an end time for the window, or neither")
	}
	if l.WindowStart != "" && (!hhmm.MatchString(l.WindowStart) || !hhmm.MatchString(l.WindowEnd)) {
		return errors.New("window times look like 22:00")
	}
	if l.WindowStart != "" && l.WindowStart == l.WindowEnd {
		return errors.New("the window's start and end can't be the same time")
	}
	return nil
}

// open reports whether t (server local time) is inside the window. Windows
// may cross midnight, such as 22:00–06:00.
func (l Limits) open(t time.Time) bool {
	if l.WindowStart == "" {
		return true
	}
	cur := t.Local().Format("15:04")
	if l.WindowStart < l.WindowEnd {
		return cur >= l.WindowStart && cur < l.WindowEnd
	}
	return cur >= l.WindowStart || cur < l.WindowEnd
}

func (s *Store) AgentLimits(id int64) (Limits, error) {
	var l Limits
	err := s.db.QueryRow("SELECT upload_kbps,download_kbps,window_start,window_end,concurrency,max_inflight_mb FROM agents WHERE id=?", id).
		Scan(&l.UploadKBps, &l.DownloadKBps, &l.WindowStart, &l.WindowEnd, &l.Concurrency, &l.MaxInflightMB)
	return l, err
}

func (s *Store) SetAgentLimits(id int64, l Limits) error {
	res, err := s.db.Exec("UPDATE agents SET upload_kbps=?,download_kbps=?,window_start=?,window_end=?,concurrency=?,max_inflight_mb=? WHERE id=?",
		l.UploadKBps, l.DownloadKBps, l.WindowStart, l.WindowEnd, l.Concurrency, l.MaxInflightMB, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("server %d not found", id)
	}
	return nil
}

func (s *Server) handlePutLimits(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var l Limits
	if err := readJSON(r, &l); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := l.validate(); err != nil {
		writeErr(w, 400, err)
		return
	}
	a, err := s.store.Agent(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := s.store.SetAgentLimits(id, l); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "update-limits", fmt.Sprintf("server %q: upload %d KB/s, download %d KB/s, window %q–%q (0 or empty = none)",
		a.Name, l.UploadKBps, l.DownloadKBps, l.WindowStart, l.WindowEnd))
	writeJSON(w, 200, l)
}
