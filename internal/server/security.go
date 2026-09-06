package server

import (
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Zero limits select production defaults; forwarded headers are ignored unless the socket peer is trusted.
type SecurityConfig struct {
	TrustedProxies          []netip.Prefix
	MaxWebSockets           int
	MaxWebSocketsPerIP      int
	MaxWebSocketsPerAccount int
	WSHandshakesPerMinute   int
	CursorUpdatesPerMinute  int
	SendRequestsPerMinute   int
	SendRequestsPerIPMinute int
	MaxPendingSends         int
}

func (c SecurityConfig) normalized() SecurityConfig {
	if c.MaxWebSockets <= 0 {
		c.MaxWebSockets = 512
	}
	if c.MaxWebSocketsPerIP <= 0 {
		c.MaxWebSocketsPerIP = 8
	}
	if c.MaxWebSocketsPerAccount <= 0 {
		c.MaxWebSocketsPerAccount = 4
	}
	if c.WSHandshakesPerMinute <= 0 {
		c.WSHandshakesPerMinute = 30
	}
	if c.CursorUpdatesPerMinute <= 0 {
		c.CursorUpdatesPerMinute = 120
	}
	if c.SendRequestsPerMinute <= 0 {
		c.SendRequestsPerMinute = 20
	}
	if c.SendRequestsPerIPMinute <= 0 {
		c.SendRequestsPerIPMinute = 60
	}
	if c.MaxPendingSends <= 0 {
		c.MaxPendingSends = 32
	}
	c.TrustedProxies = append([]netip.Prefix(nil), c.TrustedProxies...)
	return c
}

func (s *Server) trustedProxy(addr netip.Addr) bool {
	for _, prefix := range s.cfg.Security.TrustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (s *Server) clientIP(r *http.Request) string {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	var addr netip.Addr
	if err == nil {
		addr = peer.Addr()
	} else {
		addr, err = netip.ParseAddr(r.RemoteAddr)
	}
	if err != nil {
		return "unknown"
	}
	addr = addr.Unmap().WithZone("")
	fallback := addr.String()
	if !s.trustedProxy(addr) {
		return fallback
	}
	values := r.Header.Values("X-Forwarded-For")
	total := 0
	for _, value := range values {
		total += len(value)
	}
	if total > 4096 || len(values) > 32 {
		return fallback
	}
	chain := strings.Join(values, ",")
	if chain == "" {
		return fallback
	}
	hops := strings.Split(chain, ",")
	if len(hops) > 32 {
		return fallback
	}
	var parsed [32]netip.Addr
	for i, hop := range hops {
		ip, parseErr := netip.ParseAddr(strings.TrimSpace(hop))
		if parseErr != nil || ip.Zone() != "" {
			return fallback
		}
		parsed[i] = ip.Unmap()
	}
	for i := len(hops) - 1; i >= 0 && s.trustedProxy(addr); i-- {
		addr = parsed[i]
	}
	return addr.String()
}

type socketAdmission struct {
	mu       sync.Mutex
	total    int
	ips      map[string]int
	accounts map[int64]int
}

func (a *socketAdmission) acquire(ip string, account int64, cfg SecurityConfig) (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	accountFull := account > 0 && a.accounts[account] >= cfg.MaxWebSocketsPerAccount
	if a.total >= cfg.MaxWebSockets || a.ips[ip] >= cfg.MaxWebSocketsPerIP || accountFull {
		return nil, false
	}
	if a.ips == nil {
		a.ips = make(map[string]int)
		a.accounts = make(map[int64]int)
	}
	a.total++
	a.ips[ip]++
	if account > 0 {
		a.accounts[account]++
	}
	return sync.OnceFunc(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.total--
		a.ips[ip]--
		if a.ips[ip] == 0 {
			delete(a.ips, ip)
		}
		if account > 0 {
			a.accounts[account]--
			if a.accounts[account] == 0 {
				delete(a.accounts, account)
			}
		}
	}), true
}

type tokenBucket struct {
	tokens  float64
	updated time.Time
}

func (b *tokenBucket) allow(now time.Time, perMinute, burst int) bool {
	if b.updated.IsZero() {
		b.tokens = float64(burst)
	} else {
		b.tokens = min(float64(burst), b.tokens+now.Sub(b.updated).Minutes()*float64(perMinute))
	}
	b.updated = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type requestLimiter struct {
	mu        sync.Mutex
	buckets   map[string]tokenBucket
	nextSweep time.Time
}

func (l *requestLimiter) allow(key string, now time.Time, perMinute, burst int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]tokenBucket)
	}
	if !now.Before(l.nextSweep) {
		idle := max(time.Minute, time.Duration(float64(time.Minute)*float64(burst)/float64(perMinute)))
		for key, bucket := range l.buckets {
			if now.Sub(bucket.updated) >= idle {
				delete(l.buckets, key)
			}
		}
		l.nextSweep = now.Add(time.Minute)
	}
	bucket, exists := l.buckets[key]
	if !exists && len(l.buckets) >= 4096 {
		return false
	}
	allowed := bucket.allow(now, perMinute, burst)
	l.buckets[key] = bucket
	return allowed
}

func rateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	sendError(w, http.StatusTooManyRequests, "rate_limited", nil)
}
