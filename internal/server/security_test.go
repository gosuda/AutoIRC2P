package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

func TestClientIPHonorsOnlyTrustedProxyChain(t *testing.T) {
	app := New(Config{Security: SecurityConfig{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:1::/48")}}}, nil, nil, nil, nil)
	t.Cleanup(app.cancel)
	for _, tc := range []struct{ name, peer, forwarded, want string }{
		{"untrusted spoof", "192.0.2.1:4000", "198.51.100.1", "192.0.2.1"},
		{"trusted chain", "10.0.0.1:4000", "198.51.100.1, 10.0.0.2", "198.51.100.1"},
		{"untrusted intermediary", "10.0.0.1:4000", "203.0.113.9, 192.0.2.2, 10.0.0.2", "192.0.2.2"},
		{"mapped socket", "[::ffff:192.0.2.1]:4000", "198.51.100.1", "192.0.2.1"},
		{"mapped hop", "10.0.0.1:4000", "::ffff:198.51.100.1", "198.51.100.1"},
		{"IPv6 chain", "[2001:db8:1::1]:4000", "2001:db8:2::1, 2001:db8:1::2", "2001:db8:2::1"},
		{"malformed chain", "10.0.0.1:4000", "not-an-ip, 10.0.0.2", "10.0.0.1"},
		{"empty hop", "10.0.0.1:4000", "198.51.100.1,,10.0.0.2", "10.0.0.1"},
		{"oversized chain", "10.0.0.1:4000", strings.Repeat("10.0.0.2,", 32) + "198.51.100.1", "10.0.0.1"},
		{"other headers ignored", "10.0.0.1:4000", "", "10.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.forwarded)
			r.Header.Set("X-Real-IP", "203.0.113.100")
			r.Header.Set("Forwarded", "for=203.0.113.100;proto=https")
			if got := app.clientIP(r); got != tc.want {
				t.Fatalf("client IP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequestRateLimitsCanBeDisabled(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		for _, tc := range []struct {
			name, method, path string
			authenticated      bool
			burst, status      int
		}{
			{"login", http.MethodPost, "/api/auth/login", false, 10, http.StatusBadRequest},
			{"guest send", http.MethodPost, "/api/messages", false, 10, http.StatusUnauthorized},
			{"account send", http.MethodPost, "/api/messages", true, 5, http.StatusBadRequest},
			{"WebSocket handshake", http.MethodGet, "/api/ws?room=%23unknown", false, 10, http.StatusBadRequest},
		} {
			t.Run(fmt.Sprintf("%s/disabled=%t", tc.name, disabled), func(t *testing.T) {
				app, _, token := lifecycleServer(t, &lifecycleBridge{})
				app.cfg.Security.DisableRateLimits = disabled
				handler := app.Handler()
				for i := range tc.burst + 1 {
					request := httptest.NewRequest(tc.method, "https://chat.example"+tc.path, nil)
					request.Header.Set("Origin", "https://chat.example")
					if tc.authenticated {
						request.AddCookie(&http.Cookie{Name: "session", Value: token})
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					want := tc.status
					if !disabled && i == tc.burst {
						want = http.StatusTooManyRequests
					}
					if response.Code != want {
						t.Fatalf("request %d: status=%d, want %d: %s", i+1, response.Code, want, response.Body.String())
					}
				}
			})
		}
	}
}

func TestAuthenticationLimitCannotBeBypassedByForwardedSpoof(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%t", trusted), func(t *testing.T) {
			cfg := Config{}
			if trusted {
				cfg.Security.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
			}
			app := New(cfg, nil, nil, nil, nil)
			t.Cleanup(app.cancel)
			handler := app.limitAuthentication(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			for i := range 11 {
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				r.RemoteAddr = "10.0.0.1:4000"
				r.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d, 192.0.2.1", i))
				w := httptest.NewRecorder()
				handler(w, r)
				want := http.StatusNoContent
				if i == 10 {
					want = http.StatusTooManyRequests
				}
				if w.Code != want {
					t.Fatalf("attempt %d: status %d, want %d", i+1, w.Code, want)
				}
			}
		})
	}
}

func TestSocketAdmissionLimitsReleaseIndependently(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cfg           SecurityConfig
		secondIP      string
		secondAccount int64
	}{
		{"global", SecurityConfig{MaxWebSockets: 1}, "192.0.2.2", 2},
		{"IP", SecurityConfig{MaxWebSocketsPerIP: 1}, "192.0.2.1", 2},
		{"account", SecurityConfig{MaxWebSocketsPerAccount: 1}, "192.0.2.2", 1},
		{"global with rates disabled", SecurityConfig{MaxWebSockets: 1, DisableRateLimits: true}, "192.0.2.2", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var admission socketAdmission
			cfg := tc.cfg.normalized()
			release, ok := admission.acquire("192.0.2.1", 1, cfg)
			if !ok {
				t.Fatal("first connection denied")
			}
			defer release()
			if second, ok := admission.acquire(tc.secondIP, tc.secondAccount, cfg); ok {
				second()
				t.Fatal("connection exceeded limit")
			}
			release()
			release()
			next, ok := admission.acquire(tc.secondIP, tc.secondAccount, cfg)
			if !ok {
				t.Fatal("released slot was not reusable")
			}
			next()
		})
	}
}

type leaseBridge struct {
	*lifecycleBridge
	acquire func(context.Context, irc.Account) (func(), error)
	retain  func(context.Context, int64) (func(), error)
}

func (b *leaseBridge) Acquire(ctx context.Context, account irc.Account) (func(), error) {
	if b.acquire != nil {
		return b.acquire(ctx, account)
	}
	return b.lifecycleBridge.Acquire(ctx, account)
}
func (b *leaseBridge) Retain(ctx context.Context, id int64) (func(), error) {
	if b.retain != nil {
		return b.retain(ctx, id)
	}
	return b.lifecycleBridge.Retain(ctx, id)
}

func TestFailedWebSocketUpgradesReleaseSlotsButConsumeHandshakeBudget(t *testing.T) {
	calls, held := 0, 0
	bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomReady}, acquire: func(context.Context, irc.Account) (func(), error) {
		calls++
		held++
		return func() { held-- }, nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	app.cfg.Security.MaxWebSockets = 1
	for i := range 11 {
		r := httptest.NewRequest(http.MethodGet, "https://chat.example/api/ws?room=%23one", nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		w := httptest.NewRecorder()
		app.websocket(w, r)
		want := http.StatusBadRequest
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: status %d, want %d", i+1, w.Code, want)
		}
		if held != 0 {
			t.Fatalf("failed upgrade leaked %d IRC leases", held)
		}
	}
	if calls != 10 {
		t.Fatalf("account acquisitions = %d, want 10", calls)
	}
}

func TestFailedAccountAcquisitionReleasesWebSocketAdmission(t *testing.T) {
	calls := 0
	bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomReady}, acquire: func(context.Context, irc.Account) (func(), error) {
		calls++
		if calls == 1 {
			return nil, irc.ErrAccountCapacity
		}
		return func() {}, nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	app.cfg.Security.MaxWebSockets = 1
	for _, want := range []int{http.StatusTooManyRequests, http.StatusBadRequest} {
		r := httptest.NewRequest(http.MethodGet, "https://chat.example/api/ws?room=%23one", nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
		w := httptest.NewRecorder()
		app.websocket(w, r)
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
}

func TestCursorRateLimitCanBeDisabled(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%t", disabled), func(t *testing.T) {
			app, _, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
			app.cfg.Security.DisableRateLimits = disabled
			app.cfg.Security.CursorUpdatesPerMinute = 1
			sub := &subscription{room: "#one", out: make(chan frame, 1), done: make(chan struct{}), cursors: make(map[string]int64)}
			for i := range 31 {
				err := app.markRead(t.Context(), sub, "#one", 0)
				if !disabled && i == 30 {
					if !errors.Is(err, errCursorRateLimited) {
						t.Fatalf("cursor flood error = %v, want rate limit", err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("cursor update %d: %v", i+1, err)
				}
				select {
				case update := <-sub.out:
					if update.Type != "room" || update.Room == nil || update.Room.Name != "#one" {
						t.Fatalf("unexpected cursor response: %+v", update)
					}
				default:
					t.Fatal("cursor update did not return room state")
				}
			}
		})
	}
}

func TestPendingSendCancellationReleasesCapacityAndIRCLease(t *testing.T) {
	var held, sent atomic.Int64
	bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error { sent.Add(1); return nil }}, retain: func(context.Context, int64) (func(), error) {
		held.Add(1)
		return func() { held.Add(-1) }, nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	app.cfg.Security.MaxPendingSends = 1
	app.cfg.Security.DisableRateLimits = true
	started := make(chan struct{})
	app.translator = translationFunc(func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "https://chat.example/api/messages", strings.NewReader(`{"room":"#one","text":"안녕하세요 여러분.","requestId":"cancelled_send_01"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "session", Value: token})
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); app.send(response, r) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("translation did not start")
	}
	blocked, _ := postOutgoing(t, app, token, "hello", "blocked_send_001", true)
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") == "" {
		t.Fatalf("pending limit: %d %s", blocked.Code, blocked.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("cancelled operation did not finish")
	}
	if held.Load() != 0 || sent.Load() != 0 {
		t.Fatalf("cancelled send: held=%d sent=%d", held.Load(), sent.Load())
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancelled status = %d, want 503", response.Code)
	}
	next, _ := postOutgoing(t, app, token, "hello", "released_send_01", true)
	if next.Code != http.StatusOK || sent.Load() != 1 || held.Load() != 0 {
		t.Fatalf("reused send capacity: status=%d sent=%d held=%d", next.Code, sent.Load(), held.Load())
	}
}

func TestPurgedRequestIDsNeverReplay(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("claim_race=%t", race), func(t *testing.T) {
			bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error { t.Fatal("purged request reached IRC"); return nil }}}
			app, user, token := lifecycleServer(t, bridge)
			id := "purged_request_01"
			purge := func() {
				old := time.Now().Add(-48 * time.Hour).UnixMilli()
				if _, err := app.q.ClaimSend(t.Context(), store.ClaimSendParams{UserID: user.ID, RequestID: id, Room: "#one", Nick: user.Nick, Original: "hello", OriginalMode: 1, State: "sending", CreatedAt: old, UpdatedAt: old, ExpiresAt: old + 120000}); err != nil {
					t.Fatal(err)
				}
				if _, err := app.q.FinishSend(t.Context(), store.FinishSendParams{UserID: user.ID, RequestID: id, State: "failed", UpdatedAt: old, ExpiresAt: old + 120000}); err != nil {
					t.Fatal(err)
				}
				if _, err := app.q.Prune(t.Context(), time.Now(), store.RetentionPolicy{SendPayloads: 24 * time.Hour, BatchSize: 10}); err != nil {
					t.Fatal(err)
				}
			}
			if race {
				bridge.retain = func(context.Context, int64) (func(), error) { purge(); return func() {}, nil }
			} else {
				purge()
			}
			w, _ := postOutgoing(t, app, token, "hello", id, true)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"request_expired"`) {
				t.Fatalf("purged duplicate: %d %s", w.Code, w.Body.String())
			}
			request := httptest.NewRequest(http.MethodGet, "https://chat.example/api/sends/"+id, nil)
			request.AddCookie(&http.Cookie{Name: "session", Value: token})
			status := httptest.NewRecorder()
			app.Handler().ServeHTTP(status, request)
			if status.Code != http.StatusGone || !strings.Contains(status.Body.String(), `"code":"request_expired"`) {
				t.Fatalf("purged status: %d %s", status.Code, status.Body.String())
			}
		})
	}
}

func TestRequestLimiterBoundsKeyChurnWithoutResettingLiveBudgets(t *testing.T) {
	var limiter requestLimiter
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 4096 {
		if !limiter.allow(stringID(int64(i)), now, 1, 1) {
			t.Fatalf("key %d denied before capacity", i)
		}
	}
	if limiter.allow("new", now, 1, 1) {
		t.Fatal("key churn exceeded bounded limiter capacity")
	}
	if limiter.allow("0", now.Add(59*time.Second), 1, 1) {
		t.Fatal("live key lost its rate limit")
	}
	if !limiter.allow("new", now.Add(time.Minute), 1, 1) {
		t.Fatal("expired idle keys prevented admission")
	}
}

func TestCursorFloodClosesSocketWithPolicyViolationAndReleasesLease(t *testing.T) {
	released := make(chan struct{})
	bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomReady}, acquire: func(context.Context, irc.Account) (func(), error) {
		return func() { close(released) }, nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	app.cfg.Security.CursorUpdatesPerMinute = 1
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	header := http.Header{"Origin": {"https://chat.example"}, "Cookie": {"session=" + token}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/ws?room=%23one", header)
	if err != nil {
		t.Fatal(err)
	}
	if response.Body != nil {
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for {
		var f frame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatal(err)
		}
		if f.Type == "rooms" {
			break
		}
	}
	for range 30 {
		if err := conn.WriteJSON(map[string]any{"type": "read", "room": "#one", "messageId": 0}); err != nil {
			t.Fatal(err)
		}
		var f frame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatal(err)
		}
		if f.Type != "room" {
			t.Fatalf("cursor response type = %q, want room", f.Type)
		}
	}
	if err := conn.WriteJSON(map[string]any{"type": "read", "room": "#one", "messageId": 0}); err != nil {
		t.Fatal(err)
	}
	var f frame
	if err := conn.ReadJSON(&f); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("cursor flood close = %v, want 1008", err)
	}
	select {
	case <-released:
	case <-time.After(time.Minute):
		t.Fatal("closed socket retained IRC lease")
	}
}

func TestInvalidSendsConsumeAccountAttemptBudgetWithoutHoldingCapacity(t *testing.T) {
	app, _, token := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error { t.Fatal("invalid send reached IRC"); return nil }})
	app.cfg.Security.MaxPendingSends = 1
	for i := range 6 {
		w, _ := postOutgoing(t, app, token, "", "invalid_send_001", true)
		want := http.StatusBadRequest
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: status %d, want %d", i+1, w.Code, want)
		}
	}
}

func TestAnonymousSendsConsumeIPBudgetDespiteForwardedSpoof(t *testing.T) {
	app, _, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	for i := range 11 {
		r := httptest.NewRequest(http.MethodPost, "https://chat.example/api/messages", nil)
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		w := httptest.NewRecorder()
		app.send(w, r)
		want := http.StatusUnauthorized
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: status %d, want %d", i+1, w.Code, want)
		}
	}
}
