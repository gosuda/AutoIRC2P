package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

type recordingBridge struct{ sent chan string }

func (b *recordingBridge) Acquire(context.Context, irc.Account) (func(), error) {
	return func() {}, nil
}
func (b *recordingBridge) Retain(context.Context, int64) (func(), error) { return func() {}, nil }
func (b *recordingBridge) RoomState(int64, string) irc.MembershipState   { return irc.RoomReady }
func (b *recordingBridge) Send(_ context.Context, _ int64, _, text string) error {
	b.sent <- text
	return nil
}

type translationStub struct{ calls atomic.Int64 }

func (tr *translationStub) Translate(context.Context, string, string) (string, error) {
	tr.calls.Add(1)
	return "Good morning everyone.", nil
}

func TestHTTPOnlyExplicitAuthenticatedSendReachesIRC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, q, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	a, err := auth.New(q, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	user, err := a.Register(ctx, "humanNick", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.CreateSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &recordingBridge{sent: make(chan string, 10)}
	tr := &translationStub{}
	app := New(Config{AllowOrigin: func(origin string) bool { return origin == "https://chat.example" }, Rooms: []string{"#private-test"}}, q, a, tr, bridge)
	done := make(chan struct{})
	go func() { defer close(done); app.Run(ctx) }()
	defer func() { cancel(); <-done }()
	httpServer := httptest.NewServer(app.Handler())
	defer httpServer.Close()
	request := func(body, origin string, authenticated bool) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/api/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "session", Value: token})
		}
		resp, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Error(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		return resp.StatusCode
	}
	body := `{"room":"#private-test","text":"안녕하세요 여러분.","original":false,"requestId":"00000000-0000-4000-8000-000000000001"}`
	if status := request(body, "https://chat.example", false); status != 401 {
		t.Fatalf("anonymous send status %d", status)
	}
	if status := request(body, "https://evil.example", true); status != 403 {
		t.Fatalf("cross-origin send status %d", status)
	}
	if status := request(body, "https://chat.example", true); status != 200 {
		t.Fatalf("authenticated send status %d", status)
	}
	if got := <-bridge.sent; got != "Good morning everyone." {
		t.Fatalf("wire translation %q", got)
	}
	if status := request(body, "https://chat.example", true); status != 200 {
		t.Fatalf("idempotent send status %d", status)
	}
	if len(bridge.sent) != 0 {
		t.Fatal("duplicate request sent another IRC message")
	}
	injected := strings.ReplaceAll(body, "안녕하세요 여러분.", `hello\r\nPRIVMSG #i2p :injected`)
	if status := request(injected, "https://chat.example", true); status != 400 {
		t.Fatalf("IRC injection status %d", status)
	}
	raw := strings.ReplaceAll(strings.ReplaceAll(body, `"original":false`, `"original":true`), "000000000001", "000000000002")
	if status := request(raw, "https://chat.example", true); status != 200 {
		t.Fatalf("original send status %d", status)
	}
	if got := <-bridge.sent; got != "안녕하세요 여러분." {
		t.Fatalf("original wire %q", got)
	}
	if tr.calls.Load() != 1 {
		t.Fatalf("original send invoked translator; calls %d", tr.calls.Load())
	}
	header := http.Header{"Origin": []string{"https://chat.example"}}
	conn, resp, err := websocket.DefaultDialer.Dial(strings.Replace(httpServer.URL, "http", "ws", 1)+"/api/ws?room=%23private-test&lang=en", header)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Body != nil {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
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
	var f frame
	if err := conn.ReadJSON(&f); err != nil {
		t.Fatal(err)
	}
	app.Event(ctx, irc.Event{Kind: "message", Room: "#private-test", Nick: "humanNick", Text: "Good morning everyone."})
	for {
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatal(err)
		}
		if f.Type == "message" {
			break
		}
	}
	if f.Message == nil || f.Message.Original != "안녕하세요 여러분." {
		data, _ := json.Marshal(f)
		t.Fatalf("original lost on observer echo: %s", data)
	}
}

