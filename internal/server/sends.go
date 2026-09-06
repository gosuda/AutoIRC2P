package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
	"github.com/gosuda/AutoIRC2P/internal/translate"
	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
)

type Outgoing struct {
	RequestID string `json:"requestId"`
	Room      string `json:"room"`
	Original  string `json:"original"`
	State     string `json:"state"`
	MessageID int64  `json:"messageId"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
	ErrorCode string `json:"errorCode"`
}

func outgoingFrom(row store.SendRequest) Outgoing {
	return Outgoing{RequestID: row.RequestID, Room: row.Room, Original: row.Original, State: row.State, MessageID: row.MessageID, CreatedAt: time.UnixMilli(row.CreatedAt).UTC().Format(time.RFC3339Nano), ExpiresAt: time.UnixMilli(row.ExpiresAt).UTC().Format(time.RFC3339Nano), ErrorCode: row.ErrorCode}
}

func (s *Server) publishSend(row store.SendRequest) {
	outgoing := outgoingFrom(row)
	s.broadcast("", "", row.UserID, frame{Type: "send", Send: &outgoing})
}

func (s *Server) expireSends(ctx context.Context) {
	s.outgoingMu.Lock()
	defer s.outgoingMu.Unlock()
	now := time.Now().UnixMilli()
	rows, err := s.q.ExpireEchoes(ctx, store.ExpireEchoesParams{UpdatedAt: now, ExpiresAt: now})
	if err != nil {
		log.Error().Err(err).Msg("expire outgoing echoes")
		return
	}
	for _, row := range rows {
		s.publishSend(row)
	}
}

func (s *Server) sends(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentUser(r)
	if err != nil {
		sendError(w, 401, "login_required", nil)
		return
	}
	room := r.URL.Query().Get("room")
	if !s.roomAllowed(room) {
		sendError(w, 400, "invalid_message", nil)
		return
	}
	rows, err := s.q.ListSends(r.Context(), store.ListSendsParams{UserID: user.ID, Room: room, ExpiresAt: time.Now().UnixMilli()})
	if err != nil {
		writeError(w, 500, "send history unavailable")
		return
	}
	sends := make([]Outgoing, 0, len(rows))
	for _, row := range rows {
		sends = append(sends, outgoingFrom(row))
	}
	writeJSON(w, 200, map[string]any{"sends": sends})
}

func (s *Server) sendStatus(w http.ResponseWriter, r *http.Request) {
	user, err := s.currentUser(r)
	if err != nil {
		sendError(w, 401, "login_required", nil)
		return
	}
	id := httprouter.ParamsFromContext(r.Context()).ByName("requestId")
	row, err := s.q.GetSend(r.Context(), store.GetSendParams{UserID: user.ID, RequestID: id})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "send not found")
		return
	}
	if err != nil {
		writeError(w, 500, "send status unavailable")
		return
	}
	if row.PayloadPurged != 0 {
		sendError(w, 410, "request_expired", nil)
		return
	}
	writeJSON(w, 200, map[string]any{"send": outgoingFrom(row)})
}

func sendError(w http.ResponseWriter, status int, code string, row *store.SendRequest) {
	body := map[string]any{"error": code, "code": code}
	if row != nil {
		body["send"] = outgoingFrom(*row)
	}
	writeJSON(w, status, body)
}

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,80}$`)

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Security.DisableRateLimits && !s.sendIPs.allow(s.clientIP(r), time.Now(), s.cfg.Security.SendRequestsPerIPMinute, 10) {
		rateLimited(w)
		return
	}
	user, err := s.currentUser(r)
	if err != nil {
		sendError(w, 401, "login_required", nil)
		return
	}
	if !s.cfg.Security.DisableRateLimits && !s.sendAccounts.allow(stringID(user.ID), time.Now(), s.cfg.Security.SendRequestsPerMinute, 5) {
		rateLimited(w)
		return
	}
	s.sendMu.Lock()
	if s.stopping {
		s.sendMu.Unlock()
		sendError(w, 503, "not_ready", nil)
		return
	}
	if s.pendingSends >= s.cfg.Security.MaxPendingSends {
		s.sendMu.Unlock()
		rateLimited(w)
		return
	}
	s.pendingSends++
	s.sending.Add(1)
	s.sendMu.Unlock()
	defer func() {
		s.sendMu.Lock()
		s.pendingSends--
		s.sendMu.Unlock()
		s.sending.Done()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	r = r.WithContext(ctx)
	var body struct {
		Room      string `json:"room"`
		Text      string `json:"text"`
		Original  bool   `json:"original"`
		RequestID string `json:"requestId"`
	}
	if decodeJSON(w, r, &body) != nil || !s.roomAllowed(body.Room) || !requestIDPattern.MatchString(body.RequestID) || !validChat(body.Text) {
		sendError(w, 400, "invalid_message", nil)
		return
	}
	key := store.GetSendParams{UserID: user.ID, RequestID: body.RequestID}
	originalMode := int64(0)
	if body.Original {
		originalMode = 1
	}
	previous, err := s.q.GetSend(r.Context(), key)
	if err == nil {
		if previous.PayloadPurged != 0 {
			sendError(w, 409, "request_expired", nil)
			return
		}
		if previous.Room != body.Room || previous.Original != body.Text || previous.OriginalMode != originalMode {
			sendError(w, 409, "request_conflict", nil)
			return
		}
		writeJSON(w, 200, map[string]any{"send": outgoingFrom(previous)})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		sendError(w, 500, "send_unavailable", nil)
		return
	}
	_, ready := s.roomStates(user.ID, body.Room)
	if ready != "ready" {
		sendError(w, 409, "not_ready", nil)
		return
	}
	releaseAccount, err := s.bridge.Retain(ctx, user.ID)
	if err != nil {
		sendError(w, 409, "not_ready", nil)
		return
	}
	defer releaseAccount()
	target := ""
	if !body.Original {
		langs, err := s.q.RoomLanguages(ctx, body.Room)
		if err != nil {
			sendError(w, 500, "send_unavailable", nil)
			return
		}
		target = translate.RoomLanguage(langs)
		if translate.Detect(body.Text) == target {
			target = ""
		}
	}
	state := "sending"
	if target != "" {
		state = "translating"
	}
	now := time.Now()
	claimed, err := s.q.ClaimSend(ctx, store.ClaimSendParams{UserID: user.ID, RequestID: body.RequestID, Room: body.Room, Nick: user.Nick, Original: body.Text, OriginalMode: originalMode, State: state, CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(), ExpiresAt: now.Add(2 * time.Minute).UnixMilli()})
	if err != nil {
		sendError(w, 500, "send_unavailable", nil)
		return
	}
	loadCtx, loadCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	row, err := s.q.GetSend(loadCtx, key)
	loadCancel()
	if err != nil {
		sendError(w, 500, "send_unavailable", nil)
		return
	}
	if claimed == 0 {
		if row.PayloadPurged != 0 {
			sendError(w, 409, "request_expired", nil)
			return
		}
		if row.Room != body.Room || row.Original != body.Text || row.OriginalMode != originalMode {
			sendError(w, 409, "request_conflict", nil)
			return
		}
		writeJSON(w, 200, map[string]any{"send": outgoingFrom(row)})
		return
	}
	s.publishSend(row)
	initialExpiry := row.ExpiresAt
	finish := func(state, code string, status int) {
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer persistCancel()
		s.outgoingMu.Lock()
		expiry := initialExpiry
		if state == "awaiting_echo" {
			expiry = time.Now().Add(2 * time.Minute).UnixMilli()
		}
		updated, finishErr := s.q.FinishSend(persistCtx, store.FinishSendParams{State: state, ErrorCode: code, UpdatedAt: time.Now().UnixMilli(), ExpiresAt: expiry, UserID: user.ID, RequestID: body.RequestID})
		if errors.Is(finishErr, sql.ErrNoRows) {
			updated, finishErr = s.q.GetSend(persistCtx, key)
		}
		if finishErr == nil {
			s.publishSend(updated)
		}
		s.outgoingMu.Unlock()
		if finishErr != nil {
			log.Error().Err(finishErr).Msg("persist outgoing state")
			sendError(w, 500, "send_unavailable", nil)
			return
		}
		if updated.State == "confirmed" || status == 200 {
			writeJSON(w, 200, map[string]any{"send": outgoingFrom(updated)})
			return
		}
		sendError(w, status, code, &updated)
	}
	_, ready = s.roomStates(user.ID, body.Room)
	if ready != "ready" {
		finish("failed", "not_ready", 409)
		return
	}
	if ctx.Err() != nil {
		finish("failed", "interrupted", 503)
		return
	}
	text := body.Text
	if target != "" {
		translateCtx, translateCancel := context.WithTimeout(ctx, 90*time.Second)
		text, err = s.translator.Translate(translateCtx, text, target)
		translateCancel()
		if err != nil {
			code := "translation_failed"
			if ctx.Err() != nil {
				code = "interrupted"
			}
			finish("failed", code, 503)
			return
		}
	}
	if !validChat(text) || len("PRIVMSG ")+len(body.Room)+len(" :")+len(text)+2 > 512 {
		finish("failed", "invalid_message", 422)
		return
	}
	_, ready = s.roomStates(user.ID, body.Room)
	if ready != "ready" {
		finish("failed", "not_ready", 409)
		return
	}
	if ctx.Err() != nil {
		finish("failed", "interrupted", 503)
		return
	}
	s.outgoingMu.Lock()
	row, err = s.q.PrepareSend(ctx, store.PrepareSendParams{WireText: text, UpdatedAt: time.Now().UnixMilli(), UserID: user.ID, RequestID: body.RequestID})
	if err == nil {
		s.publishSend(row)
	}
	s.outgoingMu.Unlock()
	if err != nil {
		finish("failed", "interrupted", 503)
		return
	}
	_, ready = s.roomStates(user.ID, body.Room)
	if ready != "ready" {
		finish("failed", "not_ready", 409)
		return
	}
	if ctx.Err() != nil || time.Now().UnixMilli() >= initialExpiry {
		finish("failed", "interrupted", 503)
		return
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 20*time.Second)
	err = s.bridge.Send(sendCtx, user.ID, body.Room, text)
	sendCancel()
	if err != nil {
		switch {
		case errors.Is(err, irc.ErrInvalidMessage), errors.Is(err, irc.ErrLineTooLong), errors.Is(err, irc.ErrInvalidRoom):
			finish("failed", "invalid_message", 422)
		case errors.Is(err, irc.ErrNotConnected), errors.Is(err, irc.ErrNotStarted), errors.Is(err, irc.ErrObserverReadOnly):
			finish("failed", "not_ready", 409)
		default:
			finish("unconfirmed", "send_unconfirmed", 503)
		}
		return
	}
	finish("awaiting_echo", "", 200)
}
