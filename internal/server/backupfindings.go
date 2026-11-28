package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chmuzamil/backupproof/internal/snapshot"
)

// What a successful backup can still report that a person should hear about:
// files it couldn't read or a container that didn't start again
// ("backup-warnings"), a retention or clean-up step that failed
// ("maintenance-failed"), and a backup that looks very different from the one
// before it ("backup-anomaly"), which is what files encrypted by ransomware
// look like.

// backupResult is the part of an agent's backup result read here.
type backupResult struct {
	Stats            snapshot.Stats `json:"stats"`
	Warnings         []string       `json:"warnings"`
	MaintenanceError string         `json:"maintenanceError"`
}

func parseBackupResult(v any) (backupResult, bool) {
	var r backupResult
	raw, err := json.Marshal(v)
	if err != nil || json.Unmarshal(raw, &r) != nil {
		return r, false
	}
	return r, true
}

// Anomaly thresholds. Small items are left alone: a 5 MB folder halving is
// normal life, not a warning sign.
const (
	anomalyMinBytes = 10 << 20
	anomalyMinFiles = 200
)

// anomaly compares a backup with the previous one of the same item.
func anomaly(prev, cur snapshot.Stats) string {
	var out []string
	if prev.Bytes >= anomalyMinBytes && cur.Bytes < prev.Bytes/2 {
		out = append(out, fmt.Sprintf("it holds %s, down from %s in the backup before", humanBytes(cur.Bytes), humanBytes(prev.Bytes)))
	}
	if prev.Files >= anomalyMinFiles && cur.Files < prev.Files/2 {
		out = append(out, fmt.Sprintf("it has %d files, down from %d", cur.Files, prev.Files))
	}
	// Mass change: the item is normally mostly unchanged between backups,
	// and this time nearly every file changed.
	if cur.Files >= anomalyMinFiles && prev.Files >= anomalyMinFiles && prev.Unchanged*2 >= prev.Files {
		if changed := cur.Files - cur.Unchanged; changed*10 >= cur.Files*8 {
			out = append(out, fmt.Sprintf("%d of its %d files changed since the backup before, when usually most stay the same", changed, cur.Files))
		}
	}
	return strings.Join(out, "; ")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// backupFindings raises or resolves the alerts a successful backup can lead
// to. prev is the item's previous successful backup job, if any.
func (s *Server) backupFindings(sid int64, prev *Job, result any) {
	cur, ok := parseBackupResult(result)
	if !ok {
		return
	}
	name := "This item"
	if src, err := s.store.Source(sid); err == nil {
		name = src.Name
	}
	if len(cur.Warnings) > 0 {
		s.alert("backup-warnings", &sid, nil, fmt.Sprintf("%s: the backup was saved, but %s", name, joinWarnings(cur.Warnings)))
	} else {
		s.resolve("backup-warnings", &sid, nil)
	}
	if cur.MaintenanceError != "" {
		s.alert("maintenance-failed", &sid, nil, fmt.Sprintf("%s: old backups couldn't be cleaned up, so the storage keeps growing: %s", name, cur.MaintenanceError))
	} else {
		s.resolve("maintenance-failed", &sid, nil)
	}
	// An anomaly isn't resolved by the next backup: if files were encrypted,
	// the next backup looks normal compared with this one. A person dismisses it.
	if prev != nil {
		if p, ok := parseBackupResult(json.RawMessage(prev.Result)); ok {
			if what := anomaly(p.Stats, cur.Stats); what != "" {
				s.alert("backup-anomaly", &sid, nil, fmt.Sprintf("%s: the newest backup looks very different from the one before: %s. If you didn't expect this, check the files before trusting this backup; an older backup is still there to restore.", name, what))
			}
		}
	}
}

func joinWarnings(w []string) string {
	if len(w) == 1 {
		return lowerFirst(w[0])
	}
	parts := make([]string, len(w))
	for i, x := range w {
		parts[i] = fmt.Sprintf("(%d) %s", i+1, lowerFirst(x))
	}
	return strings.Join(parts, " ")
}

func lowerFirst(s string) string {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return s
	}
	// Keep names that start with capitals (macOS, Outlook) as they are.
	if len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z' {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// dismissable are alerts a person closes by saying the change was expected.
var dismissable = map[string]bool{"backup-anomaly": true}

// handleDismissAlert closes an open alert that nothing else would close.
func (s *Server) handleDismissAlert(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a, err := s.store.Alert(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if !dismissable[a.Kind] || a.Resolved != nil {
		writeErr(w, 400, errors.New("this alert closes by itself once the problem is fixed"))
		return
	}
	s.resolve(a.Kind, a.SourceID, a.AgentID)
	s.audit(s.actor(r), "dismiss-alert", fmt.Sprintf("alert #%d (%s): %s", id, a.Kind, a.Message))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
