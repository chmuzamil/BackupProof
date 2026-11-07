// Package errreport sends crash reports to a Sentry-compatible service
// (Sentry, GlitchTip) when, and only when, the operator sets BP_SENTRY_DSN.
// It is off by default and BackupProof ships with no DSN.
//
// BackupProof handles repository passwords, storage credentials and other
// people's files, so reports carry as little as possible: an error's type,
// a scrubbed message and a code stack trace. No request data, user, host
// name, breadcrumbs or local variables are ever sent; message text has
// URLs, paths, host names, addresses and anything that looks like a secret
// replaced before it leaves the process. There is no tracing or replay.
package errreport

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/chmuzamil/backupproof/internal/engine"
)

var enabled atomic.Bool

// transport replaces the network transport in tests.
var transport sentry.Transport

// Enabled reports whether Init found a DSN and reports are being sent.
func Enabled() bool { return enabled.Load() }

// Init turns reporting on if BP_SENTRY_DSN is set. component ("server",
// "agent") is attached to every report. The returned function flushes
// pending reports and should run before the process exits.
func Init(component string) (flush func(), err error) {
	dsn := strings.TrimSpace(os.Getenv("BP_SENTRY_DSN"))
	if dsn == "" {
		return func() {}, nil
	}
	env := os.Getenv("BP_SENTRY_ENVIRONMENT")
	if env == "" {
		env = "production"
	}
	err = sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Transport:        transport,
		Release:          "backupproof@" + strings.TrimPrefix(engine.Version, "backupproof/"),
		Environment:      env,
		ServerName:       "-", // never the host name: it often names the customer
		TracesSampleRate: 0,
		MaxBreadcrumbs:   -1,
		BeforeSend:       func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event { return Scrub(e) },
		BeforeBreadcrumb: func(*sentry.Breadcrumb, *sentry.BreadcrumbHint) *sentry.Breadcrumb { return nil },
		// Only the integration that adds OS, architecture and Go version.
		Integrations: func(in []sentry.Integration) []sentry.Integration {
			var out []sentry.Integration
			for _, i := range in {
				if i.Name() == "Environment" {
					out = append(out, i)
				}
			}
			return out
		},
	})
	if err != nil {
		return func() {}, fmt.Errorf("error reporting (BP_SENTRY_DSN): %w", err)
	}
	sentry.ConfigureScope(func(s *sentry.Scope) { s.SetTag("component", component) })
	enabled.Store(true)
	return func() { sentry.Flush(5 * time.Second) }, nil
}

// Error reports an unexpected internal error, with the caller's stack.
func Error(err error) {
	if !enabled.Load() || err == nil {
		return
	}
	root := err
	for u := errors.Unwrap(root); u != nil; u = errors.Unwrap(root) {
		root = u
	}
	capture(fmt.Sprintf("%T", root), err.Error(), sentry.NewStacktrace(), "internal error", nil)
}

// Recover reports a panic and then re-panics, so the process still crashes
// (and its supervisor restarts it) exactly as it would without reporting.
// Use it as `defer errreport.Recover()` at the top of a goroutine.
func Recover() {
	if !enabled.Load() {
		return
	}
	if v := recover(); v != nil {
		Panic(v)
		sentry.Flush(5 * time.Second)
		panic(v)
	}
}

// Panic reports a recovered panic value with the current stack.
func Panic(v any) {
	if !enabled.Load() {
		return
	}
	capture(fmt.Sprintf("panic (%T)", v), fmt.Sprint(v), sentry.NewStacktrace(), "panic", nil)
}

// RecoverHTTP reports handler panics and answers 500 instead of letting
// net/http log the panic and drop the connection.
func RecoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			Panic(v)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// BrowserError is an uncaught dashboard error, as posted by the browser.
type BrowserError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Stack   string `json:"stack"`
	Page    string `json:"page"`
}

var (
	chromeFrame  = regexp.MustCompile(`^\s*at (?:(.+?) \()?(\S+?):(\d+):(\d+)\)?$`)
	firefoxFrame = regexp.MustCompile(`^(.*)@(\S+?):(\d+):(\d+)$`)
	pageRoute    = regexp.MustCompile(`[^a-z/-]+`)
)

// Browser reports an error from the dashboard. Only the script file name,
// function, line and column of each stack frame are kept.
func Browser(b BrowserError) {
	if !enabled.Load() {
		return
	}
	st := &sentry.Stacktrace{}
	lines := strings.Split(b.Stack, "\n")
	for i := len(lines) - 1; i >= 0 && len(st.Frames) < 50; i-- { // Sentry wants the oldest frame first
		m := chromeFrame.FindStringSubmatch(lines[i])
		if m == nil {
			m = firefoxFrame.FindStringSubmatch(lines[i])
		}
		if m == nil {
			continue
		}
		line, _ := strconv.Atoi(m[3])
		col, _ := strconv.Atoi(m[4])
		file := m[2]
		if j := strings.LastIndexAny(file, "/\\"); j >= 0 {
			file = file[j+1:]
		}
		file, _, _ = strings.Cut(file, "?")
		st.Frames = append(st.Frames, sentry.Frame{Function: clip(m[1], 100), Filename: clip(file, 100),
			Lineno: line, Colno: col, InApp: file == "app.js"})
	}
	if len(st.Frames) == 0 {
		st = nil
	}
	typ := pageRoute.ReplaceAllString(strings.ToLower(clip(b.Type, 40)), "")
	if typ == "" {
		typ = "error"
	}
	// The page is a dashboard route such as #/sources/12: keep its words only.
	page, _, _ := strings.Cut(b.Page, "?")
	page = strings.Trim(pageRoute.ReplaceAllString(strings.ToLower(clip(page, 100)), ""), "/-")
	capture(typ, b.Message, st, "browser", map[string]string{"component": "browser", "page": "/" + page})
}

