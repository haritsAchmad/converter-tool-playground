package app

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ipLimiter is a simple token bucket: tokens refill continuously at rate
// per second up to burst, and each allowed request consumes one token.
type ipLimiter struct {
	mu       sync.Mutex
	tokens   float64
	rate     float64
	burst    float64
	last     time.Time
	lastSeen time.Time
}

func newIPLimiter(rate, burst float64) *ipLimiter {
	now := time.Now()
	return &ipLimiter{tokens: burst, rate: rate, burst: burst, last: now, lastSeen: now}
}

func (l *ipLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	l.lastSeen = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// rateLimiter tracks one ipLimiter per client IP. Entries idle longer than
// the sweep threshold are evicted so the map does not grow without bound.
type rateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipLimiter
	rate     float64
	burst    float64
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{limiters: make(map[string]*ipLimiter), rate: rate, burst: burst}
}

func (r *rateLimiter) allow(ip string) bool {
	r.mu.Lock()
	l, ok := r.limiters[ip]
	if !ok {
		l = newIPLimiter(r.rate, r.burst)
		r.limiters[ip] = l
	}
	r.mu.Unlock()
	return l.allow()
}

func (r *rateLimiter) sweep(maxIdle time.Duration) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for ip, l := range r.limiters {
		l.mu.Lock()
		idle := now.Sub(l.lastSeen)
		l.mu.Unlock()
		if idle > maxIdle {
			delete(r.limiters, ip)
		}
	}
}

// clientIP uses RemoteAddr only; forwarded headers are not trusted here
// since they are attacker-controlled unless a reverse proxy strips them,
// which this service has no way to guarantee.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
