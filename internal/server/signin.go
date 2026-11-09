package server

import (
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
