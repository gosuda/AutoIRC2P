package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRelayOriginsProtectHTTPAndWebSockets(t *testing.T) {
	app, _, _ := lifecycleServer(t, &lifecycleBridge{})
	app.cfg.AllowOrigin = func(origin string) bool {
		return origin == "https://chat.relay-a.example" || origin == "https://chat.relay-b.example:8443"
	}
	app.cfg.SecureCookies = true
	handler := app.Handler()
	finished := make(chan struct{}, 1)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { finished <- struct{}{} }()
		handler.ServeHTTP(w, r)
	}))
	defer web.Close()
	for _, tc := range []struct {
		name    string
		origins []string
		allowed bool
	}{
		{"first relay", []string{"https://chat.relay-a.example"}, true},
		{"second relay port", []string{"https://chat.relay-b.example:8443"}, true},
		{"wrong port", []string{"https://chat.relay-b.example"}, false},
		{"sibling tenant", []string{"https://other.relay-a.example"}, false},
		{"suffix attack", []string{"https://chat.relay-a.example.attacker.test"}, false},
		{"opaque origin", []string{"null"}, false},
		{"duplicate origin", []string{"https://chat.relay-a.example", "https://attacker.test"}, false},
		{"empty origin", []string{""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
			request.Header["Origin"] = tc.origins
			request.Host = "other.relay-a.example"
			request.Header.Set("X-Forwarded-Host", "chat.relay-a.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := http.StatusForbidden
			if tc.allowed {
				want = http.StatusOK
			}
			if response.Code != want {
				t.Fatalf("HTTP status = %d, want %d", response.Code, want)
			}
			if tc.allowed {
				cookies := response.Result().Cookies()
				if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
					t.Fatalf("session cookie lost secure attributes: %v", cookies)
				}
			}
			header := http.Header{"Origin": tc.origins, "Host": {"other.relay-a.example"}, "X-Forwarded-Host": {"chat.relay-a.example"}}
			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
			conn, upgrade, err := dialer.Dial("ws"+strings.TrimPrefix(web.URL, "http")+"/api/ws?room=%23one", header)
			if conn != nil {
				if closeErr := conn.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if upgrade != nil {
				if closeErr := upgrade.Body.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if tc.allowed && err != nil {
				t.Errorf("assigned relay WebSocket rejected: %v", err)
			}
			if !tc.allowed {
				if err == nil || upgrade == nil || upgrade.StatusCode != http.StatusForbidden {
					t.Errorf("untrusted WebSocket not rejected: response=%v error=%v", upgrade, err)
				}
			}
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("WebSocket handler did not stop")
			}
		})
	}
}
