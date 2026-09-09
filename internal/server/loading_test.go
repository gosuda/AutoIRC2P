package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

type membershipBridge struct {
	*lifecycleBridge
	states map[int64]map[string]irc.MembershipState
}

func (b *membershipBridge) RoomState(userID int64, room string) irc.MembershipState {
	if state := b.states[userID][room]; state != "" {
		return state
	}
	return irc.RoomPreparing
}

func TestRoomReadinessChangesRemainAvailableWithoutDatabase(t *testing.T) {
	bridge := &membershipBridge{lifecycleBridge: &lifecycleBridge{}, states: make(map[int64]map[string]irc.MembershipState)}
	app, user, _ := lifecycleServer(t, bridge)
	message, err := app.q.AddMessage(t.Context(), store.AddMessageParams{Room: "#one", Nick: user.Nick, Original: "hello", SenderUserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	subscribe := func(userID int64) *subscription {
		t.Helper()
		sub := &subscription{userID: userID, room: "#one", out: make(chan frame, 16), done: make(chan struct{}), cursors: map[string]int64{"#one": 0, "#two": 0}}
		rooms, err := app.allRooms(t.Context(), userID, sub.cursors)
		if err != nil {
			t.Fatal(err)
		}
		wantUnread := int64(1)
		if userID == user.ID {
			wantUnread = 0
		}
		if rooms[0].UnreadCount != wantUnread || rooms[0].LatestMessageID != message.ID {
			t.Fatalf("initial counters for viewer %d = %+v", userID, rooms[0])
		}
		for _, room := range rooms {
			sub.rememberRoomState(room)
		}
		app.subscribers[sub] = struct{}{}
		return sub
	}
	owner, other, guest := subscribe(user.ID), subscribe(user.ID+1), subscribe(0)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	app.q = store.New(db)
	assertOpenAndEmpty := func(sub *subscription) {
		t.Helper()
		select {
		case <-sub.done:
			t.Fatalf("viewer %d disconnected during readiness update", sub.userID)
		default:
		}
		select {
		case f := <-sub.out:
			t.Fatalf("viewer %d received redundant frame: %+v", sub.userID, f)
		default:
		}
		if sub.cursors["#one"] != 0 || sub.cursors["#two"] != 0 {
			t.Fatalf("readiness update changed viewer %d cursors: %v", sub.userID, sub.cursors)
		}
	}
	assertStates := func(sub *subscription, want ...roomState) {
		t.Helper()
		var f frame
		select {
		case f = <-sub.out:
		default:
			t.Fatalf("viewer %d did not receive readiness changes", sub.userID)
		}
		if f.Type != "roomStates" || len(f.RoomStates) != len(want) {
			t.Fatalf("viewer %d update = %+v, want %d room states", sub.userID, f, len(want))
		}
		for i, state := range want {
			if f.RoomStates[i] != state {
				t.Errorf("viewer %d readiness = %+v, want %+v", sub.userID, f.RoomStates[i], state)
			}
		}
		payload, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"latestMessageId", "unreadCount", "language", `"rooms":`, `"room":`} {
			if strings.Contains(string(payload), field) {
				t.Errorf("readiness frame overwrites metadata: %s", payload)
			}
		}
		assertOpenAndEmpty(sub)
	}
	assertStatus := func(sub *subscription, want string) {
		t.Helper()
		select {
		case f := <-sub.out:
			if f.Type != "status" || f.State != want {
				t.Fatalf("viewer %d network = %+v, want %q", sub.userID, f, want)
			}
		default:
			t.Fatalf("viewer %d did not receive network status", sub.userID)
		}
	}

	bridge.states[user.ID] = map[string]irc.MembershipState{"#one": irc.RoomReady}
	app.receive(t.Context(), irc.Event{Kind: "membership", AccountID: user.ID, Room: "#one"})
	assertStates(owner, roomState{Name: "#one", ReadState: "loading", SendState: "ready"})
	assertOpenAndEmpty(other)
	assertOpenAndEmpty(guest)
	app.receive(t.Context(), irc.Event{Kind: "status", AccountID: user.ID, State: "connected"})
	assertStatus(owner, "connected")
	assertOpenAndEmpty(owner)
	assertOpenAndEmpty(other)
	assertOpenAndEmpty(guest)

	bridge.states[0] = map[string]irc.MembershipState{"#one": irc.RoomReady}
	app.receive(t.Context(), irc.Event{Kind: "membership", Room: "#one"})
	assertStates(owner, roomState{Name: "#one", ReadState: "ready", SendState: "ready"})
	assertStates(other, roomState{Name: "#one", ReadState: "ready", SendState: "preparing"})
	assertStates(guest, roomState{Name: "#one", ReadState: "ready", SendState: "login_required"})
	app.receive(t.Context(), irc.Event{Kind: "membership", Room: "#one"})
	for _, sub := range []*subscription{owner, other, guest} {
		assertOpenAndEmpty(sub)
	}

	bridge.states[0] = map[string]irc.MembershipState{"#one": irc.RoomUnavailable, "#two": irc.RoomUnavailable}
	app.receive(t.Context(), irc.Event{Kind: "status", State: "disconnected"})
	assertStatus(owner, "connected")
	assertStatus(other, "connecting")
	assertStatus(guest, "disconnected")
	assertStates(owner, roomState{Name: "#one", ReadState: "unavailable", SendState: "ready"}, roomState{Name: "#two", ReadState: "unavailable", SendState: "preparing"})
	assertStates(other, roomState{Name: "#one", ReadState: "unavailable", SendState: "preparing"}, roomState{Name: "#two", ReadState: "unavailable", SendState: "preparing"})
	assertStates(guest, roomState{Name: "#one", ReadState: "unavailable", SendState: "login_required"}, roomState{Name: "#two", ReadState: "unavailable", SendState: "login_required"})
}

func TestHistoryIdentifiesTheViewerUsedForPersonalization(t *testing.T) {
	app, owner, ownerToken := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	other, err := app.auth.Register(t.Context(), "bob", strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := app.auth.CreateSession(t.Context(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	message, err := app.q.AddMessage(t.Context(), store.AddMessageParams{Room: "#one", Nick: owner.Nick, Original: "hello", SenderUserID: owner.ID, SenderRequestID: "private_request_01"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token string
		userID      int64
		own         bool
	}{
		{name: "owner", token: ownerToken, userID: owner.ID, own: true},
		{name: "other account", token: otherToken, userID: other.ID},
		{name: "guest"},
		{name: "invalid session", token: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/messages?room=%23one&lang=original", nil)
			if tc.token != "" {
				req.AddCookie(&http.Cookie{Name: "session", Value: tc.token})
			}
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("history status = %d: %s", response.Code, response.Body.String())
			}
			var history struct {
				UserID   *int64    `json:"userId"`
				Messages []Message `json:"messages"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
				t.Fatal(err)
			}
			if history.UserID == nil || *history.UserID != tc.userID {
				t.Fatalf("history owner = %v, want explicit viewer %d", history.UserID, tc.userID)
			}
			if len(history.Messages) != 1 || history.Messages[0].ID != message.ID {
				t.Fatalf("history = %+v, want message %d", history.Messages, message.ID)
			}
			msg := history.Messages[0]
			wantRequestID := ""
			if tc.own {
				wantRequestID = "private_request_01"
			}
			if msg.Own != tc.own || msg.RequestID != wantRequestID {
				t.Errorf("viewer %d personalization = %+v", tc.userID, msg)
			}
		})
	}
}

type gatedSummaryDB struct {
	*sql.DB
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (db *gatedSummaryDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	db.once.Do(func() { close(db.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-db.release:
		return db.DB.QueryContext(ctx, query, args...)
	}
}

func TestWebSocketBootstrapIdentityAndLiveMessageOrdering(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "snapshot before queued live messages"
		if disconnect {
			name = "disconnect cancels summary and releases account"
		}
		t.Run(name, func(t *testing.T) {
			released := make(chan struct{})
			bridge := &leaseBridge{lifecycleBridge: &lifecycleBridge{state: irc.RoomReady}, acquire: func(context.Context, irc.Account) (func(), error) {
				return func() { close(released) }, nil
			}}
			app, user, token := lifecycleServer(t, bridge)
			db, _, err := store.NewSQLite(t.Context(), filepath.Join(t.TempDir(), "summaries.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			gate := &gatedSummaryDB{DB: db, entered: make(chan struct{}), release: make(chan struct{})}
			app.q = store.New(gate)
			server := httptest.NewServer(app.Handler())
			t.Cleanup(server.Close)
			conn, response, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http")+"/api/ws?room=%23one", http.Header{"Origin": {"https://chat.example"}, "Cookie": {"session=" + token}})
			if err != nil {
				t.Fatal(err)
			}
			if response.Body != nil {
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			closeConnection := sync.OnceFunc(func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			})
			t.Cleanup(func() {
				closeConnection()
				app.cancel()
				select {
				case <-released:
				case <-time.After(5 * time.Second):
					t.Error("websocket handler did not release account during cleanup")
				}
			})
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			readFrame := func(want string) frame {
				t.Helper()
				var f frame
				if err := conn.ReadJSON(&f); err != nil {
					t.Fatal(err)
				}
				if f.Type != want {
					t.Fatalf("frame type = %q, want %q", f.Type, want)
				}
				return f
			}
			identity := readFrame("identity")
			if identity.UserID == nil || *identity.UserID != user.ID {
				t.Fatalf("socket identity = %v, want %d", identity.UserID, user.ID)
			}
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("bootstrap never requested room summaries")
			}
			if !disconnect {
				app.mu.Lock()
				app.accountStates[user.ID] = network{State: "connected", Detail: "during bootstrap"}
				app.broadcastLocked("", "", user.ID, frame{Type: "status"})
				app.mu.Unlock()
				app.receive(t.Context(), irc.Event{Kind: "message", Room: "#one", Nick: "server", Text: "during bootstrap", Service: true})
				close(gate.release)
				initialStatus := readFrame("status")
				if initialStatus.State != "connecting" {
					t.Fatalf("initial status = %+v, want captured connecting state", initialStatus)
				}
				rooms := readFrame("rooms")
				if len(rooms.Rooms) != 2 || rooms.Rooms[0].Name != "#one" || rooms.Rooms[1].Name != "#two" {
					t.Fatalf("bootstrap rooms = %+v", rooms.Rooms)
				}
				latestStatus := readFrame("status")
				if latestStatus.State != "connected" || latestStatus.Detail != "during bootstrap" {
					t.Fatalf("live status lost during bootstrap: %+v", latestStatus)
				}
				live := readFrame("message")
				if live.Message == nil || live.Message.Original != "during bootstrap" {
					t.Fatalf("queued live message lost: %+v", live)
				}
			}
			closeConnection()
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("closed socket retained its account during bootstrap")
			}
		})
	}
}
