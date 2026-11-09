package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/server"
)

// A storage's recovery kit needs the administrator to confirm who they are,
// can't be reached any other way, and always leaves a record.
func TestRecoveryKitForExistingStorage(t *testing.T) {
	const adminPass, storagePass = "a-long-admin-password", "repository-password-123"
	dir := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(dir, "server"), NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var mu sync.Mutex
	var notes []string
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		notes = append(notes, string(b))
		mu.Unlock()
	}))
	defer hook.Close()

	newClient := func() *api { jar, _ := cookiejar.New(nil); return &api{t: t, base: ts.URL, c: &http.Client{Jar: jar}} }
	admin := newClient()
	var setup struct{ CSRF string }
	admin.do("POST", "/api/setup", map[string]string{"username": "admin", "password": adminPass}, &setup)
	admin.csrf = setup.CSRF
	admin.do("PUT", "/api/settings/notify", map[string]string{"webhookUrl": hook.URL}, nil)
	admin.do("POST", "/api/users", map[string]string{"username": "op", "password": "operator-password-1", "role": "operator"}, &map[string]int64{})
	var repo struct{ ID int64 }
	admin.do("POST", "/api/repositories", map[string]any{"name": "disk", "backend": map[string]any{"type": "local", "path": filepath.Join(dir, "repo")}, "password": storagePass}, &repo)
	path := fmt.Sprintf("/api/repositories/%d/recovery-kit", repo.ID)

	// kit returns the status and raw response, which must only hold the
	// password when the request was allowed.
	kit := func(a *api, password, code string) (int, string, http.Header) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"password": password, "code": code})
		req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", a.csrf)
		resp, err := a.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK && strings.Contains(string(raw), storagePass) {
			t.Errorf("refused request got the password: %s", raw)
		}
		return resp.StatusCode, string(raw), resp.Header
	}
	wantOK := func(what, password, code string) {
		t.Helper()
		got, raw, hdr := kit(admin, password, code)
		if got != http.StatusOK || !strings.Contains(raw, storagePass) {
			t.Fatalf("%s: HTTP %d: %s", what, got, raw)
		}
		if hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", what, hdr.Get("Cache-Control"))
		}
	}
	wantCode := func(what string, a *api, password, code string, want int) {
		t.Helper()
		if got, raw, _ := kit(a, password, code); got != want {
			t.Errorf("%s: HTTP %d, want %d: %s", what, got, want, raw)
		}
	}

	// Who can't get it.
	var login struct{ CSRF string }
	op := newClient()
	op.do("POST", "/api/login", map[string]string{"username": "op", "password": "operator-password-1"}, &login)
	op.csrf = login.CSRF
	wantCode("operator", op, "operator-password-1", "", http.StatusForbidden)
	wantCode("no session", newClient(), adminPass, "", http.StatusUnauthorized)
	wantCode("session without the CSRF token", &api{t: t, base: ts.URL, c: admin.c}, adminPass, "", http.StatusForbidden)
	var tok struct{ Token string }
	admin.do("POST", "/api/tokens", map[string]any{"name": "ci", "role": "admin"}, &tok)
	if code := bearer(t, ts.URL, tok.Token, "POST", path, map[string]string{"password": adminPass}); code != http.StatusForbidden {
		t.Errorf("admin API token: HTTP %d, want 403", code)
	}

	// The password is needed, and the right one gets the storage's.
	wantCode("wrong password", admin, "not-the-password", "", http.StatusForbidden)
	wantCode("no password", admin, "", "", http.StatusForbidden)
	wantOK("right password", adminPass, "")
	if got := admin.do("POST", "/api/repositories/999/recovery-kit", map[string]string{"password": adminPass}, nil); got != http.StatusNotFound {
		t.Errorf("missing storage: HTTP %d, want 404", got)
	}

	// With two-factor on, a code is needed too and works only once.
	var tf struct{ Secret string }
	admin.do("POST", "/api/2fa/setup", nil, &tf)
	step := time.Now().Unix() / 30
	var en struct{ RecoveryCodes []string }
	admin.do("POST", "/api/2fa/enable", map[string]string{"code": totpAt(tf.Secret, step)}, &en)
	next := totpAt(tf.Secret, step+1)
	wantCode("two-factor, no code", admin, adminPass, "", http.StatusForbidden)
	wantCode("two-factor, wrong password", admin, "not-the-password", next, http.StatusForbidden)
	wantOK("two-factor, right code", adminPass, next)
	wantCode("two-factor, code replayed", admin, adminPass, next, http.StatusForbidden)
	wantOK("two-factor, recovery code", adminPass, en.RecoveryCodes[0])
	wantCode("two-factor, recovery code replayed", admin, adminPass, en.RecoveryCodes[0], http.StatusForbidden)

	// Everything is on record, without the password in it.
	var acts []struct{ Actor, Action, Detail string }
	admin.do("GET", "/api/activity?limit=100", nil, &acts)
	counts := map[string]int{}
	for _, a := range acts {
		counts[a.Action]++
		if strings.Contains(a.Detail, storagePass) || strings.Contains(a.Detail, adminPass) {
			t.Errorf("activity holds a password: %+v", a)
		}
	}
	if counts["download-recovery-kit"] != 3 {
		t.Errorf("downloads recorded: %d, want 3", counts["download-recovery-kit"])
	}
	if counts["recovery-kit-refused"] != 6 {
		t.Errorf("refusals recorded: %d, want 6", counts["recovery-kit-refused"])
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n, joined := len(notes), strings.Join(notes, "\n")
		mu.Unlock()
		if n >= 3 {
			if !strings.Contains(joined, "recovery kit") || strings.Contains(joined, storagePass) {
				t.Errorf("notifications: %s", joined)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications sent: %d, want 3", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Guessing stops at the sign-in limit, even for the right details.
	third := totpAt(tf.Secret, step+2)
	for i := 0; i < 9; i++ { // the replayed recovery code already counts as one failure
		wantCode("guess", admin, "not-the-password", "", http.StatusForbidden)
	}
	wantCode("over the limit", admin, adminPass, third, http.StatusTooManyRequests)
}
