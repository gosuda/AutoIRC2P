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

	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

type lifecycleBridge struct {
	state irc.MembershipState
	send  func(context.Context, int64, string, string) error
}

func (b *lifecycleBridge) Connect(context.Context, irc.Account) error  { return nil }
func (b *lifecycleBridge) RoomState(int64, string) irc.MembershipState { return b.state }
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
	user, err := a.Register(t.Context(), "alice@example.org", "alice", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.CreateSession(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := New(Config{Origin: "https://chat.example", Rooms: []string{"#one", "#two"}}, q, a, &translationStub{}, b)
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

func TestAmbiguousWriteCanConfirmOnLateObserverEcho(t *testing.T) {
	bridge := &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error { return errors.New("partial write") }}
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

func TestReadCursorCountsMessagesAndNeverMovesBackward(t *testing.T) {
	app, user, _ := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	add := func(room string, sender, service int64) int64 {
		t.Helper()
		row, err := app.q.AddMessage(t.Context(), store.AddMessageParams{Room: room, Nick: "someone", Original: "hello", SourceLanguage: "en", WireLanguage: "en", CreatedAt: time.Now().UnixMilli(), SenderUserID: sender, Service: service})
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
