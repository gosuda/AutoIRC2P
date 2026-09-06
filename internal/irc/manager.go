// Package irc owns the embedded I2P router and isolated IRC account connections.
package irc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
	"gosuda.org/ivnp/foundation"
)

var (
	ErrNotStarted       = errors.New("IRC manager is not started")
	ErrNotConnected     = errors.New("IRC account has not joined this room")
	ErrObserverReadOnly = errors.New("observer cannot send messages")
	errInvalidConfig    = errors.New("IRC requires an I2P server with port, a config path, and configured rooms")
	errInvalidAccount   = errors.New("IRC account has invalid ID, password, or duplicate identity")
	errNickUnavailable  = errors.New("nickname is unavailable; choose another nickname or recover it through the IRC network")
	errNoReadiness      = errors.New("IVNP destination does not support readiness")
)

type Account struct {
	ID         int64
	Nick       string
	Password   string
	Email      string
	Identity   Identity
	Registered bool
}

type Event struct {
	AccountID int64
	Room      string
	Nick      string
	Text      string
	Service   bool
	Kind      string
	State     string
}

type Config struct {
	ConfigPath string
	Server     string
	Rooms      []string
	// Zero selects 20 minutes idle and 2 minutes for PONG writes.
	IdleTimeout, PongTimeout time.Duration
}

// Manager callbacks run on connection workers and must not call Close or block.
// Connect schedules a manager-lifetime connection, not a request-lifetime one.
type Manager struct {
	cfg         Config
	node        *ivnp.Node
	addressBook *client.AddressBookService
	onEvent     func(Event)
	rooms       map[string]string
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	started     bool
	closed      bool
	ready       chan struct{}
	startErr    error
	accounts    map[int64]*accountConnection
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
}

type accountConnection struct {
	account     Account
	local       *foundation.LocalDestination
	mu          sync.Mutex
	connection  *wireConnection
	joined      map[string]bool
	unavailable map[string]bool
}

type wireConnection struct {
	net.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func New(cfg Config, onEvent func(Event)) (*Manager, error) {
	host, port, err := net.SplitHostPort(cfg.Server)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || portNumber == 0 {
		return nil, errInvalidConfig
	}
	if !strings.HasSuffix(strings.ToLower(host), ".i2p") || !safeAtom(host) {
		return nil, errInvalidConfig
	}
	if cfg.ConfigPath == "" || len(cfg.Rooms) == 0 || onEvent == nil {
		return nil, errInvalidConfig
	}
	if cfg.IdleTimeout < 0 || cfg.PongTimeout < 0 {
		return nil, errInvalidConfig
	}
	rooms := make(map[string]string, len(cfg.Rooms))
	for _, room := range cfg.Rooms {
		if !validRoom(room) {
			return nil, ErrInvalidRoom
		}
		if _, exists := rooms[fold(room)]; exists {
			return nil, ErrInvalidRoom
		}
		rooms[fold(room)] = room
	}
	configuration, err := ivnp.LoadOrCreateConfig(cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load IVNP configuration: %w", err)
	}
	addressBook, err := newAddressBook(configuration)
	if err != nil {
		return nil, fmt.Errorf("open I2P addressbook: %w", err)
	}
	// Own the resolver used by account endpoints; avoid two subscription writers.
	configuration.AddressBook.Enabled = false
	// The application uses destination endpoints directly, not local proxy ports.
	configuration.SAM.Enabled = false
	configuration.HTTPProxy.Enabled = false
	configuration.SOCKS5.Enabled = false
	configuration.Control.Enabled = false
	configuration.Metrics.Enabled = false
	node, err := ivnp.New(configuration, ivnp.Options{})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open embedded IVNP router: %w", err), addressBook.Close(), addressBook.Wait())
	}
	cfg.Rooms = append([]string(nil), cfg.Rooms...)
	return &Manager{cfg: cfg, node: node, addressBook: addressBook, onEvent: onEvent, rooms: rooms, ready: make(chan struct{}), accounts: make(map[int64]*accountConnection)}, nil
}

// Start returns before reseeding, tunnel construction, or IRC registration.
func (m *Manager) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return net.ErrClosed
	}
	if m.started {
		return nil
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.started = true
	m.wg.Go(m.runRouter)
	return nil
}

func (m *Manager) runRouter() {
	err := m.node.Start(m.ctx)
	if err == nil && m.addressBook != nil {
		err = m.addressBook.Start(m.ctx)
	}
	m.mu.Lock()
	m.startErr = err
	close(m.ready)
	m.mu.Unlock()
	if err != nil {
		m.status(0, "error", "Embedded I2P router failed to start; inspect local router configuration")
		m.cancel()
	} else {
		m.status(0, "connecting", "I2P router started; building anonymous tunnels")
		<-m.ctx.Done()
	}
	closeErr := errors.Join(m.addressBook.Close(), m.node.Close())
	waitErr := errors.Join(m.addressBook.Wait(), m.node.Wait())
	m.mu.Lock()
	m.closeErr = errors.Join(m.closeErr, closeErr, waitErr)
	m.mu.Unlock()
}

