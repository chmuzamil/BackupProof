package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// NotifySettings are stored as JSON in the settings table.
type NotifySettings struct {
	WebhookURL string `json:"webhookUrl,omitempty"` // Slack/Discord/Mattermost-compatible JSON {"text": ...}
	SMTPHost   string `json:"smtpHost,omitempty"`
	SMTPPort   int    `json:"smtpPort,omitempty"`
	SMTPUser   string `json:"smtpUser,omitempty"`
	SMTPPass   string `json:"smtpPass,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"` // comma separated
	// HeartbeatURL is pinged after every successful tick (healthchecks.io /
	// Uptime Kuma push), so a dead BackupProof server is itself detected.
	HeartbeatURL string `json:"heartbeatUrl,omitempty"`
}

func (s *Server) notifySettings() NotifySettings {
	var n NotifySettings
	json.Unmarshal([]byte(s.store.Setting("notify")), &n)
	return n
}

func (s *Server) alert(kind string, sourceID, agentID *int64, msg string) {
	if s.store.RaiseAlert(kind, sourceID, agentID, msg) {
		s.log.Printf("ALERT %s: %s", kind, msg)
		go s.send("BackupProof alert: "+kind, msg)
	}
}

func (s *Server) resolve(kind string, sourceID, agentID *int64) {
	if s.store.ResolveAlerts(kind, sourceID, agentID) > 0 {
		go s.send("BackupProof resolved: "+kind, "Resolved: "+kind)
	}
}

func (s *Server) send(subject, body string) error {
	n := s.notifySettings()
	var errs []string
	if n.WebhookURL != "" {
		payload, _ := json.Marshal(map[string]string{"text": subject + "\n" + body, "content": subject + "\n" + body})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			errs = append(errs, "webhook: "+err.Error())
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				errs = append(errs, fmt.Sprintf("webhook: HTTP %d", resp.StatusCode))
			}
		}
	}
	if n.SMTPHost != "" && n.To != "" {
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
		msg := "From: " + n.From + "\r\nTo: " + strings.Join(to, ", ") + "\r\nSubject: " + subject +
			"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
		if err := smtp.SendMail(fmt.Sprintf("%s:%d", n.SMTPHost, port), auth, n.From, to, []byte(msg)); err != nil {
			errs = append(errs, "smtp: "+err.Error())
		}
	}
	if len(errs) > 0 {
		s.log.Printf("notification failed: %s", strings.Join(errs, "; "))
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
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
