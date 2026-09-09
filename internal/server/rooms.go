package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
	"github.com/gosuda/AutoIRC2P/internal/translate"
	"github.com/rs/zerolog/log"
)

type Room struct {
	Name            string `json:"name"`
	Language        string `json:"language"`
	LatestMessageID int64  `json:"latestMessageId"`
	UnreadCount     int64  `json:"unreadCount"`
	ReadState       string `json:"readState"`
	SendState       string `json:"sendState"`
}

type roomState struct {
	Name      string `json:"name"`
	ReadState string `json:"readState"`
	SendState string `json:"sendState"`
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
	if account == irc.RoomUnavailable {
		return read, "unavailable"
	}
	if account == irc.RoomReady {
		return read, "ready"
	}
	return read, "preparing"
}

func (s *Server) roomMetadata(ctx context.Context, userID int64, name string, cursors map[string]int64) (Room, error) {
	summaries, err := s.q.RoomSummaries(ctx, []string{name}, cursors, userID)
	if err != nil {
		return Room{}, err
	}
	summary := summaries[0]
	cursors[name] = summary.Cursor
	return s.roomFromSummary(userID, summary), nil
}

func (s *Server) allRooms(ctx context.Context, userID int64, cursors map[string]int64) ([]Room, error) {
	summaries, err := s.q.RoomSummaries(ctx, s.cfg.Rooms, cursors, userID)
	if err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(summaries))
	for _, summary := range summaries {
		cursors[summary.Name] = summary.Cursor
		rooms = append(rooms, s.roomFromSummary(userID, summary))
	}
	return rooms, nil
}

func (s *Server) roomFromSummary(userID int64, summary store.RoomSummary) Room {
	room := Room{Name: summary.Name, LatestMessageID: summary.LatestMessageID, UnreadCount: summary.UnreadCount}
	if s.translator != nil {
		room.Language = translate.RoomTargetLanguage(summary.Name)
	}
	room.ReadState, room.SendState = s.roomStates(userID, summary.Name)
	return room
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

func (s *Server) subscriptionsFor(userID int64) []*subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := make([]*subscription, 0, len(s.subscribers))
	for sub := range s.subscribers {
		if userID == 0 || sub.userID == userID {
			subs = append(subs, sub)
		}
	}
	return subs
}

func (sub *subscription) rememberRoomState(room Room) {
	if sub.roomStates == nil {
		sub.roomStates = make(map[string]roomState)
	}
	sub.roomStates[room.Name] = roomState{Name: room.Name, ReadState: room.ReadState, SendState: room.SendState}
}

func (s *Server) refreshRoomStates(name string, userID int64) {
	names := s.cfg.Rooms
	if name != "" {
		if !s.roomAllowed(name) {
			return
		}
		names = []string{name}
	}
	for _, sub := range s.subscriptionsFor(userID) {
		sub.cursorMu.Lock()
		var changed []roomState
		for _, roomName := range names {
			read, send := s.roomStates(sub.userID, roomName)
			state := roomState{Name: roomName, ReadState: read, SendState: send}
			if previous, exists := sub.roomStates[roomName]; exists && previous == state {
				continue
			}
			if sub.roomStates == nil {
				sub.roomStates = make(map[string]roomState)
			}
			sub.roomStates[roomName] = state
			changed = append(changed, state)
		}
		if len(changed) != 0 {
			sub.push(frame{Type: "roomStates", RoomStates: changed})
		}
		sub.cursorMu.Unlock()
	}
}

func (s *Server) refreshRoom(ctx context.Context, name string) {
	for _, sub := range s.subscriptionsFor(0) {
		sub.cursorMu.Lock()
		room, err := s.roomMetadata(ctx, sub.userID, name, sub.cursors)
		if err != nil {
			log.Error().Err(err).Msg("refresh room metadata")
			sub.close()
		} else {
			sub.rememberRoomState(room)
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
	if !s.cfg.Security.DisableRateLimits && !sub.cursorBudget.allow(time.Now(), s.cfg.Security.CursorUpdatesPerMinute, 30) {
		return errCursorRateLimited
	}
	cursors := map[string]int64{room: max(id, sub.cursors[room])}
	metadata, err := s.roomMetadata(ctx, sub.userID, room, cursors)
	if err != nil {
		return err
	}
	sub.cursors[room] = cursors[room]
	sub.rememberRoomState(metadata)
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
