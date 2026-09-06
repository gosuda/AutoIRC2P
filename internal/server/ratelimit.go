package server

import (
	"net"
	"net/http"
	"time"
)

type authWindow struct {
	until    time.Time
	attempts int
}

func (s *Server) limitAuthentication(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		now := time.Now()
		s.authMu.Lock()
		if s.authAttempts == nil {
			s.authAttempts = make(map[string]authWindow)
		}
		for key, window := range s.authAttempts {
			if !now.Before(window.until) {
				delete(s.authAttempts, key)
			}
		}
		window, exists := s.authAttempts[host]
		if !exists {
			window = authWindow{until: now.Add(time.Minute)}
		}
		denied := window.attempts >= 10 || !exists && len(s.authAttempts) >= 4096
		if !denied {
			window.attempts++
			s.authAttempts[host] = window
		}
		s.authMu.Unlock()
		if denied {
			w.Header().Set("Retry-After", "60")
			writeError(w, 429, "too many account attempts; wait one minute")
			return
		}
		next(w, r)
	}
}
