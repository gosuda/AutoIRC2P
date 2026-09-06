package server

import (
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/store"
	"github.com/gosuda/AutoIRC2P/internal/translate"
)

func stringID(id int64) string { return strconv.FormatInt(id, 10) }
func (s *Server) currentUser(r *http.Request) (store.User, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return store.User{}, auth.ErrCredentials
	}
	return s.auth.Session(r.Context(), cookie.Value)
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	var user *auth.User
	if row, err := s.currentUser(r); err == nil {
		u := auth.Public(row)
		user = &u
	}
	cursors, err := s.parseCursors(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var userID int64
	if user != nil {
		userID = user.ID
	}
	rooms, err := s.allRooms(r.Context(), userID, cursors)
	if err != nil {
		writeError(w, 500, "room state unavailable")
		return
	}
	s.mu.Lock()
	status := s.net
	if user != nil {
		if accountStatus, ok := s.accountStates[user.ID]; ok {
			status = accountStatus
		}
	}
	s.mu.Unlock()
	lang := "ko"
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "en") {
		lang = "en"
	}
	writeJSON(w, 200, map[string]any{"user": user, "rooms": rooms, "network": status, "displayLanguage": lang})
}
func (s *Server) salt(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if len(email) > 254 {
		writeError(w, 400, "invalid email")
		return
	}
	writeJSON(w, 200, map[string]any{"salt": s.auth.Salt(email), "iterations": auth.Iterations, "algorithm": "PBKDF2-SHA256"})
}

type credentials struct {
	Email        string `json:"email"`
	Nick         string `json:"nick"`
	PasswordHash string `json:"passwordHash"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	user, err := s.auth.Register(r.Context(), body.Email, body.Nick, body.PasswordHash)
	if err != nil {
		if errors.Is(err, auth.ErrInput) || errors.Is(err, auth.ErrRegistration) {
			writeError(w, 400, err.Error())
		} else {
			writeError(w, 503, "registration unavailable")
		}
		return
	}
	s.startSession(w, r, user)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	user, err := s.auth.Login(r.Context(), body.Email, body.PasswordHash)
	if err != nil {
		if errors.Is(err, auth.ErrCredentials) {
			writeError(w, 401, "invalid credentials")
		} else {
			writeError(w, 503, "login unavailable")
		}
		return
	}
	s.startSession(w, r, user)
}
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user store.User) {
	token, err := s.auth.CreateSession(r.Context(), user.ID)
	if err != nil {
		writeError(w, 500, "session unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 60 * 60})
	writeJSON(w, 200, map[string]any{"user": auth.Public(user)})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		if err = s.auth.Logout(r.Context(), cookie.Value); err != nil {
			writeError(w, 500, "logout unavailable")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	if !s.roomAllowed(room) {
		writeError(w, 400, "unknown room")
		return
	}
	before := int64(math.MaxInt64)
	if value := r.URL.Query().Get("before"); value != "" {
		var err error
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			writeError(w, 400, "invalid cursor")
			return
		}
	}
	rows, err := s.q.Messages(r.Context(), store.MessagesParams{Room: room, ID: before})
	if err != nil {
		writeError(w, 500, "history unavailable")
		return
	}
	languages, err := s.q.RoomLanguages(r.Context(), room)
	if err != nil {
		writeError(w, 500, "room state unavailable")
		return
	}
	roomLanguage := translate.RoomLanguage(languages)
	messages := make([]Message, 0, len(rows))
	lang := language(r)
	viewer, _ := s.currentUser(r)
	for i := len(rows) - 1; i >= 0; i-- {
		msg := messageFrom(rows[i], lang)
		msg.RoomLanguage = roomLanguage
		messages = append(messages, personalize(msg, viewer.ID))
	}
	writeJSON(w, 200, map[string]any{"messages": messages})
	for _, msg := range messages {
		if msg.TranslationState == "pending" {
			s.enqueue(msg, lang)
		}
	}
}

func validChat(text string) bool {
	if strings.TrimSpace(text) == "" || len(text) > 4096 || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, 404, "not found")
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	path := filepath.Join(s.cfg.WebDir, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
	if r.URL.Path == "/" {
		path = filepath.Join(s.cfg.WebDir, "index.html")
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		http.FileServer(http.Dir(s.cfg.WebDir)).ServeHTTP(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeError(w, 503, "frontend not built; run bun install and bun run build in web")
}
