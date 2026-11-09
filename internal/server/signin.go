package server

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	signinFailures = 10
	signinWindow   = 15 * time.Minute
	signinChecks   = 4
)

// signinLimiter counts recent failed sign-ins per address.
type signinLimiter struct {
	mu   sync.Mutex
	seen map[string]*signinCount
}

type signinCount struct {
	n     int
	first time.Time
}

func (l *signinLimiter) allowed(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.seen[addr]
	return c == nil || now.Sub(c.first) > signinWindow || c.n < signinFailures
}

func (l *signinLimiter) failed(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]*signinCount{}
	}
	for a, c := range l.seen {
		if now.Sub(c.first) > signinWindow {
			delete(l.seen, a)
		}
	}
	c := l.seen[addr]
	if c == nil {
		c = &signinCount{first: now}
		l.seen[addr] = c
	}
	c.n++
}

func (l *signinLimiter) clear(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, addr)
}

// takeSigninSlot waits for room to check a password, and answers the request
// itself if there's none. A true result must be followed by releasing the slot.
func (s *Server) takeSigninSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.signinSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	case <-time.After(20 * time.Second):
		writeErr(w, http.StatusTooManyRequests, errors.New("the server is busy with other sign-ins; try again shortly"))
		return false
	}
}

// clientAddr is the address a request came from. The forwarding headers are
// only believed from a proxy on this machine; elsewhere anyone could set them.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	if v := r.Header.Get("X-Real-Ip"); v != "" {
		return v
	}
	if f := strings.Split(r.Header.Get("X-Forwarded-For"), ","); f[len(f)-1] != "" {
		return strings.TrimSpace(f[len(f)-1])
	}
	return host
}
