package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

func deleteOutgoing(app *Server, token, id, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodDelete, "https://chat.example/api/sends/"+id, nil)
	r.Header.Set("Origin", origin)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
	}
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	return w
}

func TestDeletedFailedSendKeepsClaimAndFreshRetryRemainsActionable(t *testing.T) {
	writes := 0
	app, user, token := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error {
		writes++
		return irc.ErrNotConnected
	}})
	app.cfg.Security.DisableRateLimits = true
	response, send := postOutgoing(t, app, token, "keep this text", "failed_to_delete_01", true)
	if response.Code != http.StatusConflict || send.State != "failed" || !send.OriginalMode {
		t.Fatalf("failed send: %d %+v", response.Code, send)
	}
	for range 2 {
		deleted := deleteOutgoing(app, token, send.RequestID, "https://chat.example")
		if deleted.Code != http.StatusOK {
			t.Fatalf("delete failed send: %d %s", deleted.Code, deleted.Body.String())
		}
	}
	rows, err := app.q.ListSends(t.Context(), store.ListSendsParams{UserID: user.ID, Room: "#one", ExpiresAt: time.Now().UnixMilli()})
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted send visible after reload: %+v %v", rows, err)
	}
	response, duplicate := postOutgoing(t, app, token, "keep this text", send.RequestID, true)
	if response.Code != http.StatusOK || !duplicate.Dismissed || writes != 1 {
		t.Fatalf("deleted identity replayed: %d %+v writes=%d", response.Code, duplicate, writes)
	}
	response, retry := postOutgoing(t, app, token, "keep this text", "fresh_retry_send_01", true)
	if response.Code != http.StatusConflict || retry.State != "failed" || retry.Dismissed || !retry.OriginalMode || writes != 2 {
		t.Fatalf("fresh retry failure: %d %+v writes=%d", response.Code, retry, writes)
	}
	rows, err = app.q.ListSends(t.Context(), store.ListSendsParams{UserID: user.ID, Room: "#one", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	if err != nil || len(rows) != 1 || rows[0].RequestID != retry.RequestID {
		t.Fatalf("retry failure no longer actionable: %+v %v", rows, err)
	}
}

func TestSendDeletionRequiresOwnerSameOriginAndTerminalState(t *testing.T) {
	app, user, token := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady})
	other, err := app.auth.Register(t.Context(), "bob", strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := app.auth.CreateSession(t.Context(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state, token, origin string
		want                       int
	}{
		{name: "anonymous", state: "failed", origin: "https://chat.example", want: http.StatusUnauthorized},
		{name: "other_account", state: "failed", token: otherToken, origin: "https://chat.example", want: http.StatusNotFound},
		{name: "cross_origin", state: "failed", token: token, origin: "https://evil.example", want: http.StatusForbidden},
		{name: "translating", state: "translating", token: token, origin: "https://chat.example", want: http.StatusConflict},
		{name: "sending", state: "sending", token: token, origin: "https://chat.example", want: http.StatusConflict},
		{name: "awaiting_echo", state: "awaiting_echo", token: token, origin: "https://chat.example", want: http.StatusConflict},
		{name: "confirmed", state: "confirmed", token: token, origin: "https://chat.example", want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "guarded_send_" + tc.name
			if _, err := app.q.ClaimSend(t.Context(), store.ClaimSendParams{UserID: user.ID, RequestID: id, Room: "#one", State: tc.state}); err != nil {
				t.Fatal(err)
			}
			response := deleteOutgoing(app, tc.token, id, tc.origin)
			if response.Code != tc.want {
				t.Fatalf("delete status=%d want=%d: %s", response.Code, tc.want, response.Body.String())
			}
			row, err := app.q.GetSend(t.Context(), store.GetSendParams{UserID: user.ID, RequestID: id})
			if err != nil || row.Dismissed != 0 || row.State != tc.state {
				t.Fatalf("protected send mutated: %+v %v", row, err)
			}
		})
	}
}

var errUnknownWriteResult = errors.New("write result unknown")

func TestDeletedUnconfirmedSendStillAssociatesLateDeliveredHistory(t *testing.T) {
	app, user, token := lifecycleServer(t, &lifecycleBridge{state: irc.RoomReady, send: func(context.Context, int64, string, string) error {
		return errUnknownWriteResult
	}})
	response, send := postOutgoing(t, app, token, "late delivery", "late_deleted_send_01", false)
	if response.Code != http.StatusServiceUnavailable || send.State != "unconfirmed" || send.OriginalMode {
		t.Fatalf("uncertain send: %d %+v", response.Code, send)
	}
	response = deleteOutgoing(app, token, send.RequestID, "https://chat.example")
	if response.Code != http.StatusOK {
		t.Fatalf("delete uncertain send: %d %s", response.Code, response.Body.String())
	}
	row, err := app.q.GetSend(t.Context(), store.GetSendParams{UserID: user.ID, RequestID: send.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	app.receive(t.Context(), irc.Event{Kind: "message", Room: row.Room, Nick: user.Nick, Text: row.WireText})
	row, err = app.q.GetSend(t.Context(), store.GetSendParams{UserID: user.ID, RequestID: send.RequestID})
	if err != nil || row.State != "confirmed" || row.MessageID == 0 {
		t.Fatalf("deleted send lost late echo association: %+v %v", row, err)
	}
	messages, err := app.q.Messages(t.Context(), store.MessagesParams{Room: "#one", ID: row.MessageID + 1})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != user.ID || messages[0].SenderRequestID != send.RequestID {
		t.Fatalf("delivered history lost ownership: %+v %v", messages, err)
	}
	response = deleteOutgoing(app, token, send.RequestID, "https://chat.example")
	if response.Code != http.StatusConflict {
		t.Fatalf("delivered send was deletable: %d", response.Code)
	}
}