func TestReaderLanguageControlsHistoryAndLiveTranslation(t *testing.T) {
	cases := []struct {
		name, lang, translation string
		disabled                bool
	}{
		{"original", "original", "", false},
		{"en", "en", "Hello, everyone.", false},
		{"ko", "ko", "안녕하세요 여러분.", false},
		{"disabled with stale language", "en", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
			var calls atomic.Int64
			app.translator = translationFunc(func(_ context.Context, text, target string) (string, error) {
				calls.Add(1)
				if target == "ko" {
					return text, nil
				}
				return "Hello, everyone.", nil
			})
			if tc.disabled {
				app.translator = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); app.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			server := httptest.NewServer(app.Handler())
			t.Cleanup(server.Close)
			original := "안녕하세요 여러분."
			if _, err := app.q.AddMessage(ctx, store.AddMessageParams{Room: "#one", Nick: "bob", Original: original, CreatedAt: time.Now().UnixMilli()}); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/messages?room=%23one&lang="+tc.lang, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("history status = %d, body = %s", response.Code, response.Body.String())
			}
			var history struct{ Messages []Message }
			if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
				t.Fatal(err)
			}
			if len(history.Messages) != 1 {
				t.Fatalf("history messages = %d, want 1", len(history.Messages))
			}
			state, target := "pending", tc.lang
			if tc.lang == "original" || tc.disabled {
				state, target = "excluded", ""
			}
			assertInitial := func(msg *Message) {
				t.Helper()
				if msg == nil || msg.Original != original || msg.Translation != "" || msg.TargetLanguage != target || msg.TranslationState != state {
					t.Fatalf("initial message = %+v, want target %q and state %q", msg, target, state)
				}
			}
			assertInitial(&history.Messages[0])
			conn, responseWS, err := websocket.DefaultDialer.Dial(strings.Replace(server.URL, "http", "ws", 1)+"/api/ws?room=%23one&lang="+tc.lang, http.Header{"Origin": []string{"https://chat.example"}})
			if err != nil {
				t.Fatal(err)
			}
			if responseWS.Body != nil {
				if err := responseWS.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := conn.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			var event frame
			if err := conn.ReadJSON(&event); err != nil {
				t.Fatal(err)
			}
			app.Event(ctx, irc.Event{Kind: "message", Room: "#one", Nick: "bob", Text: original})
			var liveID int64
			for {
				if err := conn.ReadJSON(&event); err != nil {
					t.Fatal(err)
				}
				if event.Type == "message" {
					assertInitial(event.Message)
					liveID = event.Message.ID
					if tc.lang == "original" || tc.disabled {
						break
					}
				}
				if event.Type == "translation" && event.Message != nil && event.Message.ID == liveID {
					if event.Message.TranslationState != "ready" || event.Message.Translation != tc.translation || event.Message.TargetLanguage != target {
						t.Fatalf("live translation = %+v, want %q in %q", event.Message, tc.translation, target)
					}
					break
				}
			}
			cancel()
			<-done
			if (tc.lang == "original" || tc.disabled) && calls.Load() != 0 {
				t.Fatalf("original-only reader invoked translator %d times", calls.Load())
			}
		})
	}
}

func TestNicknameAuthenticationDoesNotDiscloseInternalEmail(t *testing.T) {
	app, _, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	proof := strings.Repeat("cd", 32)
	handler := app.Handler()
	var cookie *http.Cookie
	for _, step := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/auth/register", `{"nick":"newAccount","passwordHash":"` + proof + `"}`},
		{http.MethodPost, "/api/auth/login", `{"nick":" NEWACCOUNT ","passwordHash":"` + proof + `"}`},
		{http.MethodGet, "/api/session", ""},
	} {
		req := httptest.NewRequest(step.method, "https://chat.example"+step.path, strings.NewReader(step.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://chat.example")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d: %s", step.path, response.Code, response.Body.String())
		}
		stored, err := app.q.UserByNick(t.Context(), "newAccount")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			User map[string]any `json:"user"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.User) != 2 || body.User["id"] != float64(stored.ID) || body.User["nick"] != "newAccount" {
			t.Fatalf("%s exposed unexpected public user fields: %v", step.path, body.User)
		}
		if strings.Contains(response.Body.String(), stored.Email) {
			t.Fatalf("%s disclosed the internal email", step.path)
		}
		if step.method == http.MethodPost {
			cookies := (&http.Response{Header: response.Header()}).Cookies()
			cookie = nil
			for _, candidate := range cookies {
				if candidate.Name == "session" {
					cookie = candidate
				}
			}
			if cookie == nil {
				t.Fatalf("%s did not create a session", step.path)
			}
		}
	}
}

func TestAuthenticationRejectsEmailCredentials(t *testing.T) {
	app, _, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	for _, path := range []string{"/api/auth/register", "/api/auth/login"} {
		req := httptest.NewRequest(http.MethodPost, "https://chat.example"+path, strings.NewReader(`{"email":"person@example.org","nick":"anotherNick","passwordHash":"`+strings.Repeat("ab", 32)+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://chat.example")
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted email credentials: status=%d", path, response.Code)
		}
	}
}
