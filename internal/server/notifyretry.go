package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Alerts must reach someone even when a channel is briefly down: a failed
// delivery is kept in an outbox and retried with backoff for a day, each
// channel's last outcome is shown in Settings (and on the overview when it's
// failing), and a problem still open a day later is sent again.

var notifyMigrations = []string{
	`CREATE TABLE IF NOT EXISTS notify_outbox (
  id INTEGER PRIMARY KEY, event TEXT NOT NULL, channels TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, next TEXT NOT NULL, first TEXT NOT NULL)`,
	"ALTER TABLE alerts ADD COLUMN reminded TEXT",
}

// retryBackoff is the wait before each retry; the last repeats until a day has passed.
var retryBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour}

const (
	retryFor    = 24 * time.Hour
	remindAfter = 24 * time.Hour
)

// storedEvent is an event as kept in the outbox.
type storedEvent struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	HTML     string `json:"html,omitempty"`
	Key      string `json:"key,omitempty"`
	Resolved bool   `json:"resolved,omitempty"`
	Urgent   bool   `json:"urgent,omitempty"`
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// notifyOrQueue delivers an alert and keeps what failed for a retry.
func (s *Server) notifyOrQueue(ev event) {
	failed := s.deliver(ev, nil)
	if len(failed) == 0 {
		return
	}
	raw, _ := json.Marshal(storedEvent(ev))
	chans, _ := json.Marshal(sortedKeys(failed))
	t := time.Now().UTC()
	if _, err := s.store.db.Exec("INSERT INTO notify_outbox(event,channels,next,first) VALUES(?,?,?,?)",
		string(raw), string(chans), t.Add(retryBackoff[0]).Format(time.RFC3339Nano), t.Format(time.RFC3339Nano)); err != nil {
		s.log.Printf("could not keep a failed notification for a retry: %v", err)
	}
}

// retryNotifications resends what is due in the outbox.
func (s *Server) retryNotifications(now time.Time) {
	type row struct {
		id       int64
		ev       storedEvent
		chans    []string
		attempts int
		first    time.Time
	}
	rows, err := s.store.db.Query("SELECT id,event,channels,attempts,first FROM notify_outbox WHERE next<=? ORDER BY id LIMIT 50", now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	var due []row
	for rows.Next() {
		var r row
		var evRaw, chRaw, first string
		if rows.Scan(&r.id, &evRaw, &chRaw, &r.attempts, &first) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(evRaw), &r.ev)
		_ = json.Unmarshal([]byte(chRaw), &r.chans)
		r.first, _ = time.Parse(time.RFC3339Nano, first)
		due = append(due, r)
	}
	rows.Close()
	for _, r := range due {
		only := map[string]bool{}
		for _, c := range r.chans {
			only[c] = true
		}
		failed := s.deliver(event(r.ev), only)
		if len(failed) == 0 {
			s.dropOutbox(r.id)
			continue
		}
		if now.Sub(r.first) > retryFor {
			s.log.Printf("gave up delivering %q to %s after a day of retries", r.ev.Subject, strings.Join(sortedKeys(failed), ", "))
			s.dropOutbox(r.id)
			continue
		}
		wait := retryBackoff[min(r.attempts+1, len(retryBackoff)-1)]
		chans, _ := json.Marshal(sortedKeys(failed))
		if _, err := s.store.db.Exec("UPDATE notify_outbox SET channels=?, attempts=attempts+1, next=? WHERE id=?",
			string(chans), now.Add(wait).UTC().Format(time.RFC3339Nano), r.id); err != nil {
			s.log.Printf("could not reschedule a failed notification: %v", err)
		}
	}
}

func (s *Server) dropOutbox(id int64) {
	if _, err := s.store.db.Exec("DELETE FROM notify_outbox WHERE id=?", id); err != nil {
		s.log.Printf("could not remove a sent notification from the outbox: %v", err)
	}
}

