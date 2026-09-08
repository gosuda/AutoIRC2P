package irc

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
)

const (
	channelJoinInterval = 100 * time.Millisecond
	keepaliveInterval   = 30 * time.Second
)

func (m *Manager) serveConnection(state *accountConnection, conn *wireConnection) (result error) {
	joinCtx, cancelJoins := context.WithCancel(state.ctx)
	var joins sync.WaitGroup
	stop := context.AfterFunc(state.ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() {
		cancelJoins()
		result = errors.Join(result, conn.Close())
		joins.Wait()
		state.mu.Lock()
		state.connection = nil
		clear(state.joined)
		clear(state.unavailable)
		state.mu.Unlock()
	}()
	state.mu.Lock()
	state.connection = conn
	state.mu.Unlock()
	sessionNick := state.account.Nick
	if state.account.ID == 0 && state.account.Password == "" {
		for {
			sessionNick = fmt.Sprintf("Irc2PGuest%05d", rand.IntN(100000))
			if sessionNick != state.guestNick {
				break
			}
		}
		state.guestNick = sessionNick
	}
	if err := conn.write(state.ctx, "NICK "+sessionNick); err != nil {
		return err
	}
	if err := conn.write(state.ctx, "USER "+sessionNick+" 0 * :HexChat"); err != nil {
		return err
	}

	reader := frameReader{reader: bufio.NewReaderSize(conn, 512)}
	service := nickService{account: state.account}
	ctcp := ctcpResponder{}
	welcome := false
	idleTimeout := cmp.Or(m.cfg.IdleTimeout, 20*time.Minute)
	welcomeDeadline := time.Now().Add(idleTimeout)
	var serviceDeadline time.Time
	pongTimeout := cmp.Or(m.cfg.PongTimeout, 2*time.Minute)
	for {
		deadline := time.Now().Add(idleTimeout)
		if !welcome {
			deadline = welcomeDeadline
		}
		if !service.done && !serviceDeadline.IsZero() && serviceDeadline.Before(deadline) {
			deadline = serviceDeadline
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return err
		}
		msg, err := reader.read()
		serviceExpired := !service.done && !serviceDeadline.IsZero() && !time.Now().Before(serviceDeadline)
		if err != nil {
			if welcome && serviceExpired && timeoutError(err) {
				service.done = true
				m.status(state.account.ID, "error", "NickServ verification timed out; no further credentials sent")
				continue
			}
			return err
		}
		if serviceExpired {
			service.done = true
			m.status(state.account.ID, "error", "NickServ verification timed out; no further credentials sent")
		}
		if msg.command == "PING" && len(msg.params) > 0 && len(msg.params) <= 2 {
			if err := conn.writeWithTimeout(state.ctx, "PONG "+msg.rawParams, pongTimeout); err != nil {
				return err
			}
			continue
		}
		if msg.command == "PONG" && serverSource(msg.prefix) {
			log.Info().Int64("account_id", state.account.ID).Msg("IRC keepalive PONG received")
			continue
		}
		if msg.command == "ERROR" {
			return m.serverRefusal(state, msg, net.ErrClosed)
		}
		if serverSource(msg.prefix) {
			switch msg.command {
			case "437":
				if len(msg.params) < 2 {
					return m.serverRefusal(state, msg, errNickUnavailable)
				}
				room, ok := m.rooms[fold(msg.params[1])]
				if !ok {
					return m.serverRefusal(state, msg, errNickUnavailable)
				}
				m.setRoomState(state, room, RoomUnavailable)
				m.status(state.account.ID, "error", "IRC channel temporarily unavailable: "+room)
				continue
			case "432", "433", "436":
				return m.serverRefusal(state, msg, errNickUnavailable)
			case "001":
				if welcome || len(msg.params) < 1 || fold(msg.params[0]) != fold(sessionNick) {
					continue
				}
				welcome = true
				service.server = msg.prefix
				var err error
				serviceDeadline, err = m.joinAndDiscoverServices(joinCtx, conn, state.account, &joins)
				if err != nil {
					return err
				}
				joins.Go(func() {
					m.keepalive(joinCtx, state.account.ID, conn, min(keepaliveInterval, max(time.Nanosecond, idleTimeout/2)), pongTimeout)
				})
			case "403", "405", "471", "473", "474", "475", "477", "489":
				if len(msg.params) > 1 {
					m.setRoomState(state, msg.params[1], RoomUnavailable)
				}
				m.status(state.account.ID, "error", "IRC server refused a configured room; check channel access requirements")
			}
		}
		// Servers may issue CTCP challenges before completing registration.
		if reply := ctcp.accept(msg, sessionNick, time.Now()); reply != "" {
			if err := conn.write(state.ctx, reply); err != nil {
				return err
			}
			continue
		}
		if !welcome {
			continue
		}
		action := service.accept(msg)
		if action.status != "" {
			status := "error"
			if action.registered || action.line != "" {
				status = "connected"
			}
			m.status(state.account.ID, status, action.status)
		}
		if action.line != "" {
			if err := conn.write(state.ctx, action.line); err != nil {
				return err
			}
		}
		if action.registered {
			state.account.Registered = true
			m.onEvent(Event{AccountID: state.account.ID, Kind: "registered", Service: true})
		}
		if len(msg.params) == 0 {
			continue
		}
		nick := sourceNick(msg.prefix)
		if fold(nick) == fold(sessionNick) {
			switch msg.command {
			case "JOIN":
				if room, ok := m.rooms[fold(msg.params[0])]; ok {
					m.setRoomState(state, room, RoomReady)
					m.status(state.account.ID, "connected", "IRC connected; joined "+room)
				}
			case "PART":
				m.setRoomState(state, msg.params[0], RoomUnavailable)
			case "NICK":
				return errNickUnavailable
			}
		}
		if msg.command == "KICK" && len(msg.params) >= 2 && fold(msg.params[1]) == fold(sessionNick) {
			m.setRoomState(state, msg.params[0], RoomUnavailable)
			m.status(state.account.ID, "error", "Removed from an IRC room; not automatically rejoining")
		}
		if msg.command != "PRIVMSG" && msg.command != "NOTICE" || len(msg.params) != 2 {
			continue
		}
		text := msg.params[1]
		if !utf8.ValidString(text) {
			continue
		}
		isService := msg.command == "NOTICE" || serviceNick(nick) || !strings.Contains(msg.prefix, "!") || strings.ContainsRune(text, '\x01')
		if isService {
			text = m.redactService(state, text)
		}
		if isService && fold(msg.params[0]) == fold(sessionNick) {
			m.onEvent(Event{AccountID: state.account.ID, Nick: nick, Text: text, Service: true, Kind: "message"})
			continue
		}
		room, ok := m.rooms[fold(msg.params[0])]
		if !ok || state.account.ID != 0 || !validNick(nick) {
			continue
		}
		m.onEvent(Event{AccountID: 0, Room: room, Nick: nick, Text: text, Service: isService, Kind: "message"})
	}
}

func (m *Manager) keepalive(ctx context.Context, accountID int64, conn *wireConnection, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.writeWithTimeout(ctx, "PING :autoirc2p", timeout); err != nil {
				err = errors.Join(err, conn.Close())
				if ctx.Err() == nil {
					event := log.Warn().Int64("account_id", accountID)
					event.Err(err).Msg("IRC keepalive write failed")
				}
				return
			}
			event := log.Info().Int64("account_id", accountID)
			event.Dur("interval_ms", interval).Msg("IRC keepalive PING sent")
		}
	}
}