// Connect is idempotent for an already scheduled account. The caller retains
// ownership of Identity.Keys; only the validated destination is retained here.
func (m *Manager) Connect(ctx context.Context, account Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validNick(account.Nick) || serviceNick(account.Nick) {
		return ErrInvalidNick
	}
	if account.ID < 0 {
		return errInvalidAccount
	}
	if account.ID != 0 && len(account.Password) < 16 {
		return errInvalidAccount
	}
	if account.Password != "" && !validPassword(account.Password) {
		return errInvalidAccount
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return net.ErrClosed
	}
	if !m.started {
		return ErrNotStarted
	}
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if _, exists := m.accounts[account.ID]; exists {
		return nil
	}
	for _, active := range m.accounts {
		if active.account.Identity.Address == account.Identity.Address || fold(active.account.Nick) == fold(account.Nick) {
			return errInvalidAccount
		}
	}
	local, err := restoreIdentity(account.Identity)
	if err != nil {
		return err
	}
	account.Identity.Keys = nil
	account.Email = ""
	state := &accountConnection{account: account, local: local, joined: make(map[string]bool), unavailable: make(map[string]bool)}
	m.accounts[account.ID] = state
	m.wg.Go(func() { m.runAccount(state) })
	return nil
}

func (m *Manager) runAccount(state *accountConnection) {
	defer state.local.ReleaseSensitive()
	defer func() {
		m.mu.Lock()
		delete(m.accounts, state.account.ID)
		m.mu.Unlock()
	}()
	select {
	case <-m.ctx.Done():
		return
	case <-m.ready:
	}
	m.mu.Lock()
	startErr := m.startErr
	m.mu.Unlock()
	if startErr != nil {
		return
	}
	var endpoint ivnp.DestinationEndpoint
	delay := 5 * time.Second
	for m.ctx.Err() == nil {
		if endpoint == nil {
			var err error
			endpoint, err = m.node.DestinationController().CreateDestination(m.ctx, ivnp.DestinationSpec{Local: state.local})
			if err != nil {
				slog.Warn("I2P destination creation failed", "error", err)
				m.status(state.account.ID, "connecting", "I2P identity endpoint unavailable; retrying")
				if !pause(m.ctx, delay) {
					return
				}
				delay = min(delay*2, 2*time.Minute)
				continue
			}
			defer func() {
				if err := endpoint.Close(); err != nil {
					m.status(state.account.ID, "error", "I2P endpoint closed with a cleanup error")
				}
			}()
		}
		m.status(state.account.ID, "connecting", "Waiting for I2P destination tunnels")
		ready, ok := endpoint.(ivnp.ReadyDestinationEndpoint)
		if !ok {
			m.status(state.account.ID, "error", errNoReadiness.Error())
			return
		}
		stage := "destination readiness"
		readyCtx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
		err := ready.WaitReady(readyCtx)
		cancel()
		if err == nil {
			stage = "IRC dial"
			m.status(state.account.ID, "connecting", "Connecting to IRC over I2P")
			dialCtx, dialCancel := context.WithTimeout(m.ctx, 2*time.Minute)
			var conn net.Conn
			conn, err = m.dialIRC(dialCtx, endpoint)
			dialCancel()
			if err == nil {
				stage = "IRC session"
				delay = 5 * time.Second
				err = m.serveConnection(state, &wireConnection{Conn: conn})
			}
		}
		if errors.Is(err, errNickUnavailable) {
			m.status(state.account.ID, "error", errNickUnavailable.Error())
			return
		}
		if m.ctx.Err() != nil {
			return
		}
		slog.Warn("IRC connection attempt failed", "account_id", state.account.ID, "stage", stage, "error", err)
		m.status(state.account.ID, "disconnected", "IRC connection failed during "+stage+"; reconnecting without replaying messages")
		if !pause(m.ctx, delay) {
			return
		}
		delay = min(delay*2, 2*time.Minute)
	}
}

// Send writes one channel frame exactly once. A write failure has uncertain
// delivery; callers must not automatically retry it.
func (m *Manager) Send(ctx context.Context, accountID int64, room, text string) error {
	if accountID == 0 {
		return ErrObserverReadOnly
	}
	canonical, ok := m.rooms[fold(room)]
	if !ok {
		return ErrInvalidRoom
	}
	line, err := channelLine(canonical, text)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	state := m.accounts[accountID]
	closed := m.closed
	lifetime := m.ctx
	m.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if lifetime != nil {
		if err := lifetime.Err(); err != nil {
			return err
		}
	}
	if state == nil {
		return ErrNotConnected
	}
	state.mu.Lock()
	conn := state.connection
	joined := state.joined[fold(canonical)]
	state.mu.Unlock()
	if conn == nil || !joined {
		return ErrNotConnected
	}
	return conn.write(ctx, line)
}

func (m *Manager) status(accountID int64, state, text string) {
	m.onEvent(Event{AccountID: accountID, State: state, Text: text, Service: true, Kind: "status"})
}

func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		started := m.started
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.Unlock()
		if started {
			m.wg.Wait()
		} else {
			m.closeErr = errors.Join(m.addressBook.Close(), m.addressBook.Wait(), m.node.Close(), m.node.Wait())
		}
	})
	return m.closeErr
}

func (c *wireConnection) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.Conn.Close() })
	return c.closeErr
}

func (c *wireConnection) write(ctx context.Context, line string) error {
	return c.writeWithTimeout(ctx, line, 20*time.Second)
}

func (c *wireConnection) writeWithTimeout(ctx context.Context, line string, timeout time.Duration) error {
	if strings.ContainsAny(line, "\r\n\x00") {
		return ErrInvalidMessage
	}
	if len(line)+2 > 512 {
		return ErrLineTooLong
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	n, err := io.WriteString(c.Conn, line+"\r\n")
	if err == nil && n != len(line)+2 {
		err = io.ErrShortWrite
	}
	if err != nil {
		return errors.Join(err, c.Close())
	}
	return nil
}

func pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