// remindOpenAlerts sends again, once a day, every problem that is still open.
func (s *Server) remindOpenAlerts(now time.Time) {
	cut := now.Add(-remindAfter).UTC().Format(time.RFC3339Nano)
	rows, err := s.store.db.Query(`SELECT id,source_id,agent_id,kind,message,created FROM alerts
  WHERE resolved IS NULL AND created<=? AND (reminded IS NULL OR reminded<=?) ORDER BY id`, cut, cut)
	if err != nil {
		return
	}
	var due []Alert
	for rows.Next() {
		var a Alert
		var sid, aid *int64
		if rows.Scan(&a.ID, &sid, &aid, &a.Kind, &a.Message, &a.Created) == nil {
			a.SourceID, a.AgentID = sid, aid
			due = append(due, a)
		}
	}
	rows.Close()
	for _, a := range due {
		// Recorded first: if it can't be, no reminder, rather than one a minute.
		if _, err := s.store.db.Exec("UPDATE alerts SET reminded=? WHERE id=?", now.UTC().Format(time.RFC3339Nano), a.ID); err != nil {
			s.log.Printf("could not record an alert reminder: %v", err)
			continue
		}
		what := alertWords[a.Kind]
		if what == "" {
			what = a.Kind
		}
		since, _ := time.Parse(time.RFC3339Nano, a.Created)
		ev := event{
			Subject: "BackupProof: still not fixed: " + what,
			Body:    fmt.Sprintf("%s\n\nOpen since %s.%s", a.Message, since.Local().Format("Jan 2, 15:04"), s.dashboardLine()),
			Key:     alertKey(a.Kind, a.SourceID, a.AgentID), Urgent: true,
		}
		go s.notifyOrQueue(ev)
	}
}

// channelStatus is a channel's most recent delivery outcome.
type channelStatus struct {
	LastOK      string `json:"lastOk,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	LastErrorAt string `json:"lastErrorAt,omitempty"`
}

// failing reports whether the channel's newest outcome is a failure.
func (c channelStatus) failing() bool { return c.LastErrorAt != "" && c.LastErrorAt > c.LastOK }

var deliveryMu sync.Mutex

func (s *Server) recordDelivery(name string, err error) {
	deliveryMu.Lock()
	defer deliveryMu.Unlock()
	st := s.deliveryStatus()
	c := st[name]
	t := now()
	if err == nil {
		c.LastOK = t
	} else {
		c.LastError, c.LastErrorAt = err.Error(), t
	}
	st[name] = c
	raw, _ := json.Marshal(st)
	if e := s.store.SetSetting("notify-status", string(raw)); e != nil {
		s.log.Printf("could not save notification status: %v", e)
	}
}

func (s *Server) deliveryStatus() map[string]channelStatus {
	st := map[string]channelStatus{}
	if raw := s.store.Setting("notify-status"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	return st
}

// failingChannels lists configured channels whose last delivery failed.
func (s *Server) failingChannels() []map[string]string {
	n := s.notifySettings()
	on := map[string]bool{}
	for _, c := range channels(n, event{}) {
		on[c.name] = c.on
	}
	var out []map[string]string
	st := s.deliveryStatus()
	for _, name := range sortedKeys(st) {
		if c := st[name]; on[name] && c.failing() {
			out = append(out, map[string]string{"channel": name, "error": c.LastError, "at": c.LastErrorAt})
		}
	}
	return out
}

// notice is a setup problem shown on the home page: nothing has failed yet,
// but when something does, nobody would hear about it.
type notice struct {
	Text string `json:"text"`
	Href string `json:"href,omitempty"`
}

func (s *Server) notices() []notice {
	var out []notice
	n := s.notifySettings()
	configured := false
	for _, c := range channels(n, event{}) {
		configured = configured || c.on
	}
	if !configured {
		out = append(out, notice{Text: "No alert channel is set up, so nobody hears when a backup or restore test fails. Add email or a chat app in Settings → Alerts.", Href: "#/settings"})
	}
	for _, c := range s.failingChannels() {
		out = append(out, notice{Text: fmt.Sprintf("Alerts aren't reaching you by %s: %s. Fix it in Settings → Alerts; failed alerts are retried for a day.", c["channel"], c["error"]), Href: "#/settings"})
	}
	if n.HeartbeatURL == "" {
		out = append(out, notice{Text: "Nothing checks that this dashboard is running. It starts every backup, so if it stops, all backups stop. Add a heartbeat address (healthchecks.io, Uptime Kuma) in Settings → Alerts so you hear about it.", Href: "#/settings"})
	}
	return out
}
