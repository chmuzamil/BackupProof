package errreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

// Every secret-looking or identifying value here must be gone from the
// report that would be sent.
var secrets = []string{
	"hunter2", "s3cr3t-pass", "AKIAIOSFODNN7EXAMPLE", "0051234567890ab0000000001",
	"K005abcdefghijklmnopqrstuvwxyz12", "bpt_9f8e7d6c5b4a", "AGE-SECRET-KEY-1QQQ",
	"alice@example.com", "203.0.113.7", "2001:db8::1", "acme-backups", "mail.acme.com",
	"/var/lib/acme", `C:\Users\alice`, "payroll.sql", "MIIEvQIBADANBgkq",
}

const leaky = `open /var/lib/acme/payroll.sql: permission denied; password=hunter2 BP_PASSWORD: "s3cr3t-pass" ` +
	`AWS AKIAIOSFODNN7EXAMPLE keyID 0051234567890ab0000000001 appKey K005abcdefghijklmnopqrstuvwxyz12 ` +
	`Authorization: Bearer bpt_9f8e7d6c5b4a AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQ alice@example.com ` +
	`dial tcp 203.0.113.7:443 or [2001:db8::1]:22 s3://acme-backups/x?endpoint=https://s3.example.com ` +
	`lookup mail.acme.com C:\Users\alice\docs -----BEGIN PRIVATE KEY-----MIIEvQIBADANBgkq-----END PRIVATE KEY-----`

func start(t *testing.T) *sentry.MockTransport {
	t.Helper()
	mt := &sentry.MockTransport{}
	transport = mt
	t.Setenv("BP_SENTRY_DSN", "https://public@errors.invalid/1")
	flush, err := Init("server")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		flush()
		enabled.Store(false)
		transport = nil
		sentry.CurrentHub().BindClient(nil)
	})
	return mt
}

func sent(t *testing.T, mt *sentry.MockTransport) string {
	t.Helper()
	sentry.Flush(0)
	ev := mt.Events()
	if len(ev) != 1 {
		t.Fatalf("got %d events, want 1", len(ev))
	}
	b, err := json.Marshal(ev[0])
	if err != nil {
		t.Fatal(err)
	}
	var v any // re-encode without HTML escaping so "<redacted>" reads as written
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := json.Unmarshal(b, &v); err != nil || enc.Encode(v) != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("report contains %q:\n%s", s, out)
		}
	}
	for _, field := range []string{`"server_name"`, `"request"`, `"user":{"`, `"breadcrumbs"`, `"vars"`, `"abs_path"`} {
		if strings.Contains(out, field) {
			t.Errorf("report has %s:\n%s", field, out)
		}
	}
	return out
}

func TestOffWithoutDSN(t *testing.T) {
	t.Setenv("BP_SENTRY_DSN", "")
	if _, err := Init("server"); err != nil || Enabled() {
		t.Fatalf("Init without DSN: enabled=%v err=%v", Enabled(), err)
	}
	Error(errors.New("x")) // must not panic or send
}

func TestErrorIsScrubbed(t *testing.T) {
	mt := start(t)
	Error(fmt.Errorf("saving: %w", errors.New(leaky)))
	out := sent(t, mt)
	for _, want := range []string{"permission denied", "<redacted>", "<path>", "<ip>", "<email>", `"component":"server"`, `"filename":"errreport_test.go"`} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestHTTPPanicIsReportedAndAnswered(t *testing.T) {
	mt := start(t)
	h := RecoverHTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(leaky) }))
	req := httptest.NewRequest("POST", "/api/sources?token=hunter2", strings.NewReader(`{"password":"hunter2"}`))
	req.Header.Set("Cookie", "bp_session=hunter2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if out := sent(t, mt); !strings.Contains(out, `"type":"panic"`) {
		t.Errorf("not reported as a panic:\n%s", out)
	}
}

func TestBrowserError(t *testing.T) {
	mt := start(t)
	Browser(BrowserError{Type: "TypeError", Message: "x is undefined near " + leaky, Page: "#/sources/id/files?path=/var/lib/acme",
		Stack: "TypeError: x\n    at renderFiles (https://backup.acme.com/app.js?v=2:120:7)\n    at https://backup.acme.com/app.js:3900:3\nrenderJobs@https://backup.acme.com/app.js:55:1"})
	out := sent(t, mt)
	for _, want := range []string{`"filename":"app.js"`, `"function":"renderFiles"`, `"lineno":120`, `"page":"/sources/id/files"`, `"platform":"javascript"`, `"component":"browser"`} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %s:\n%s", want, out)
		}
	}
}

func TestScrubTextKeepsPlainMessages(t *testing.T) {
	for _, s := range []string{
		"runtime error: index out of range [3] with length 3",
		"invalid memory address or nil pointer dereference",
		"database is locked",
	} {
		if got := ScrubText(s); got != s {
			t.Errorf("ScrubText(%q) = %q", s, got)
		}
	}
}
