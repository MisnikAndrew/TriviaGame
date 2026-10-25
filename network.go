package main

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// limiter is a token bucket per key: a key may make `burst` requests at once
// and then `rate` per second. Buckets idle for a minute are forgotten, so
// burst/rate must stay below a minute. A nil limiter allows everything.
type limiter struct {
	rate, burst float64
	now         func() time.Time

	mu      sync.Mutex
	buckets map[string]bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), now: time.Now, buckets: map[string]bucket{}}
}

func (l *limiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.at) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = bucket{tokens: l.burst, at: now}
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.buckets[key] = b
	return allowed
}

// streamCounter caps the number of open event streams per key.
// A nil counter allows everything.
type streamCounter struct {
	max int

	mu sync.Mutex
	n  map[string]int
}

func newStreamCounter(max int) *streamCounter {
	return &streamCounter{max: max, n: map[string]int{}}
}

func (c *streamCounter) acquire(key string) bool {
	if c == nil || c.max <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key] >= c.max {
		return false
	}
	c.n[key]++
	return true
}

func (c *streamCounter) release(key string) {
	if c == nil || c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key]--; c.n[key] <= 0 {
		delete(c.n, key)
	}
}

// clientIP is the address of the browser. Behind a tunnel or a reverse proxy
// (trustProxy) it is taken from the proxy's headers — so the server must then
// be reachable through that proxy only, or anybody could forge them. Of the
// X-Forwarded-For chain the last entry is used: it is the one the nearest proxy
// appended, the others come from the client.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); ip != nil {
			return ip.String()
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			last := xff[strings.LastIndexByte(xff, ',')+1:]
			if ip := net.ParseIP(strings.TrimSpace(last)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limitKey is what requests are counted by: the IPv4 address, or the /64
// network of an IPv6 one — the smallest block a provider hands to a customer.
func limitKey(ip string) string {
	p := net.ParseIP(ip)
	switch {
	case p == nil:
		return ip
	case p.To4() != nil:
		return p.To4().String()
	default:
		return p.Mask(net.CIDRMask(64, 128)).String()
	}
}

// isLocalIP reports whether ip belongs to this machine or its own network.
func isLocalIP(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && (p.IsLoopback() || p.IsPrivate() || p.IsLinkLocalUnicast())
}

// normalizePublicURL checks the -public-url flag: scheme://host[:port] and
// nothing else (the page asks for /api/... from the root, so no sub-paths).
func normalizePublicURL(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("нужен адрес вида https://quiz.example.com, а не %q", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("в адресе %q не должно быть пути, параметров и логина — только схема, хост и порт", s)
	}
	return u.Scheme + "://" + u.Host, nil
}
