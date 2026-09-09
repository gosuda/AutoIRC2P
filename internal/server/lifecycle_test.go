package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

type lifecycleBridge struct {
	state         irc.MembershipState
	observerState irc.MembershipState
	send          func(context.Context, int64, string, string) error
}

func (b *lifecycleBridge) Acquire(context.Context, irc.Account) (func(), error) {
	return func() {}, nil
}
func (b *lifecycleBridge) Retain(context.Context, int64) (func(), error) { return func() {}, nil }
func (b *lifecycleBridge) RoomState(accountID int64, _ string) irc.MembershipState {
	if accountID == 0 && b.observerState != "" {
		return b.observerState
	}
	return b.state
}
func (b *lifecycleBridge) Send(ctx context.Context, id int64, room, text string) error {
	return b.send(ctx, id, room, text)
}

func lifecycleServer(t *testing.T, b Bridge) (*Server, store.User, string) {
	t.Helper()
	db, q, err := store.NewSQLite(t.Context(), filepath.Join(t.TempDir(), "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	a, err := auth.New(q, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	user, err := a.Register(t.Context(), "alice", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.CreateSession(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := New(Config{AllowOrigin: func(origin string) bool { return origin == "https://chat.example" }, Rooms: []string{"#one", "#two"}}, q, a, &translationStub{}, b)
	t.Cleanup(app.cancel)
	return app, user, token
}

func postOutgoing(t *testing.T, app *Server, token, text, id string, original bool) (*httptest.ResponseRecorder, Outgoing) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"room": "#one", "text": text, "requestId": id, "original": original})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://chat.example/api/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	response := httptest.NewRecorder()
	app.send(response, req)
	var result struct {
		Send Outgoing `json:"send"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return response, result.Send
}

func TestEarlyObserverEchoRemainsConfirmedAfterWriteReturns(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomReady}
	app, user, token := lifecycleServer(t, bridge)
	sub := &subscription{userID: user.ID, room: "#one", lang: "en", out: make(chan frame, 32), done: make(chan struct{}), cursors: map[string]int64{"#one": 0, "#two": 0}}
	app.subscribers[sub] = struct{}{}
	sends := 0
	bridge.send = func(ctx context.Context, _ int64, room, text string) error {
		sends++
		app.receive(ctx, irc.Event{Kind: "message", Room: room, Nick: user.Nick, Text: text})
		return nil
	}
	response, outgoing := postOutgoing(t, app, token, "Good morning everyone.", "early_echo_request_01", true)
	if response.Code != 200 || outgoing.State != "confirmed" || outgoing.MessageID <= 0 {
		t.Fatalf("early echo response: %d %s", response.Code, response.Body.String())
	}
	confirmed := false
	for len(sub.out) > 0 {
		f := <-sub.out
		if f.Send == nil {
			continue
		}
		if confirmed && f.Send.State != "confirmed" {
			t.Fatalf("confirmation regressed to %s", f.Send.State)
		}
		confirmed = confirmed || f.Send.State == "confirmed"
	}
	if !confirmed {
		t.Fatal("owner never received confirmation")
	}
	response, again := postOutgoing(t, app, token, "Good morning everyone.", "early_echo_request_01", true)
	if response.Code != 200 || again.MessageID != outgoing.MessageID || sends != 1 {
		t.Fatalf("duplicate resend: status=%d send=%+v calls=%d", response.Code, again, sends)
	}
	response, _ = postOutgoing(t, app, token, "Different payload.", "early_echo_request_01", true)
	if response.Code != 409 || sends != 1 {
		t.Fatalf("request ID reused: status=%d calls=%d", response.Code, sends)
	}
	rows, err := app.q.Messages(t.Context(), store.MessagesParams{Room: "#one", ID: outgoing.MessageID + 1})
	if err != nil {
		t.Fatal(err)
	}
	own := personalize(messageFrom(rows[0], "en"), user.ID)
	guest := personalize(messageFrom(rows[0], "en"), 0)
	if !own.Own || own.RequestID != outgoing.RequestID || guest.Own || guest.RequestID != "" {
		t.Fatalf("ownership privacy: own=%+v guest=%+v", own, guest)
	}
}

func TestPreparingRoomDoesNotTranslateOrSend(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomPreparing, send: func(context.Context, int64, string, string) error {
		t.Fatal("wire send before JOIN readiness")
		return nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	response, _ := postOutgoing(t, app, token, "안녕하세요 여러분.", "not_ready_request_01", false)
	if response.Code != 409 {
		t.Fatalf("preparing send: %d %s", response.Code, response.Body.String())
	}
	if app.translator.(*translationStub).calls.Load() != 0 {
		t.Fatal("provider called before readiness")
	}
}

func TestSendingDependsOnSenderMembershipNotObserver(t *testing.T) {
	for _, tc := range []struct {
		name               string
		sender, observer   irc.MembershipState
		wantStatus, writes int
	}{
		{"reader still joining", irc.RoomReady, irc.RoomPreparing, http.StatusOK, 1},
		{"reader unavailable", irc.RoomReady, irc.RoomUnavailable, http.StatusOK, 1},
		{"sender still joining", irc.RoomPreparing, irc.RoomReady, http.StatusConflict, 0},
		{"sender removed", irc.RoomUnavailable, irc.RoomReady, http.StatusConflict, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			bridge := &lifecycleBridge{state: tc.sender, observerState: tc.observer, send: func(_ context.Context, _ int64, room, text string) error {
				writes++
				if room != "#one" || text != "hello without the reader" {
					t.Fatalf("IRC write = %q %q", room, text)
				}
				return nil
			}}
			app, _, token := lifecycleServer(t, bridge)
			app.translator = nil
			response, outgoing := postOutgoing(t, app, token, "hello without the reader", "independent_readiness_01", true)
			if response.Code != tc.wantStatus || writes != tc.writes {
				t.Fatalf("send status=%d writes=%d, want %d/%d: %s", response.Code, writes, tc.wantStatus, tc.writes, response.Body.String())
			}
			if tc.wantStatus == http.StatusOK && outgoing.State != "awaiting_echo" {
				t.Fatalf("write without observer confirmation = %q, want awaiting_echo", outgoing.State)
			}
		})
	}
}

var errPartialWrite = errors.New("partial write")

func TestAmbiguousWriteCanConfirmOnLateObserverEcho(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error { return errPartialWrite }}
	app, user, token := lifecycleServer(t, bridge)
	response, outgoing := postOutgoing(t, app, token, "Good morning everyone.", "ambiguous_request_01", true)
	if response.Code != 503 || outgoing.State != "unconfirmed" || outgoing.MessageID != 0 {
		t.Fatalf("ambiguous send: %d %s", response.Code, response.Body.String())
	}
	app.receive(t.Context(), irc.Event{Kind: "message", Room: "#one", Nick: user.Nick, Text: "Good morning everyone."})
	row, err := app.q.GetSend(t.Context(), store.GetSendParams{UserID: user.ID, RequestID: outgoing.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "confirmed" || row.MessageID == 0 || row.ErrorCode != "" {
		t.Fatalf("late echo did not confirm: %+v", row)
	}
}

type translationFunc func(context.Context, string, string) (string, error)

func (f translationFunc) Translate(ctx context.Context, text, lang string) (string, error) {
	return f(ctx, text, lang)
}

func TestRoomLosingReadinessDuringTranslationDoesNotWrite(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error {
		t.Fatal("wire send after membership loss")
		return nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	app.translator = translationFunc(func(context.Context, string, string) (string, error) {
		bridge.state = irc.RoomUnavailable
		return "Good morning everyone.", nil
	})
	response, outgoing := postOutgoing(t, app, token, "안녕하세요 여러분.", "lost_ready_request_01", false)
	if response.Code != 409 || outgoing.State != "failed" || outgoing.ErrorCode != "not_ready" {
		t.Fatalf("lost readiness: %d %s", response.Code, response.Body.String())
	}
}

func TestTranslatedSendInTargetLanguageDoesNotBypassProviderFailure(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error {
		t.Fatal("wire send after translation failure")
		return nil
	}}
	app, _, token := lifecycleServer(t, bridge)
	app.translator = translationFunc(func(context.Context, string, string) (string, error) {
		return "", context.DeadlineExceeded
	})
	response, outgoing := postOutgoing(t, app, token, "This message is already written in English.", "same_language_request_01", false)
	if response.Code != http.StatusServiceUnavailable || outgoing.State != "failed" || outgoing.ErrorCode != "translation_failed" {
		t.Fatalf("translation failure: %d %s", response.Code, response.Body.String())
	}
}

func TestDisabledTranslationSendsOriginalForStaleClient(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomReady}
	app, user, token := lifecycleServer(t, bridge)
	app.translator = nil
	sends := 0
	bridge.send = func(ctx context.Context, _ int64, room, text string) error {
		sends++
		if text != "번역 없이 보내는 메시지" {
			t.Fatalf("wire text = %q, want unchanged original", text)
		}
		app.receive(ctx, irc.Event{Kind: "message", Room: room, Nick: user.Nick, Text: text})
		return nil
	}
	response, outgoing := postOutgoing(t, app, token, "번역 없이 보내는 메시지", "disabled_translation_01", false)
	if response.Code != http.StatusOK || outgoing.State != "confirmed" {
		t.Fatalf("original send without provider: %d %s", response.Code, response.Body.String())
	}
	response, repeated := postOutgoing(t, app, token, "번역 없이 보내는 메시지", "disabled_translation_01", false)
	if response.Code != http.StatusOK || repeated.MessageID != outgoing.MessageID || sends != 1 {
		t.Fatalf("duplicate original send: status=%d send=%+v wire sends=%d", response.Code, repeated, sends)
	}
	bridge.state = irc.RoomPreparing
	response, _ = postOutgoing(t, app, token, "아직 입장하지 않은 채널", "disabled_not_ready_01", false)
	if response.Code != http.StatusConflict || sends != 1 {
		t.Fatalf("unready original send: status=%d wire sends=%d", response.Code, sends)
	}
}

func TestReadCursorCountsMessagesAndNeverMovesBackward(t *testing.T) {
	app, user, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	add := func(room string, sender, service int64) int64 {
		t.Helper()
		row, err := app.q.AddMessage(t.Context(), store.AddMessageParams{Room: room, Nick: "someone", Original: "hello", CreatedAt: time.Now().UnixMilli(), SenderUserID: sender, Service: service})
		if err != nil {
			t.Fatal(err)
		}
		return row.ID
	}
	first := add("#one", 0, 0)
	add("#two", 0, 0)
	add("#one", user.ID, 0)
	add("#one", 0, 1)
	last := add("#one", 0, 0)
	sub := &subscription{userID: user.ID, room: "#one", out: make(chan frame, 16), done: make(chan struct{}), cursors: map[string]int64{"#one": 0}}
	room, err := app.roomMetadata(t.Context(), user.ID, "#one", sub.cursors)
	if err != nil {
		t.Fatal(err)
	}
	if room.UnreadCount != 2 {
		t.Fatalf("unread count includes ID gaps, own, or service messages: %+v", room)
	}
	for _, cursor := range []int64{first, last, first} {
		if err := app.markRead(t.Context(), sub, "#one", cursor); err != nil {
			t.Fatal(err)
		}
	}
	room, err = app.roomMetadata(t.Context(), user.ID, "#one", sub.cursors)
	if err != nil {
		t.Fatal(err)
	}
	if room.UnreadCount != 0 || sub.cursors["#one"] != last {
		t.Fatalf("read cursor regressed: %+v cursor=%d", room, sub.cursors["#one"])
	}
	baseline, err := app.roomMetadata(t.Context(), user.ID, "#one", map[string]int64{})
	if err != nil {
		t.Fatal(err)
	}
	if baseline.UnreadCount != 0 || baseline.LatestMessageID != last {
		t.Fatalf("missing cursor did not establish baseline: %+v", baseline)
	}
	if err := app.markRead(t.Context(), sub, "#one", last+1000); err != nil {
		t.Fatal(err)
	}
	next := add("#one", 0, 0)
	room, err = app.roomMetadata(t.Context(), user.ID, "#one", sub.cursors)
	if err != nil {
		t.Fatal(err)
	}
	if room.UnreadCount != 1 || room.LatestMessageID != next {
		t.Fatalf("future cursor hid a later incoming message: %+v", room)
	}
}

func TestAccountNetworkStatusStaysIndependentAcrossReconnects(t *testing.T) {
	released := make(chan struct{}, 8)
	bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomPreparing}, acquire: func(context.Context, irc.Account) (func(), error) {
		return func() { released <- struct{}{} }, nil
	}}
	app, user, token := lifecycleServer(t, bridge)
	app.receive(t.Context(), irc.Event{Kind: "status", State: "connected", Text: "observer connection"})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); app.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	snapshot := func(authenticated bool, want string) network {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "session", Value: token})
		}
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("session status = %d, body = %s", response.Code, response.Body.String())
		}
		var session struct {
			Network network
			Rooms   []Room
		}
		if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
			t.Fatal(err)
		}
		if session.Network.State != want {
			t.Errorf("authenticated=%v session network = %+v, want %q", authenticated, session.Network, want)
		}
		for _, room := range session.Rooms {
			wantSend := "login_required"
			if authenticated {
				wantSend = "preparing"
			}
			if room.SendState != wantSend {
				t.Errorf("authenticated=%v room %q send state = %q, want %q before JOIN", authenticated, room.Name, room.SendState, wantSend)
			}
		}
		return session.Network
	}
	readStatus := func(conn *websocket.Conn, want network) {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		for {
			var f frame
			if err := conn.ReadJSON(&f); err != nil {
				t.Fatal(err)
			}
			if f.Type != "status" {
				continue
			}
			if f.State != want.State || f.Detail != want.Detail {
				t.Errorf("websocket status = %q (%q), session = %+v", f.State, f.Detail, want)
			}
			return
		}
	}
	var closeAccounts []func()
	connect := func(authenticated bool, want string) *websocket.Conn {
		t.Helper()
		header := http.Header{"Origin": {"https://chat.example"}}
		if authenticated {
			header.Set("Cookie", "session="+token)
		}
		conn, response, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/ws?room=%23one", header)
		if err != nil {
			t.Fatal(err)
		}
		closed := false
		closeConnection := func() {
			if closed {
				return
			}
			closed = true
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(closeConnection)
		if authenticated {
			closeAccounts = append(closeAccounts, closeConnection)
		}
		if response.Body != nil {
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
		}
		readStatus(conn, snapshot(authenticated, want))
		return conn
	}
	account := connect(true, "connecting")
	guest := connect(false, "connected")
	for _, step := range []struct {
		name                   string
		accountID              int64
		state, personal, guest string
	}{
		{"observer update before personal startup", 0, "connected", "connecting", "connected"},
		{"personal startup", user.ID, "connecting", "connecting", "connected"},
		{"observer disconnect during startup", 0, "disconnected", "connecting", "disconnected"},
		{"personal connected before JOIN", user.ID, "connected", "connected", "disconnected"},
		{"observer recovery", 0, "connected", "connected", "connected"},
		{"personal disconnect", user.ID, "disconnected", "disconnected", "connected"},
		{"observer update during personal disconnect", 0, "connected", "disconnected", "connected"},
		{"personal stop", user.ID, "stopped", "stopped", "connected"},
		{"observer update after personal stop", 0, "connected", "stopped", "connected"},
		{"personal reconnect", user.ID, "connecting", "connecting", "connected"},
		{"personal connection restored", user.ID, "connected", "connected", "connected"},
		{"observer disconnect after personal recovery", 0, "disconnected", "connected", "disconnected"},
	} {
		t.Log(step.name)
		app.receive(ctx, irc.Event{Kind: "status", AccountID: step.accountID, State: step.state, Text: step.name})
		readStatus(account, snapshot(true, step.personal))
		guestStatus := snapshot(false, step.guest)
		if step.accountID == 0 {
			readStatus(guest, guestStatus)
		}
		if step.accountID == user.ID && step.state == "stopped" {
			connect(true, "stopped")
		}
	}
	app.receive(ctx, irc.Event{Kind: "status", AccountID: user.ID, State: "stopped"})
	readStatus(account, snapshot(true, "stopped"))
	for _, closeAccount := range closeAccounts {
		closeAccount()
	}
	for range closeAccounts {
		select {
		case <-released:
		case <-time.After(5 * time.Second):
			t.Fatal("closed socket retained its IRC account lease")
		}
	}
	snapshot(true, "connecting")
	connect(true, "connecting")
}
