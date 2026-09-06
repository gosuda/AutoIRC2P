package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
	"github.com/gosuda/AutoIRC2P/internal/translate"
)

type Room struct {
	Name            string `json:"name"`
	Language        string `json:"language"`
	LatestMessageID int64  `json:"latestMessageId"`
	UnreadCount     int64  `json:"unreadCount"`
	ReadState       string `json:"readState"`
	SendState       string `json:"sendState"`
}

var errInvalidCursors = errors.New("invalid read cursors")
var errCursorRateLimited = errors.New("cursor rate exceeded")

func (s *Server) parseCursors(r *http.Request) (map[string]int64, error) {
	cursors := make(map[string]int64)
	raw := r.URL.Query().Get("cursors")
	if raw == "" {
		return cursors, nil
	}
	values := make(map[string]*int64)
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &values) != nil || values == nil {
		return nil, errInvalidCursors
	}
	for room, id := range values {
		if !s.roomAllowed(room) || id == nil || *id < 0 {
			return nil, errInvalidCursors
		}
		cursors[room] = *id
	}
	return cursors, nil
}

func (s *Server) roomStates(userID int64, room string) (string, string) {
	observer := s.bridge.RoomState(0, room)
	read := "loading"
	if observer == irc.RoomReady {
		read = "ready"
	}
	if observer == irc.RoomUnavailable {
		read = "unavailable"
	}
	if userID == 0 {
		return read, "login_required"
	}
	account := s.bridge.RoomState(userID, room)
	if observer == irc.RoomUnavailable || account == irc.RoomUnavailable {
		return read, "unavailable"
	}
	if observer == irc.RoomReady && account == irc.RoomReady {
		return read, "ready"
	}
	return read, "preparing"
}

func (s *Server) roomMetadata(ctx context.Context, userID int64, name string, cursors map[string]int64) (Room, error) {
	room := Room{Name: name}
	room.ReadState, room.SendState = s.roomStates(userID, name)
	err := s.q.Transaction(ctx, func(q *store.Queries) error {
		latest, err := q.LatestRoomMessage(ctx, name)
		if err != nil {
			return err
		}
		room.LatestMessageID = latest
		langs, err := q.RoomLanguages(ctx, name)
		if err != nil {
			return err
		}
		room.Language = translate.RoomLanguage(langs)
		cursor, exists := cursors[name]
		if !exists {
			cursors[name] = latest
			return nil
		}
		cursor, err = q.BoundReadCursor(ctx, store.BoundReadCursorParams{Room: name, ID: cursor})
		if err != nil {
			return err
		}
		cursors[name] = cursor
		room.UnreadCount, err = q.UnreadMessages(ctx, store.UnreadMessagesParams{Room: name, ID: cursor, SenderUserID: userID})
		return err
	})
	return room, err
}

func (s *Server) allRooms(ctx context.Context, userID int64, cursors map[string]int64) ([]Room, error) {
	rooms := make([]Room, 0, len(s.cfg.Rooms))
	for _, name := range s.cfg.Rooms {
		room, err := s.roomMetadata(ctx, userID, name, cursors)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, nil
}

func (s *Server) rooms(w http.ResponseWriter, r *http.Request) {
	cursors, err := s.parseCursors(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	user, _ := s.currentUser(r)
	rooms, err := s.allRooms(r.Context(), user.ID, cursors)
	if err != nil {
		writeError(w, 500, "room state unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"rooms": rooms})
}

func (sub *subscription) push(f frame) {
	select {
	case <-sub.done:
		return
	default:
	}
	select {
	case sub.out <- f:
	default:
		sub.close()
	}
}

func (s *Server) refreshRooms(ctx context.Context, name string, userID int64) {
	s.mu.Lock()
	subs := make([]*subscription, 0, len(s.subscribers))
	for sub := range s.subscribers {
		if userID == 0 || sub.userID == userID {
			subs = append(subs, sub)
		}
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.cursorMu.Lock()
		names := s.cfg.Rooms
		if name != "" {
			names = []string{name}
		}
		for _, roomName := range names {
			room, err := s.roomMetadata(ctx, sub.userID, roomName, sub.cursors)
			if err != nil {
				slog.Error("refresh room metadata", "error", err)
				sub.close()
				break
			}
			sub.push(frame{Type: "room", Room: &room})
		}
		sub.cursorMu.Unlock()
	}
}

func (s *Server) markRead(ctx context.Context, sub *subscription, room string, id int64) error {
	if !s.roomAllowed(room) || id < 0 {
		return errInvalidCursors
	}
	sub.cursorMu.Lock()
	defer sub.cursorMu.Unlock()
	if !sub.cursorBudget.allow(time.Now(), s.cfg.Security.CursorUpdatesPerMinute, 30) {
		return errCursorRateLimited
	}
	bounded, err := s.q.BoundReadCursor(ctx, store.BoundReadCursorParams{Room: room, ID: id})
	if err != nil {
		return err
	}
	if bounded > sub.cursors[room] {
		sub.cursors[room] = bounded
	}
	metadata, err := s.roomMetadata(ctx, sub.userID, room, sub.cursors)
	if err != nil {
		return err
	}
	sub.push(frame{Type: "room", Room: &metadata})
	return nil
}

func personalize(msg Message, userID int64) Message {
	msg.Own = userID != 0 && msg.senderUserID == userID
	msg.RequestID = ""
	if msg.Own {
		msg.RequestID = msg.senderRequestID
	}
	return msg
}