func capture(typ, value string, st *sentry.Stacktrace, mechanism string, tags map[string]string) {
	e := sentry.NewEvent()
	e.Level = sentry.LevelError
	e.Exception = []sentry.Exception{{Type: clip(typ, 100), Value: value, Stacktrace: st,
		Mechanism: &sentry.Mechanism{Type: mechanism, Handled: sentry.Pointer(mechanism != "panic")}}}
	sentry.WithScope(func(s *sentry.Scope) {
		s.SetTags(tags)
		sentry.CaptureEvent(e)
	})
}

// Scrub removes everything from an event that could identify a customer,
// their machines or their data, or that could be a secret. It is applied to
// every event just before it is sent.
func Scrub(e *sentry.Event) *sentry.Event {
	if e == nil {
		return nil
	}
	e.ServerName = ""
	e.User = sentry.User{}
	e.Request = nil
	e.Breadcrumbs = nil
	e.Attachments = nil
	e.Threads = nil
	e.Transaction = ""
	e.Message = ScrubText(e.Message)
	for k := range e.Contexts {
		if k != "os" && k != "runtime" && k != "device" {
			delete(e.Contexts, k)
		}
	}
	if dev := e.Contexts["device"]; dev != nil {
		e.Contexts["device"] = sentry.Context{"arch": dev["arch"], "num_cpu": dev["num_cpu"]}
	}
	if osc := e.Contexts["os"]; osc != nil {
		e.Contexts["os"] = sentry.Context{"name": osc["name"]}
	}
	for k, v := range e.Tags {
		switch k {
		case "component", "page":
			e.Tags[k] = clip(v, 100)
		default:
			delete(e.Tags, k)
		}
	}
	for i := range e.Exception {
		ex := &e.Exception[i]
		if ex.Mechanism != nil && ex.Mechanism.Type == "browser" {
			e.Platform = "javascript"
			e.Contexts = nil // the server's OS and Go version, not the browser's
		}
		ex.Value = ScrubText(ex.Value)
		if ex.Stacktrace != nil {
			for j := range ex.Stacktrace.Frames {
				f := &ex.Stacktrace.Frames[j]
				if f.Filename == "" {
					f.Filename = f.AbsPath
				}
				f.Filename = codePath(f.Filename)
				f.AbsPath, f.Vars = "", nil
				f.PreContext, f.ContextLine, f.PostContext = nil, "", nil
			}
		}
	}
	return e
}

// codePath keeps a source file name relative to the module, so a build
// without -trimpath doesn't send the builder's home directory.
func codePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, root := range []string{"/backupproof/", "/go/pkg/mod/", "/src/"} {
		if i := strings.LastIndex(p, root); i >= 0 && (strings.HasPrefix(p, "/") || strings.Contains(p, ":/")) {
			return p[i+len(root):]
		}
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, ":/") {
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[i+1:]
		}
	}
	return p
}

var scrubbers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`-----BEGIN [^-]*-----[\s\S]*?(-----END [^-]*-----|$)`), "<pem>"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+\S+`), "$1 <redacted>"},
	{regexp.MustCompile(`\bbp[a-z]{0,4}[0-9]?[_:]\S+`), "<redacted>"}, // API tokens (bpt_…), keys (bpkey1:…)
	// name=value pairs whose name sounds secret (password=…, B2_APPLICATION_KEY: …).
	{regexp.MustCompile(`(?i)\b([\w-]*(?:pass(?:word|phrase)?|pwd|secret|token|key|auth\w*|credential|cookie|session|dsn|signature|otp|totp)[\w-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`), "$1$2<redacted>"},
	{regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S+`), "<url>"},
	{regexp.MustCompile(`[\w.+-]+@[\w-]+(\.[\w-]+)+`), "<email>"},
	{regexp.MustCompile(`(?i)\b[a-z]:\\[^\s"'<>|]*`), "<path>"},
	{regexp.MustCompile(`(^|[\s"'(=\[,])~?/[^\s"'),\]]+`), "$1<path>"},
	{regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`), "<ip>"},
	{regexp.MustCompile(`(?i)\[?\b([0-9a-f]{0,4}:){2,7}[0-9a-f]{1,4}\b\]?(:\d+)?`), "<ip>"},
	{regexp.MustCompile(`(?i)\b([a-z0-9-]+\.)+[a-z]{2,}\b`), "<host>"},
}

// Anything long with a digit in it looks random: keys, tokens, key IDs, digests.
var longRandom = regexp.MustCompile(`[A-Za-z0-9+/_=-]{20,}`)

// ScrubText redacts secrets, URLs, paths, addresses and host names in s and
// caps its length.
func ScrubText(s string) string {
	if s == "" {
		return s
	}
	s = clip(s, 2000)
	for _, sc := range scrubbers {
		s = sc.re.ReplaceAllString(s, sc.with)
	}
	s = longRandom.ReplaceAllStringFunc(s, func(w string) string {
		if strings.ContainsAny(w, "0123456789") {
			return "<redacted>"
		}
		return w
	})
	return clip(s, 1000)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
