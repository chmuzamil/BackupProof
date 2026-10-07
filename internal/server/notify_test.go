package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every channel gets the alert in its own format; PagerDuty opens an
// incident and resolves the same one when the problem is fixed.
func TestNotifyChannels(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]*http.Request{}
	bodies := map[string][]string{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = append(got[r.URL.Path], r)
		bodies[r.URL.Path] = append(bodies[r.URL.Path], string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer fake.Close()
	telegramAPI, pushoverAPI, pagerDutyAPI = fake.URL, fake.URL, fake.URL

	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	n := NotifySettings{
		WebhookURL: fake.URL + "/slack", TeamsURL: fake.URL + "/teams",
		TelegramToken: "123:ABC", TelegramChatID: "-100",
		NtfyURL: fake.URL + "/ntfy/backups", NtfyToken: "tk_ntfy",
		GotifyURL: fake.URL + "/gotify", GotifyToken: "gotify-app",
		PushoverToken: "po-app", PushoverUser: "po-user", PagerDutyKey: "pd-key",
	}
	b, _ := json.Marshal(n)
	if err := srv.store.SetSetting("notify", string(b)); err != nil {
		t.Fatal(err)
	}
	agent := int64(7)
	if err := srv.notify(event{Subject: "BackupProof: server offline", Body: "web-01 is quiet", Key: alertKey("agent-offline", nil, &agent), Urgent: true}); err != nil {
		t.Fatal(err)
	}
	if err := srv.notify(event{Subject: "BackupProof resolved: server offline", Body: "fixed", Key: alertKey("agent-offline", nil, &agent), Resolved: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/slack", "/teams", "/bot123:ABC/sendMessage", "/ntfy/backups", "/gotify/message", "/1/messages.json", "/v2/enqueue"} {
		if len(got[path]) != 2 {
			t.Errorf("%s: %d requests, want 2", path, len(got[path]))
		}
	}
	if h := got["/ntfy/backups"][0]; h.Header.Get("Authorization") != "Bearer tk_ntfy" || h.Header.Get("Priority") != "high" {
		t.Errorf("ntfy headers: %v", h.Header)
	}
	if got["/gotify/message"][0].Header.Get("X-Gotify-Key") != "gotify-app" {
		t.Error("gotify key header missing")
	}
	if !strings.Contains(bodies["/teams"][0], "application/vnd.microsoft.card.adaptive") {
		t.Errorf("teams payload: %s", bodies["/teams"][0])
	}
	if !strings.Contains(bodies["/1/messages.json"][0], "user=po-user") {
		t.Errorf("pushover payload: %s", bodies["/1/messages.json"][0])
	}
	var trig, res map[string]any
	_ = json.Unmarshal([]byte(bodies["/v2/enqueue"][0]), &trig)
	_ = json.Unmarshal([]byte(bodies["/v2/enqueue"][1]), &res)
	if trig["event_action"] != "trigger" || res["event_action"] != "resolve" || trig["dedup_key"] != res["dedup_key"] || trig["routing_key"] != "pd-key" {
		t.Errorf("pagerduty: trigger %v, resolve %v", trig, res)
	}
}

func TestWeeklyReportSchedule(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	now := time.Date(2026, 10, 12, 8, 30, 0, 0, time.Local) // a Monday
	b, _ := json.Marshal(NotifySettings{WeeklyReport: true, ReportDay: 1, ReportHour: 8})
	_ = srv.store.SetSetting("notify", string(b))
	srv.maybeSendWeeklyReport(now) // no email configured: logs, but marks the week as handled
	if srv.store.Setting("report_sent") != "2026-10-12" {
		t.Fatalf("report not attempted on its day and hour")
	}
	_ = srv.store.SetSetting("report_sent", "")
	srv.maybeSendWeeklyReport(now.Add(time.Hour)) // 9:00: wrong hour
	if srv.store.Setting("report_sent") != "" {
		t.Error("report sent at the wrong hour")
	}
	rep, err := srv.buildWeeklyReport(now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.subject(), "nothing is protected yet") || !strings.Contains(rep.html("https://x"), "Open the dashboard") {
		t.Errorf("report: %q", rep.subject())
	}
}

// A saved password or token is kept when left empty, but not when its
// destination changes, so it can't be sent to a server someone else picked.
func TestSavedSecretsStayWithTheirDestination(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "s"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	b, _ := json.Marshal(NotifySettings{SMTPHost: "mail.example.com", SMTPPort: 587, SMTPPass: "smtp-secret", GotifyURL: "https://gotify.example.com", GotifyToken: "g-secret", PagerDutyKey: "pd-secret"})
	_ = srv.store.SetSetting("notify", string(b))
	put := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		srv.handlePutNotify(w, httptest.NewRequest("PUT", "/api/settings/notify", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body)
		}
	}
	put(`{"smtpHost":"mail.example.com","smtpPort":587,"gotifyUrl":"https://gotify.example.com"}`)
	if n := srv.notifySettings(); n.SMTPPass != "smtp-secret" || n.GotifyToken != "g-secret" || n.PagerDutyKey != "pd-secret" {
		t.Fatalf("unchanged destinations lost their secrets: %+v", n)
	}
	put(`{"smtpHost":"attacker.example","smtpPort":587,"gotifyUrl":"https://attacker.example"}`)
	if n := srv.notifySettings(); n.SMTPPass != "" || n.GotifyToken != "" || n.PagerDutyKey != "pd-secret" {
		t.Fatalf("secrets followed a changed destination: %+v", n)
	}
}
