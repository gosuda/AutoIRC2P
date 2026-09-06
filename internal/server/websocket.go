package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	if !s.roomAllowed(room) {
		writeError(w, 400, "unknown room")
		return
	}
	if !s.sameOrigin(r) {
		writeError(w, 403, "cross-origin connection rejected")
		return
	}
	cursors, err := s.parseCursors(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	sub := &subscription{room: room, lang: language(r), out: make(chan frame, 256), done: make(chan struct{}), cursors: cursors}
	if user, err := s.currentUser(r); err == nil {
		sub.userID = user.ID
		account, err := s.auth.Account(user)
		if err != nil {
			writeError(w, 500, "identity unavailable")
			return
		}
		if err = s.bridge.Connect(r.Context(), account); err != nil {
			slog.Debug("account connection unavailable", "error", err)
		}
	}
	upgrader := websocket.Upgrader{CheckOrigin: s.sameOrigin, HandshakeTimeout: 10 * time.Second}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	sub.push(frame{Type: "identity", UserID: &sub.userID})
	sub.cursorMu.Lock()
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	status := s.net
	if accountStatus, ok := s.accountStates[sub.userID]; ok {
		status = accountStatus
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, sub); s.mu.Unlock(); sub.close() }()
	sub.push(frame{Type: "status", State: status.State, Detail: status.Detail})
	rooms, roomErr := s.allRooms(r.Context(), sub.userID, sub.cursors)
	if roomErr == nil {
		sub.push(frame{Type: "rooms", Rooms: rooms})
	}
	sub.cursorMu.Unlock()
	if roomErr != nil {
		if err := conn.Close(); err != nil {
			slog.Debug("websocket closed", "error", err)
		}
		return
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer sub.close()
		conn.SetReadLimit(4096)
		if err := conn.SetReadDeadline(time.Now().Add(75 * time.Second)); err != nil {
			return
		}
		conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(75 * time.Second)) })
		for {
			var request struct {
				Type      string `json:"type"`
				Room      string `json:"room"`
				MessageID int64  `json:"messageId"`
			}
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if request.Type != "read" || s.markRead(r.Context(), sub, request.Room, request.MessageID) != nil {
				return
			}
		}
	}()
	defer func() {
		sub.close()
		if err := conn.Close(); err != nil {
			slog.Debug("websocket closed", "error", err)
		}
		<-readDone
	}()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.lifetime.Done():
			return
		case <-sub.done:
			return
		case f := <-sub.out:
			if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if err := conn.WriteJSON(f); err != nil {
				return
			}
		case <-ticker.C:
			// Revalidate long-lived authenticated sockets after logout or session expiry.
			if sub.userID != 0 {
				if _, err := s.currentUser(r); err != nil {
					return
				}
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}
