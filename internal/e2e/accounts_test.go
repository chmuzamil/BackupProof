package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // TOTP is defined with HMAC-SHA1.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chmuzamil/backupproof/internal/server"
)

func totpAt(secret string, step int64) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1_000_000)
}

// bearer sends a request with an API token instead of a session.
func bearer(t *testing.T, base, tok, method, path string, body any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTwoFactorTokensAndActivity(t *testing.T) {
	data := filepath.Join(t.TempDir(), "server")
	srv, err := server.New(server.Config{DataDir: data, NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	newClient := func() *api { jar, _ := cookiejar.New(nil); return &api{t: t, base: ts.URL, c: &http.Client{Jar: jar}} }
	admin := newClient()
	var setup struct{ CSRF string }
	admin.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &setup)
	admin.csrf = setup.CSRF

	// Changing the password needs the current one.
	if code := admin.do("POST", "/api/users/password", map[string]string{"currentPassword": "wrong-password", "password": "another-long-password"}, nil); code != http.StatusForbidden {
		t.Errorf("password change with a wrong current password: HTTP %d, want 403", code)
	}

	// Turn on two-factor sign-in.
	var tf struct {
		Secret string
		QR     []string
	}
	admin.do("POST", "/api/2fa/setup", nil, &tf)
	if tf.Secret == "" || len(tf.QR) < 21 {
		t.Fatalf("setup: %+v", tf)
	}
	step := time.Now().Unix() / 30
	var en struct{ RecoveryCodes []string }
	admin.do("POST", "/api/2fa/enable", map[string]string{"code": totpAt(tf.Secret, step)}, &en)
	if len(en.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes: %v", en.RecoveryCodes)
	}

	login := func(code string) (int, *api) {
		c := newClient()
		var first struct {
			NeedCode  bool
			Challenge string
		}
		c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &first)
		if !first.NeedCode || first.Challenge == "" {
			t.Fatalf("password step should ask for a code: %+v", first)
		}
		var done struct{ CSRF string }
		got := c.do("POST", "/api/login", map[string]string{"challenge": first.Challenge, "code": code}, nil)
		if got == http.StatusOK {
			c.do("GET", "/api/status", nil, &done)
		}
		return got, c
	}
	if code, _ := login("000000"); code != http.StatusUnauthorized {
		t.Errorf("wrong code: HTTP %d, want 401", code)
	}
	if code, _ := login(totpAt(tf.Secret, step)); code != http.StatusUnauthorized {
		t.Errorf("reusing the code that turned two-factor on: HTTP %d, want 401", code)
	}
	next := totpAt(tf.Secret, step+1)
	if code, _ := login(next); code != http.StatusOK {
		t.Errorf("next code: HTTP %d, want 200", code)
	}
	if code, _ := login(next); code != http.StatusUnauthorized {
		t.Errorf("replayed code: HTTP %d, want 401", code)
	}
	if code, _ := login(en.RecoveryCodes[0]); code != http.StatusOK {
		t.Errorf("recovery code: HTTP %d, want 200", code)
	}
	if code, _ := login(en.RecoveryCodes[0]); code != http.StatusUnauthorized {
		t.Errorf("recovery code used twice: HTTP %d, want 401", code)
	}
	// Five wrong tries end the attempt; then even a right code is refused.
	{
		c := newClient()
		var first struct{ Challenge string }
		c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &first)
		for i := 0; i < 5; i++ {
			c.do("POST", "/api/login", map[string]string{"challenge": first.Challenge, "code": "111111"}, nil)
		}
		if code := c.do("POST", "/api/login", map[string]string{"challenge": first.Challenge, "code": en.RecoveryCodes[1]}, nil); code != http.StatusUnauthorized {
			t.Errorf("sixth try on one sign-in: HTTP %d, want 401", code)
		}
	}

	// API tokens.
	var tok struct{ Token string }
	admin.do("POST", "/api/tokens", map[string]any{"name": "ci", "role": "operator", "expiresDays": 30}, &tok)
	var ro struct{ Token string }
	admin.do("POST", "/api/tokens", map[string]any{"name": "monitoring", "role": "auditor"}, &ro)
	if code := bearer(t, ts.URL, tok.Token, "GET", "/api/sources", nil); code != http.StatusOK {
		t.Errorf("token reading items: HTTP %d", code)
	}
	if code := bearer(t, ts.URL, tok.Token, "POST", "/api/users", map[string]string{"username": "x", "password": "long-password-1", "role": "admin"}); code != http.StatusForbidden {
		t.Errorf("token creating a user: HTTP %d, want 403", code)
	}
	if code := bearer(t, ts.URL, tok.Token, "POST", "/api/tokens", map[string]string{"name": "x", "role": "admin"}); code != http.StatusForbidden {
		t.Errorf("token creating a token: HTTP %d, want 403", code)
	}
	if code := bearer(t, ts.URL, ro.Token, "POST", "/api/repositories", map[string]any{"name": "r", "backend": map[string]string{"type": "local", "path": t.TempDir()}, "password": "repository-password-1"}); code != http.StatusForbidden {
		t.Errorf("auditor token adding storage: HTTP %d, want 403", code)
	}
	if code := bearer(t, ts.URL, "bpt_not-a-token", "GET", "/api/sources", nil); code != http.StatusUnauthorized {
		t.Errorf("unknown token: HTTP %d, want 401", code)
	}
	var list []struct {
		ID   int64
		Name string
	}
	admin.do("GET", "/api/tokens", nil, &list)
	if len(list) != 2 {
		t.Fatalf("tokens: %+v", list)
	}
	admin.do("DELETE", fmt.Sprintf("/api/tokens/%d", list[0].ID), nil, &map[string]bool{})
	if code := bearer(t, ts.URL, tok.Token, "GET", "/api/sources", nil); code != http.StatusUnauthorized {
		t.Errorf("deleted token: HTTP %d, want 401", code)
	}

	// The activity log shows what happened, newest first.
	var acts []struct{ Actor, Action, Detail string }
	admin.do("GET", "/api/activity?limit=100", nil, &acts)
	seen := map[string]bool{}
	for _, a := range acts {
		seen[a.Action] = true
	}
	for _, want := range []string{"setup", "enable-2fa", "sign-in", "create-token", "delete-token"} {
		if !seen[want] {
			t.Errorf("activity is missing %q: %+v", want, acts)
		}
	}
	var found []struct{ Detail string }
	admin.do("GET", "/api/activity?q=monitoring", nil, &found)
	if len(found) != 1 || !strings.Contains(found[0].Detail, "monitoring") {
		t.Errorf("activity search: %+v", found)
	}

	// A locked-out admin turns two-factor off from the server's console.
	srv.Close()
	if err := server.RecoverAccount(data, "admin", "", true); err != nil {
		t.Fatal(err)
	}
	srv2, err := server.New(server.Config{DataDir: data, NoLocalAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	jar, _ := cookiejar.New(nil)
	c := &api{t: t, base: ts2.URL, c: &http.Client{Jar: jar}}
	var after struct{ NeedCode bool }
	c.do("POST", "/api/login", map[string]string{"username": "admin", "password": "a-long-admin-password"}, &after)
	if after.NeedCode {
		t.Error("two-factor still asked for after admin reset-2fa")
	}
}
