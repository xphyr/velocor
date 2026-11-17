package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter throttles password guessing. A client IP is blocked after maxPerIP failures
// within window, and a (username, IP) pair after maxPerUser failures. Keying the per-user
// limit by IP as well means an attacker cannot lock the real administrator out.
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry
	now     func() time.Time
}

type limitEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

const (
	loginWindow     = 10 * time.Minute
	loginBlock      = 10 * time.Minute
	loginMaxPerIP   = 20
	loginMaxPerUser = 5
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: map[string]*limitEntry{}, now: time.Now}
}

func (l *loginLimiter) keys(ip, user string) []struct {
	key   string
	limit int
} {
	return []struct {
		key   string
		limit int
	}{{"ip|" + ip, loginMaxPerIP}, {"user|" + strings.ToLower(user) + "|" + ip, loginMaxPerUser}}
}

// blocked returns how long the caller must wait, or 0.
func (l *loginLimiter) blocked(ip, user string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	for _, k := range l.keys(ip, user) {
		if e := l.entries[k.key]; e != nil && now.Before(e.blockedUntil) {
			wait = max(wait, e.blockedUntil.Sub(now))
		}
	}
	return wait
}

func (l *loginLimiter) fail(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range l.keys(ip, user) {
		e := l.entries[k.key]
		if e == nil || now.Sub(e.windowStart) > loginWindow {
			e = &limitEntry{windowStart: now}
			l.entries[k.key] = e
		}
		e.failures++
		if e.failures >= k.limit {
			e.blockedUntil = now.Add(loginBlock)
		}
	}
	// Opportunistic cleanup keeps the map bounded.
	if len(l.entries) > 10000 {
		for key, e := range l.entries {
			if now.Sub(e.windowStart) > loginWindow && now.After(e.blockedUntil) {
				delete(l.entries, key)
			}
		}
	}
}

func (l *loginLimiter) succeed(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, "user|"+strings.ToLower(user)+"|"+ip)
}

// clientIP returns the TCP peer address. X-Forwarded-For is deliberately ignored: velod is
// not deployed behind a trusted proxy, and honouring the header would let clients evade
// the limiter.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