func (m *Manager) joinAndDiscoverServices(ctx context.Context, conn *wireConnection, account Account, joins *sync.WaitGroup) (time.Time, error) {
	m.status(account.ID, "connected", "IRC connected; joining configured rooms")
	var deadline time.Time
	if account.Password != "" {
		deadline = time.Now().Add(time.Minute)
		if err := conn.write(ctx, "WHOIS NickServ"); err != nil {
			return time.Time{}, err
		}
	}
	joins.Go(func() {
		if err := m.joinRooms(ctx, conn); err != nil && ctx.Err() == nil {
			m.status(account.ID, "error", "IRC channel join failed; reconnecting")
			if err := conn.Close(); err != nil {
				m.status(account.ID, "error", "IRC connection cleanup failed")
			}
		}
	})
	return deadline, nil
}

func (m *Manager) joinRooms(ctx context.Context, conn *wireConnection) error {
	for index, room := range m.cfg.Rooms {
		if index > 0 && !pause(ctx, channelJoinInterval) {
			return ctx.Err()
		}
		if err := conn.write(ctx, "JOIN "+room); err != nil {
			return err
		}
	}
	return nil
}

func timeoutError(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

func (m *Manager) serverRefusal(state *accountConnection, msg frame, cause error) error {
	reason := ""
	if len(msg.params) > 0 {
		reason = m.redactService(state, msg.params[len(msg.params)-1])
	}
	return fmt.Errorf("%w (IRC %s: %s)", cause, msg.command, reason)
}

func (m *Manager) redactService(state *accountConnection, text string) string {
	if state.account.Password != "" {
		text = strings.ReplaceAll(text, state.account.Password, "[redacted]")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, other := range m.accounts {
		if id != state.account.ID && other.account.Password != "" {
			text = strings.ReplaceAll(text, other.account.Password, "[redacted]")
		}
	}
	return text
}

func serviceNick(nick string) bool {
	switch fold(nick) {
	case "nickserv", "chanserv", "memoserv", "operserv", "hostserv", "botserv", "helpserv", "global":
		return true
	}
	return false
}
