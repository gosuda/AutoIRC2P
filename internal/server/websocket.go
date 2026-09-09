package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/rs/zerolog/log"
)

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.cfg.Security.DisableRateLimits && !s.handshakes.allow(ip, time.Now(), s.cfg.Security.WSHandshakesPerMinute, 10) {
		rateLimited(w)
		return
	}
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
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	user, authErr := s.currentUser(r)
	var userID int64
	if authErr == nil {
		userID = user.ID
	}
	releaseSlot, admitted := s.admission.acquire(ip, userID, s.cfg.Security)
	if !admitted {
		rateLimited(w)
		return
	}
	defer releaseSlot()
	if userID > 0 {
		account, err := s.auth.Account(ctx, user)
		if err != nil {
			writeError(w, 500, "identity unavailable")
			return
		}
		releaseAccount, err := s.bridge.Acquire(ctx, account)
		account.ReleaseSensitive()
		if err != nil {
			if errors.Is(err, irc.ErrAccountCapacity) {
				rateLimited(w)
			} else {
				writeError(w, 503, "account connection unavailable")
			}
			return
		}
		defer releaseAccount()
	}
	if ctx.Err() != nil {
		writeError(w, 503, "connection interrupted")
		return
	}
	sub := &subscription{userID: userID, room: room, lang: s.language(r), out: make(chan frame, 256), done: make(chan struct{}), cursors: cursors}
	upgrader := websocket.Upgrader{CheckOrigin: s.sameOrigin, HandshakeTimeout: 10 * time.Second}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	sub.cursorMu.Lock()
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	status := s.networkStatusLocked(sub.userID)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subscribers, sub)
		s.forgetStoppedAccountLocked(sub.userID)
		s.mu.Unlock()
		sub.close()
	}()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer sub.close()
		defer cancel()
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
			if request.Type != "read" {
				return
			}
			if err := s.markRead(ctx, sub, request.Room, request.MessageID); err != nil {
				if errors.Is(err, errCursorRateLimited) {
					if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "cursor rate exceeded"), time.Now().Add(10*time.Second)); err != nil {
						log.Debug().Err(err).Msg("websocket policy close")
					}
				}
				return
			}
		}
	}()
	defer func() {
		cancel()
		sub.close()
		if err := conn.Close(); err != nil {
			log.Debug().Err(err).Msg("websocket closed")
		}
		<-readDone
	}()
	writeFrame := func(f frame) error {
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		return conn.WriteJSON(f)
	}
	// Capture live events before the snapshot, but keep all bootstrap frames ahead of them.
	if err := func() error {
		defer sub.cursorMu.Unlock()
		if err := writeFrame(frame{Type: "identity", UserID: &sub.userID}); err != nil {
			return err
		}
		if err := writeFrame(frame{Type: "status", State: status.State, Detail: status.Detail}); err != nil {
			return err
		}
		rooms, err := s.allRooms(ctx, sub.userID, sub.cursors)
		if err != nil {
			return err
		}
		for _, room := range rooms {
			sub.rememberRoomState(room)
		}
		return writeFrame(frame{Type: "rooms", Rooms: rooms})
	}(); err != nil {
		return
	}
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			return
		case f := <-sub.out:
			if err := writeFrame(f); err != nil {
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
