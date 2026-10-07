package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// NotifySettings are stored as JSON in the settings table. Fields marked
// secret are never sent back to the browser; saving an empty value keeps the
// stored one.
type NotifySettings struct {
	// Email
	SMTPHost string `json:"smtpHost,omitempty"`
	SMTPPort int    `json:"smtpPort,omitempty"`
	SMTPUser string `json:"smtpUser,omitempty"`
	SMTPPass string `json:"smtpPass,omitempty"` // secret
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"` // comma separated

	// Slack, Discord, Mattermost or any webhook that accepts {"text": …}
	WebhookURL string `json:"webhookUrl,omitempty"`
	// Microsoft Teams: a Workflows "When a Teams webhook request is received" URL.
	TeamsURL string `json:"teamsUrl,omitempty"`
	// Telegram: a bot token from @BotFather and the chat to post in.
	TelegramToken  string `json:"telegramToken,omitempty"` // secret
	TelegramChatID string `json:"telegramChatId,omitempty"`
	// ntfy: a topic URL such as https://ntfy.sh/my-backups, optional access token.
	NtfyURL   string `json:"ntfyUrl,omitempty"`
	NtfyToken string `json:"ntfyToken,omitempty"` // secret
	// Gotify: the server address and an application token.
	GotifyURL   string `json:"gotifyUrl,omitempty"`
	GotifyToken string `json:"gotifyToken,omitempty"` // secret
	// Pushover: the application token and the user (or group) key.
	PushoverToken string `json:"pushoverToken,omitempty"` // secret
	PushoverUser  string `json:"pushoverUser,omitempty"`  // secret
	// PagerDuty Events API v2 routing (integration) key. Problems open an
	// incident; when they're fixed the incident is resolved.
	PagerDutyKey string `json:"pagerDutyKey,omitempty"` // secret

	// HeartbeatURL is pinged after every successful tick (healthchecks.io /
	// Uptime Kuma push), so a dead BackupProof server is itself detected.
	HeartbeatURL string `json:"heartbeatUrl,omitempty"`

	// Weekly summary by email: the day (0 = Sunday) and hour, in the server's time zone.
	WeeklyReport bool `json:"weeklyReport,omitempty"`
	ReportDay    int  `json:"reportDay,omitempty"`
	ReportHour   int  `json:"reportHour,omitempty"`
}

// secretFields lets the settings form show "saved" without revealing values.
func (n *NotifySettings) secretFields() map[string]*string {
	return map[string]*string{
		"smtpPass": &n.SMTPPass, "telegramToken": &n.TelegramToken, "ntfyToken": &n.NtfyToken,
		"gotifyToken": &n.GotifyToken, "pushoverToken": &n.PushoverToken, "pushoverUser": &n.PushoverUser, "pagerDutyKey": &n.PagerDutyKey,
	}
}

func (s *Server) notifySettings() NotifySettings {
	var n NotifySettings
	raw := s.store.Setting("notify")
	if raw == "" {
		return n
	}
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		s.log.Printf("warning: ignoring unreadable notification settings: %v", err)
		return NotifySettings{}
	}
	return n
}

// event is one notification: a new problem, a fixed one, a test or a report.
type event struct {
	Subject  string
	Body     string
	HTML     string // optional HTML body for email
	Key      string // stable per problem, so PagerDuty can resolve what it opened
	Resolved bool
	Urgent   bool
}

func alertKey(kind string, sourceID, agentID *int64) string {
	k := "backupproof:" + kind
	if sourceID != nil {
		k += ":item:" + strconv.FormatInt(*sourceID, 10)
	}
	if agentID != nil {
		k += ":server:" + strconv.FormatInt(*agentID, 10)
	}
	return k
}

var alertWords = map[string]string{
	"backup-failed": "backup failed", "drill-failed": "restore test failed", "job-failed": "job failed",
	"backup-overdue": "backup overdue", "proof-stale": "restore test overdue", "agent-offline": "server offline",
}

func (s *Server) alert(kind string, sourceID, agentID *int64, msg string) {
	if s.store.RaiseAlert(kind, sourceID, agentID, msg) {
		s.log.Printf("ALERT %s: %s", kind, msg)
		what := alertWords[kind]
		if what == "" {
			what = kind
		}
		ev := event{Subject: "BackupProof: " + what, Body: msg + s.dashboardLine(), Key: alertKey(kind, sourceID, agentID), Urgent: true}
		go func() { _ = s.notify(ev) }() // notify logs its own failures
	}
}

func (s *Server) resolve(kind string, sourceID, agentID *int64) {
	if s.store.ResolveAlerts(kind, sourceID, agentID) > 0 {
		name := ""
		if sourceID != nil {
			if src, err := s.store.Source(*sourceID); err == nil {
				name = src.Name
			}
		} else if agentID != nil {
			if a, err := s.store.Agent(*agentID); err == nil {
				name = "the server " + a.Name
			}
		}
		what := alertWords[kind]
		if what == "" {
			what = kind
		}
		msg := "Fixed: " + what
		if name != "" {
			msg = name + ": " + what + " — fixed, everything is working again."
		}
		ev := event{Subject: "BackupProof resolved: " + what, Body: msg, Key: alertKey(kind, sourceID, agentID), Resolved: true}
		go func() { _ = s.notify(ev) }()
	}
}

func (s *Server) dashboardLine() string {
	if u := s.publicURL(); u != "" && !strings.Contains(u, "127.0.0.1") && !strings.Contains(u, "localhost") {
		return "\n\nOpen the dashboard: " + u
	}
	return ""
}

// send is the simple form used by tests and reports.
func (s *Server) send(subject, body string) error {
	return s.notify(event{Subject: subject, Body: body})
}

