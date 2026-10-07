package server

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The setup code protects first-run setup: until an admin exists, anyone who
// can reach the dashboard could otherwise create the admin account. The code
// is written to DataDir/setup-code.txt and printed in the server log; the
// installers show it. Requests made directly on the machine (loopback, not
// via a reverse proxy) don't need it.

const setupCodeFile = "setup-code.txt"

// unambiguous characters: no 0/O, 1/I/L
const setupAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func (s *Server) setupCodePath() string { return filepath.Join(s.cfg.DataDir, setupCodeFile) }

// ensureSetupCode creates the code if no admin exists yet and returns it.
func (s *Server) ensureSetupCode() (string, error) {
	if b, err := os.ReadFile(s.setupCodePath()); err == nil {
		if code := strings.TrimSpace(string(b)); code != "" {
			return code, nil
		}
	}
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		if i == 4 {
			sb.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(setupAlphabet))))
		if err != nil {
			return "", err
		}
		sb.WriteByte(setupAlphabet[n.Int64()])
	}
	code := sb.String()
	if err := os.WriteFile(s.setupCodePath(), []byte(code+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing setup code: %w", err)
	}
	return code, nil
}

func (s *Server) removeSetupCode() {
	if err := os.Remove(s.setupCodePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Printf("could not remove %s: %v", s.setupCodePath(), err)
	}
}

// isLocalRequest reports whether r was made on this machine directly. A
// request relayed by a reverse proxy also arrives from loopback, so any
// forwarding header makes it non-local.
func isLocalRequest(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkSetupCode validates the code for a non-local first-run setup.
func (s *Server) checkSetupCode(r *http.Request, given string) error {
	if isLocalRequest(r) {
		return nil
	}
	want, err := s.ensureSetupCode()
	if err != nil {
		return err
	}
	norm := func(c string) string {
		return strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(c), " ", ""), "-", ""))
	}
	if given == "" || subtle.ConstantTimeCompare([]byte(norm(given)), []byte(norm(want))) != 1 {
		return errors.New("the setup code is missing or wrong. It was shown when BackupProof was installed, and is saved in setup-code.txt in the data folder")
	}
	return nil
}
