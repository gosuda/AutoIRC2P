package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog/log"
)

type Bridge interface {
	Acquire(context.Context, irc.Account) (func(), error)
	Retain(context.Context, int64) (func(), error)
	Send(context.Context, int64, string, string) error
	RoomState(int64, string) irc.MembershipState
}
type Translator interface {
	Translate(context.Context, string, string) (string, error)
}
type Config struct {
	WebDir string
	// AllowOrigin must be concurrency-safe and trust only explicitly assigned origins.
	AllowOrigin   func(string) bool
	Rooms         []string
	SecureCookies bool
	Security      SecurityConfig
}
type Message struct {
	ID               int64  `json:"id"`
	Room             string `json:"room"`
	Nick             string `json:"nick"`
	Original         string `json:"original"`
	Translation      string `json:"translation"`
	TargetLanguage   string `json:"targetLanguage"`
	TranslationState string `json:"translationState"`
	CreatedAt        string `json:"createdAt"`
	Service          bool   `json:"service"`
	Own              bool   `json:"own"`
	RequestID        string `json:"requestId,omitempty"`
	senderUserID     int64
	senderRequestID  string
}
type frame struct {
	Type    string    `json:"type"`
	Message *Message  `json:"message,omitempty"`
	State   string    `json:"state,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Rooms   []Room    `json:"rooms,omitempty"`
	Room    *Room     `json:"room,omitempty"`
	Send    *Outgoing `json:"send,omitempty"`
	UserID  *int64    `json:"userId,omitempty"`
}
type network struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}
type subscription struct {
	room, lang   string
	userID       int64
	out          chan frame
	done         chan struct{}
	once         sync.Once
	cursorMu     sync.Mutex
	cursors      map[string]int64
	cursorBudget tokenBucket
}

func (s *subscription) close() { s.once.Do(func() { close(s.done) }) }

type translationJob struct {
	message Message
	lang    string
}
type Server struct {
	q             *store.Queries
	auth          *auth.Service
	translator    Translator
	bridge        Bridge
	cfg           Config
	mu            sync.Mutex
	subscribers   map[*subscription]struct{}
	net           network
	accountStates map[int64]network
	jobs          chan translationJob
	queued        map[string]struct{}
	events        chan irc.Event
	authMu        sync.Mutex
	authAttempts  map[string]authWindow
	serviceID     int64
	admission     socketAdmission
	handshakes    requestLimiter
	sendAccounts  requestLimiter
	sendIPs       requestLimiter
	lifetime      context.Context
	cancel        context.CancelFunc
	sendMu        sync.Mutex
	sending       sync.WaitGroup
	pendingSends  int
	stopping      bool
	outgoingMu    sync.Mutex
}

// New disables incoming and outgoing translation when tr is nil.
func New(cfg Config, q *store.Queries, a *auth.Service, tr Translator, bridge Bridge) *Server {
	cfg.Security = cfg.Security.normalized()
	lifetime, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, q: q, auth: a, translator: tr, bridge: bridge, subscribers: make(map[*subscription]struct{}), net: network{State: "connecting", Detail: "Building I2P tunnels"}, accountStates: make(map[int64]network), events: make(chan irc.Event, 1024), lifetime: lifetime, cancel: cancel}
	if tr != nil {
		s.jobs = make(chan translationJob, 1024)
		s.queued = make(map[string]struct{})
	}
	return s
}
func (s *Server) Event(ctx context.Context, event irc.Event) {
	select {
	case s.events <- event:
	case <-ctx.Done():
	case <-s.lifetime.Done():
	}
}
func (s *Server) Run(ctx context.Context) {
	var wg sync.WaitGroup
	if s.translator != nil {
		for range 4 {
			wg.Go(func() { s.translateWorker(ctx) })
		}
	}
	defer func() {
		s.sendMu.Lock()
		s.stopping = true
		s.cancel()
		s.sendMu.Unlock()
		s.sending.Wait()
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.q.RecoverSends(recoveryCtx); err != nil {
			log.Error().Err(err).Msg("recover interrupted sends")
		}
		s.mu.Lock()
		for sub := range s.subscribers {
			sub.close()
		}
		s.mu.Unlock()
		wg.Wait()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireSends(ctx)
		case event := <-s.events:
			s.receive(ctx, event)
		}
	}
}
func (s *Server) receive(ctx context.Context, event irc.Event) {
	switch event.Kind {
	case "registered":
		if event.AccountID == 0 {
			return
		}
		if err := s.q.RegisterIRC(ctx, event.AccountID); err != nil {
			log.Error().Err(err).Msg("persist IRC registration")
		}
		return
	case "status":
		status := network{State: event.State, Detail: event.Text}
		s.mu.Lock()
		if event.AccountID == 0 {
			s.net = status
		} else {
			s.accountStates[event.AccountID] = status
		}
		s.broadcastLocked("", "", event.AccountID, frame{Type: "status"})
		s.forgetStoppedAccountLocked(event.AccountID)
		s.mu.Unlock()
		s.refreshRooms(ctx, "", event.AccountID)
		return
	case "membership":
		s.refreshRooms(ctx, event.Room, event.AccountID)
		return
	}
	if event.Service {
		s.serviceID--
		msg := Message{ID: s.serviceID, Room: event.Room, Nick: event.Nick, Original: event.Text, Translation: event.Text, TranslationState: "excluded", Service: true, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		s.broadcast(event.Room, "", event.AccountID, frame{Type: "message", Message: &msg})
		return
	}
	if !s.roomAllowed(event.Room) || event.Text == "" {
		return
	}
	if event.AccountID != 0 {
		return
	}
	s.outgoingMu.Lock()
	var row store.Message
	var confirmed *store.SendRequest
	err := s.q.Transaction(ctx, func(q *store.Queries) error {
		original := event.Text
		echo, echoErr := q.PendingEcho(ctx, store.PendingEchoParams{Room: event.Room, Nick: event.Nick, WireText: event.Text, CreatedAt: time.Now().Add(-20 * time.Minute).UnixMilli()})
		if echoErr != nil && !errors.Is(echoErr, sql.ErrNoRows) {
			return echoErr
		}
		params := store.AddMessageParams{Room: event.Room, Nick: event.Nick, CreatedAt: time.Now().UnixMilli()}
		if echoErr == nil {
			original = echo.Original
			params.SenderUserID = echo.UserID
			params.SenderRequestID = echo.RequestID
		}
		params.Original = original
		var err error
		row, err = q.AddMessage(ctx, params)
		if err != nil {
			return err
		}
		if echoErr == nil {
			updated, err := q.ConsumeEcho(ctx, store.ConsumeEchoParams{MessageID: row.ID, UpdatedAt: time.Now().UnixMilli(), UserID: echo.UserID, RequestID: echo.RequestID})
			if err != nil {
				return err
			}
			confirmed = &updated
		}
		return nil
	})
	if err != nil {
		s.outgoingMu.Unlock()
		log.Error().Err(err).Msg("persist IRC message")
		return
	}
	if confirmed != nil {
		s.publishSend(*confirmed)
	}
	s.outgoingMu.Unlock()
	s.refreshRooms(ctx, row.Room, 0)
	languages := []string{"original"}
	if s.translator != nil {
		languages = []string{"en", "ko", "original"}
	}
	for _, lang := range languages {
		msg := messageFrom(row, lang)
		s.broadcast(row.Room, lang, 0, frame{Type: "message", Message: &msg})
		if msg.TranslationState == "pending" && s.hasReaders(row.Room, lang) {
			s.enqueue(msg, lang)
		}
	}
}
func messageFrom(row store.Message, lang string) Message {
	msg := Message{ID: row.ID, Room: row.Room, Nick: row.Nick, Original: row.Original, TargetLanguage: lang, TranslationState: "pending", CreatedAt: time.UnixMilli(row.CreatedAt).UTC().Format(time.RFC3339Nano), Service: row.Service != 0, senderUserID: row.SenderUserID, senderRequestID: row.SenderRequestID}
	if lang == "original" {
		msg.TargetLanguage = ""
		msg.TranslationState = "excluded"
		return msg
	}
	if msg.Service {
		msg.TranslationState = "excluded"
		msg.Translation = msg.Original
	}
	return msg
}
func (s *Server) hasReaders(room, lang string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subscribers {
		if sub.room == room && sub.lang == lang {
			return true
		}
	}
	return false
}
func jobKey(msg Message, lang string) string { return stringID(msg.ID) + ":" + lang }
func (s *Server) enqueue(msg Message, lang string) {
	key := jobKey(msg, lang)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queued[key]; ok {
		return
	}
	s.queued[key] = struct{}{}
	select {
	case s.jobs <- translationJob{message: msg, lang: lang}:
	default:
		delete(s.queued, key)
		msg.TranslationState = "failed"
		s.broadcastLocked(msg.Room, lang, 0, frame{Type: "translation", Message: &msg})
	}
}
func (s *Server) translateWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			jobCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			text, err := s.translator.Translate(jobCtx, job.message.Original, job.lang)
			cancel()
			msg := job.message
			msg.Translation = text
			msg.TranslationState = "ready"
			if err != nil {
				msg.TranslationState = "failed"
			}
			s.mu.Lock()
			delete(s.queued, jobKey(msg, job.lang))
			s.broadcastLocked(msg.Room, job.lang, 0, frame{Type: "translation", Message: &msg})
			s.mu.Unlock()
		}
	}
}
func (s *Server) networkStatusLocked(userID int64) network {
	if userID == 0 {
		return s.net
	}
	if status, ok := s.accountStates[userID]; ok {
		return status
	}
	return network{State: "connecting", Detail: "Waiting for IRC account connection"}
}

func (s *Server) forgetStoppedAccountLocked(userID int64) {
	status, ok := s.accountStates[userID]
	if !ok || status.State != "stopped" {
		return
	}
	for sub := range s.subscribers {
		if sub.userID == userID {
			return
		}
	}
	delete(s.accountStates, userID)
}

func (s *Server) broadcast(room, lang string, userID int64, f frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcastLocked(room, lang, userID, f)
}
func (s *Server) broadcastLocked(room, lang string, userID int64, f frame) {
	for sub := range s.subscribers {
		wrongRoom := room != "" && sub.room != room
		wrongLanguage := lang != "" && sub.lang != lang
		wrongAccount := userID != 0 && sub.userID != userID
		if wrongRoom || wrongLanguage || wrongAccount {
			continue
		}
		outgoing := f
		if f.Type == "status" {
			status := s.networkStatusLocked(sub.userID)
			outgoing.State, outgoing.Detail = status.State, status.Detail
		}
		if f.Message != nil {
			msg := personalize(*f.Message, sub.userID)
			if msg.Service {
				if msg.Room == "" {
					msg.Room = sub.room
				}
				msg.TargetLanguage = sub.lang
			}
			outgoing.Message = &msg
		}
		select {
		case sub.out <- outgoing:
		default:
			sub.close()
		}
	}
}
func (s *Server) roomAllowed(room string) bool {
	for _, r := range s.cfg.Rooms {
		if r == room {
			return true
		}
	}
	return false
}
func (s *Server) language(r *http.Request) string {
	if s.translator == nil || r.URL.Query().Get("lang") == "original" {
		return "original"
	}
	if r.URL.Query().Get("lang") == "en" {
		return "en"
	}
	return "ko"
}
func (s *Server) Handler() http.Handler {
	scripts, err := scriptPolicy(s.cfg.WebDir)
	if err != nil {
		log.Error().Err(err).Msg("read frontend script policy")
		scripts = "'self'"
	}
	router := httprouter.New()
	router.HandlerFunc(http.MethodGet, "/api/session", s.session)
	router.HandlerFunc(http.MethodGet, "/api/auth/salt", s.salt)
	router.HandlerFunc(http.MethodPost, "/api/auth/register", s.limitAuthentication(s.register))
	router.HandlerFunc(http.MethodPost, "/api/auth/login", s.limitAuthentication(s.login))
	router.HandlerFunc(http.MethodPost, "/api/auth/logout", s.logout)
	router.HandlerFunc(http.MethodGet, "/api/messages", s.history)
	router.HandlerFunc(http.MethodPost, "/api/messages", s.send)
	router.HandlerFunc(http.MethodGet, "/api/rooms", s.rooms)
	router.HandlerFunc(http.MethodGet, "/api/sends", s.sends)
	router.HandlerFunc(http.MethodGet, "/api/sends/:requestId", s.sendStatus)
	router.HandlerFunc(http.MethodGet, "/api/ws", s.websocket)
	router.HandlerFunc(http.MethodGet, "/api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	router.NotFound = http.HandlerFunc(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src "+scripts+"; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" && !s.sameOrigin(r) {
			writeError(w, 403, "cross-origin request rejected")
			return
		}
		router.ServeHTTP(w, r)
	})
}
func (s *Server) sameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return false
	}
	if len(origins) == 1 {
		return origins[0] != "" && s.cfg.AllowOrigin != nil && s.cfg.AllowOrigin(origins[0])
	}
	return r.Header.Get("Sec-Fetch-Site") != "cross-site"
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Debug().Err(err).Msg("HTTP response closed")
	}
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

var errInvalidBody = errors.New("invalid JSON request")

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errInvalidBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errInvalidBody
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errInvalidBody
	}
	return nil
}