// notify delivers ev to every configured channel and reports which failed.
func (s *Server) notify(ev event) error {
	n := s.notifySettings()
	type channel struct {
		name string
		on   bool
		fn   func() error
	}
	text := ev.Subject + "\n" + ev.Body
	channels := []channel{
		{"email", n.SMTPHost != "" && n.To != "", func() error { return sendEmail(n, ev) }},
		{"webhook", n.WebhookURL != "", func() error {
			return postJSON(n.WebhookURL, map[string]string{"text": text, "content": text}, nil)
		}},
		{"Microsoft Teams", n.TeamsURL != "", func() error { return postJSON(n.TeamsURL, teamsCard(ev), nil) }},
		{"Telegram", n.TelegramToken != "" && n.TelegramChatID != "", func() error {
			return postJSON(telegramAPI+"/bot"+n.TelegramToken+"/sendMessage",
				map[string]any{"chat_id": n.TelegramChatID, "text": text, "disable_web_page_preview": true}, nil)
		}},
		{"ntfy", n.NtfyURL != "", func() error {
			h := map[string]string{"Title": ev.Subject, "Tags": "floppy_disk"}
			if ev.Urgent {
				h["Priority"], h["Tags"] = "high", "warning"
			} else if ev.Resolved {
				h["Tags"] = "white_check_mark"
			}
			if n.NtfyToken != "" {
				h["Authorization"] = "Bearer " + n.NtfyToken
			}
			return post(n.NtfyURL, "text/plain; charset=utf-8", strings.NewReader(ev.Body), h)
		}},
		{"Gotify", n.GotifyURL != "" && n.GotifyToken != "", func() error {
			prio := 5
			if ev.Urgent {
				prio = 8
			}
			return postJSON(strings.TrimRight(n.GotifyURL, "/")+"/message", map[string]any{"title": ev.Subject, "message": ev.Body, "priority": prio},
				map[string]string{"X-Gotify-Key": n.GotifyToken})
		}},
		{"Pushover", n.PushoverToken != "" && n.PushoverUser != "", func() error {
			form := url.Values{"token": {n.PushoverToken}, "user": {n.PushoverUser}, "title": {ev.Subject}, "message": {ev.Body}}
			if ev.Urgent {
				form.Set("priority", "1")
			}
			return post(pushoverAPI+"/1/messages.json", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
		}},
		{"PagerDuty", n.PagerDutyKey != "" && (ev.Key != "" || !ev.Resolved), func() error {
			key := ev.Key
			if key == "" {
				key = "backupproof:" + ev.Subject
			}
			action := "trigger"
			if ev.Resolved {
				action = "resolve"
			}
			body := map[string]any{"routing_key": n.PagerDutyKey, "event_action": action, "dedup_key": key}
			if !ev.Resolved {
				sev := "info"
				if ev.Urgent {
					sev = "error"
				}
				body["payload"] = map[string]any{"summary": truncate(ev.Subject+": "+firstLine(ev.Body), 1000), "source": "BackupProof", "severity": sev}
			}
			return postJSON(pagerDutyAPI+"/v2/enqueue", body, nil)
		}},
	}
	var errs []string
	for _, c := range channels {
		if !c.on {
			continue
		}
		if err := c.fn(); err != nil {
			errs = append(errs, c.name+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		s.log.Printf("notification failed: %s", strings.Join(errs, "; "))
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var notifyClient = &http.Client{Timeout: 15 * time.Second}

// Service addresses (variables so tests can point them at a fake server).
var (
	telegramAPI  = "https://api.telegram.org"
	pushoverAPI  = "https://api.pushover.net"
	pagerDutyAPI = "https://events.pagerduty.com"
)

func post(u, contentType string, body io.Reader, headers map[string]string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := notifyClient.Do(req)
	if err != nil {
		return redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// redact keeps tokens that live in URLs (Telegram) out of logs and messages.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s request failed: %v", ue.Op, ue.Err)
	}
	return err
}

func postJSON(u string, v any, headers map[string]string) error {
	b, _ := json.Marshal(v)
	return post(u, "application/json", bytes.NewReader(b), headers)
}

// teamsCard is the Adaptive Card payload Teams Workflows webhooks expect.
func teamsCard(ev event) map[string]any {
	color := "Default"
	if ev.Urgent {
		color = "Attention"
	} else if ev.Resolved {
		color = "Good"
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4",
				"body": []any{
					map[string]any{"type": "TextBlock", "text": ev.Subject, "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
					map[string]any{"type": "TextBlock", "text": ev.Body, "wrap": true},
				},
			},
		}},
	}
}

func sendEmail(n NotifySettings, ev event) error {
	port := n.SMTPPort
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if n.SMTPUser != "" {
		auth = smtp.PlainAuth("", n.SMTPUser, n.SMTPPass, n.SMTPHost)
	}
	to := strings.Split(n.To, ",")
	for i := range to {
		to[i] = strings.TrimSpace(to[i])
	}
	clean := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	head := "From: " + clean(n.From) + "\r\nTo: " + clean(strings.Join(to, ", ")) + "\r\nSubject: " + clean(ev.Subject) +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\n"
	var msg string
	if ev.HTML == "" {
		msg = head + "Content-Type: text/plain; charset=utf-8\r\n\r\n" + ev.Body + "\r\n"
	} else {
		b := "bp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		msg = head + "Content-Type: multipart/alternative; boundary=" + b + "\r\n\r\n" +
			"--" + b + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + ev.Body + "\r\n" +
			"--" + b + "\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + ev.HTML + "\r\n--" + b + "--\r\n"
	}
	return smtp.SendMail(fmt.Sprintf("%s:%d", n.SMTPHost, port), auth, n.From, to, []byte(msg))
}

func (s *Server) heartbeat() {
	if u := s.notifySettings().HeartbeatURL; u != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}
