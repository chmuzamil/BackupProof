package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"rsc.io/qr"
)

// Routes for two-factor sign-in, API tokens and the activity log.
func (s *Server) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/2fa/setup", s.auth("auditor", s.handleTwoFactorSetup))
	mux.HandleFunc("POST /api/2fa/enable", s.auth("auditor", s.handleTwoFactorEnable))
	mux.HandleFunc("POST /api/2fa/disable", s.auth("auditor", s.handleTwoFactorDisable))
	mux.HandleFunc("POST /api/users/{id}/2fa/reset", s.auth("admin", s.handleTwoFactorReset))
	mux.HandleFunc("GET /api/tokens", s.auth("admin", s.handleListTokens))
	mux.HandleFunc("POST /api/tokens", s.auth("admin", s.handleCreateToken))
	mux.HandleFunc("DELETE /api/tokens/{id}", s.auth("admin", s.handleDeleteToken))
	mux.HandleFunc("GET /api/activity", s.auth("auditor", s.handleActivity))
}

// tokenForbidden lists what API tokens may never do.
func tokenForbidden(pattern string) bool {
	return strings.Contains(pattern, "/api/users") || strings.Contains(pattern, "/api/tokens") || strings.Contains(pattern, "/api/2fa")
}

// --- sign-in with a second factor ----------------------------------------------

// A login challenge is the half-finished sign-in between the password and the
// authenticator code. It lives in memory for five minutes and allows five tries.
type loginChallenge struct {
	userID  int64
	expires time.Time
	tries   int
}

type challenges struct {
	mu sync.Mutex
	m  map[string]*loginChallenge
}

func (c *challenges) add(userID int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*loginChallenge{}
	}
	for k, v := range c.m {
		if time.Now().After(v.expires) {
			delete(c.m, k)
		}
	}
	tok := bpcrypto.RandomToken("bpc_")
	c.m[bpcrypto.TokenHash(tok)] = &loginChallenge{userID: userID, expires: time.Now().Add(5 * time.Minute)}
	return tok
}

// take returns the user of a live challenge and counts the attempt.
func (c *challenges) take(tok string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := bpcrypto.TokenHash(tok)
	ch := c.m[k]
	if ch == nil || time.Now().After(ch.expires) {
		delete(c.m, k)
		return 0, errors.New("the sign-in took too long. Enter your password again")
	}
	ch.tries++
	if ch.tries > 5 {
		delete(c.m, k)
		return 0, errors.New("too many wrong codes. Enter your password again")
	}
	return ch.userID, nil
}

func (c *challenges) done(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, bpcrypto.TokenHash(tok))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Challenge, Code string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	// Step 2: the code from the authenticator app (or a recovery code).
	if req.Challenge != "" {
		uid, err := s.logins.take(req.Challenge)
		if err != nil {
			writeErr(w, 401, err)
			return
		}
		method, err := s.store.VerifySecondFactor(uid, req.Code)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			writeErr(w, 401, errors.New("that code is wrong or was already used. Try the newest code from your authenticator app"))
			return
		}
		s.logins.done(req.Challenge)
		u, err := s.store.UserByID(uid)
		if err != nil {
			writeErr(w, 401, err)
			return
		}
		s.audit(u.Username, "sign-in", "with password and "+method)
		s.startSession(w, r, u)
		return
	}
	// Step 1: the password.
	u, err := s.store.Authenticate(req.Username, req.Password)
	if err != nil {
		time.Sleep(300 * time.Millisecond)
		writeErr(w, 401, err)
		return
	}
	if u.TwoFactor {
		writeJSON(w, 200, map[string]any{"needCode": true, "challenge": s.logins.add(u.ID)})
		return
	}
	s.audit(u.Username, "sign-in", "with password")
	s.startSession(w, r, u)
}

// --- two-factor setup ---------------------------------------------------------------

func (s *Server) handleTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	secret, err := s.store.BeginTwoFactor(u.ID)
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	uri := totpURI(secret, u.Username)
	code, err := qr.Encode(uri, qr.M)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	// The QR code goes to the browser as rows of 0/1 and is drawn there as
	// SVG, so no image service or inline image is needed.
	rows := make([]string, code.Size)
	for y := 0; y < code.Size; y++ {
		var b strings.Builder
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		rows[y] = b.String()
	}
	writeJSON(w, 200, map[string]any{"secret": b32.EncodeToString(secret), "uri": uri, "qr": rows})
}

func (s *Server) handleTwoFactorEnable(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	u := currentUser(r)
	codes, err := s.store.EnableTwoFactor(u.ID, req.Code)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.audit(u.Username, "enable-2fa", "")
	writeJSON(w, 200, map[string]any{"recoveryCodes": codes})
}

func (s *Server) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	u := currentUser(r)
	if _, err := s.store.Authenticate(u.Username, req.Password); err != nil {
		time.Sleep(300 * time.Millisecond)
		writeErr(w, 403, errors.New("that password is wrong"))
		return
	}
	if err := s.store.DisableTwoFactor(u.ID); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(u.Username, "disable-2fa", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleTwoFactorReset(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	target, err := s.store.UserByID(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := s.store.DisableTwoFactor(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.audit(s.actor(r), "reset-2fa", fmt.Sprintf("turned off two-factor sign-in for %q", target.Username))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleChangePassword needs the current password, so a borrowed session
// can't lock the owner out.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ CurrentPassword, Password string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	u := currentUser(r)
	if _, err := s.store.Authenticate(u.Username, req.CurrentPassword); err != nil {
		time.Sleep(300 * time.Millisecond)
		writeErr(w, 403, errors.New("your current password is wrong"))
		return
	}
	if err := s.store.SetPassword(u.ID, req.Password); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.audit(u.Username, "change-password", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- API tokens -------------------------------------------------------------------------

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.APITokens()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(ts))
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Role    string `json:"role"`
		Expires int    `json:"expiresDays"` // 0 = never
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Expires < 0 || req.Expires > 3650 {
		writeErr(w, 400, errors.New("expiry must be between 1 and 3650 days, or 0 for never"))
		return
	}
	u := currentUser(r)
	tok, id, err := s.store.CreateAPIToken(u.ID, req.Name, req.Role, time.Duration(req.Expires)*24*time.Hour)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	exp := "never expires"
	if req.Expires > 0 {
		exp = fmt.Sprintf("expires in %d days", req.Expires)
	}
	s.audit(u.Username, "create-token", fmt.Sprintf("API token %q (%s role, %s)", strings.TrimSpace(req.Name), req.Role, exp))
	writeJSON(w, 200, map[string]any{"id": id, "token": tok})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	name, err := s.store.DeleteAPIToken(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	s.audit(s.actor(r), "delete-token", fmt.Sprintf("API token %q", name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- activity ----------------------------------------------------------------------------

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.store.Activity(before, limit, r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, nonNil(rows))
}
