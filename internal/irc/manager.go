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
	ErrAccountCapacity  = errors.New("IRC account capacity reached")
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
	// Zero selects 16 registered accounts and a 2-minute last-release grace.
	MaxAccounts      int
	AccountIdleGrace time.Duration
}

// Manager callbacks run on connection workers and must not call Close or block.
type Manager struct {
	cfg               Config
	node              *ivnp.Node
	addressBook       *client.AddressBookService
	createDestination func(context.Context, ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error)
	onEvent           func(Event)
	rooms             map[string]string
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	started           bool
	closed            bool
	ready             chan struct{}
	startErr          error
	accounts          map[int64]*accountConnection
	wg                sync.WaitGroup
	accountWG         sync.WaitGroup
	closeOnce         sync.Once
	closeErr          error
}

type accountConnection struct {
	account Account
	local   *foundation.LocalDestination
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	// Lease fields are protected by Manager.mu, not the connection mutex.
	leases      int
	closing     bool
	idleTimer   *time.Timer
	idleEpoch   uint64
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
	if cfg.IdleTimeout < 0 || cfg.PongTimeout < 0 || cfg.MaxAccounts < 0 || cfg.AccountIdleGrace < 0 {
		return nil, errInvalidConfig
	}
	if cfg.MaxAccounts == 0 {
		cfg.MaxAccounts = 16
	}
	if cfg.AccountIdleGrace == 0 {
		cfg.AccountIdleGrace = 2 * time.Minute
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
	// IVNP also owns its default destination; the observer has a separate one.
	if cfg.MaxAccounts > configuration.State.MaxDestinations-2 {
		return nil, fmt.Errorf("IRC account limit exceeds IVNP destination capacity: %w", errInvalidConfig)
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
	return &Manager{cfg: cfg, node: node, addressBook: addressBook, createDestination: node.DestinationController().CreateDestination, onEvent: onEvent, rooms: rooms, ready: make(chan struct{}), accounts: make(map[int64]*accountConnection)}, nil
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
	m.mu.Lock()
	for _, state := range m.accounts {
		m.stopAccountLocked(state)
	}
	m.mu.Unlock()
	m.accountWG.Wait()
	closeErr := errors.Join(m.addressBook.Close(), m.node.Close())
	waitErr := errors.Join(m.addressBook.Wait(), m.node.Wait())
	m.mu.Lock()
	m.closeErr = errors.Join(m.closeErr, closeErr, waitErr)
	m.mu.Unlock()
}

// ConnectObserver pins account zero until manager shutdown. Identity.Keys stays
// caller-owned; the manager restores and owns a separate private destination.
func (m *Manager) ConnectObserver(ctx context.Context, account Account) error {
	if account.ID != 0 {
		return errInvalidAccount
	}
	_, err := m.acquire(ctx, account)
	return err
}

// Acquire holds a registered account until the returned idempotent release runs.
// The caller must release even after ctx is canceled; ctx only bounds admission.
func (m *Manager) Acquire(ctx context.Context, account Account) (func(), error) {
	if account.ID <= 0 {
		return nil, errInvalidAccount
	}
	return m.acquire(ctx, account)
}

func (m *Manager) acquire(ctx context.Context, account Account) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validNick(account.Nick) || serviceNick(account.Nick) {
		return nil, ErrInvalidNick
	}
	shortPassword := account.ID != 0 && len(account.Password) < 16
	invalidPassword := account.Password != "" && !validPassword(account.Password)
	if shortPassword || invalidPassword {
		return nil, errInvalidAccount
	}
	for {
		m.mu.Lock()
		if err := m.admissionErrorLocked(ctx); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if state := m.accounts[account.ID]; state != nil {
			if state.closing || state.ctx.Err() != nil {
				done := state.done
				m.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-m.ctx.Done():
					return nil, m.ctx.Err()
				case <-done:
					continue
				}
			}
			if state.account.Identity.Address != account.Identity.Address || state.account.Nick != account.Nick {
				m.mu.Unlock()
				return nil, errInvalidAccount
			}
			release := m.leaseLocked(state)
			m.mu.Unlock()
			return release, nil
		}
		registered := len(m.accounts)
		if m.accounts[0] != nil {
			registered--
		}
		if account.ID != 0 && registered >= m.cfg.MaxAccounts {
			m.mu.Unlock()
			return nil, ErrAccountCapacity
		}
		for _, active := range m.accounts {
			if active.account.Identity.Address == account.Identity.Address || fold(active.account.Nick) == fold(account.Nick) {
				m.mu.Unlock()
				return nil, errInvalidAccount
			}
		}
		local, err := restoreIdentity(account.Identity)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if err := m.admissionErrorLocked(ctx); err != nil {
			local.ReleaseSensitive()
			m.mu.Unlock()
			return nil, err
		}
		account.Identity.Keys = nil
		account.Email = ""
		lifetime, cancel := context.WithCancel(m.ctx)
		state := &accountConnection{account: account, local: local, ctx: lifetime, cancel: cancel, done: make(chan struct{}), joined: make(map[string]bool), unavailable: make(map[string]bool)}
		m.accounts[account.ID] = state
		release := m.leaseLocked(state)
		m.accountWG.Go(func() { m.runAccount(state) })
		m.mu.Unlock()
		return release, nil
	}
}

func (m *Manager) admissionErrorLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return net.ErrClosed
	}
	if !m.started {
		return ErrNotStarted
	}
	return m.ctx.Err()
}

// Retain holds an existing registered account without creating a destination.
func (m *Manager) Retain(ctx context.Context, accountID int64) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.admissionErrorLocked(ctx); err != nil {
		return nil, err
	}
	if accountID <= 0 {
		return nil, ErrObserverReadOnly
	}
	state := m.accounts[accountID]
	if state == nil || state.closing || state.ctx.Err() != nil {
		return nil, ErrNotConnected
	}
	return m.leaseLocked(state), nil
}

func (m *Manager) leaseLocked(state *accountConnection) func() {
	if state.account.ID == 0 {
		return nil
	}
	state.leases++
	state.idleEpoch++
	if state.idleTimer != nil {
		state.idleTimer.Stop()
		state.idleTimer = nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.accounts[state.account.ID] != state || state.closing || m.closed {
				return
			}
			state.leases--
			if state.leases != 0 {
				return
			}
			state.idleEpoch++
			epoch := state.idleEpoch
			state.idleTimer = time.AfterFunc(m.cfg.AccountIdleGrace, func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if m.accounts[state.account.ID] == state && state.idleEpoch == epoch && state.leases == 0 {
					m.stopAccountLocked(state)
				}
			})
		})
	}
}

func (m *Manager) stopAccountLocked(state *accountConnection) {
	state.closing = true
	state.idleEpoch++
	if state.idleTimer != nil {
		state.idleTimer.Stop()
		state.idleTimer = nil
	}
	state.cancel()
}

func (m *Manager) finishAccount(state *accountConnection, endpoint ivnp.DestinationEndpoint) {
	m.mu.Lock()
	m.stopAccountLocked(state)
	m.mu.Unlock()
	if endpoint != nil {
		if err := endpoint.Close(); err != nil {
			slog.Warn("I2P endpoint cleanup failed", "account_id", state.account.ID, "error", err)
		}
	}
	state.local.ReleaseSensitive()
	state.mu.Lock()
	state.connection = nil
	clear(state.joined)
	clear(state.unavailable)
	state.mu.Unlock()
	// Emit before removing the slot so a replacement cannot publish status first.
	m.status(state.account.ID, "stopped", "IRC account connection stopped")
	m.mu.Lock()
	state.account.Password = ""
	if m.accounts[state.account.ID] == state {
		delete(m.accounts, state.account.ID)
	}
	close(state.done)
	m.mu.Unlock()
}

func (m *Manager) runAccount(state *accountConnection) {
	var endpoint ivnp.DestinationEndpoint
	defer func() { m.finishAccount(state, endpoint) }()
	select {
	case <-state.ctx.Done():
		return
	case <-m.ready:
	}
	m.mu.Lock()
	startErr := m.startErr
	m.mu.Unlock()
	if startErr != nil {
		return
	}
	delay := 5 * time.Second
	for state.ctx.Err() == nil {
		if endpoint == nil {
			var err error
			endpoint, err = m.createDestination(state.ctx, ivnp.DestinationSpec{Local: state.local})
			if err != nil {
				slog.Warn("I2P destination creation failed", "error", err)
				m.status(state.account.ID, "connecting", "I2P identity endpoint unavailable; retrying")
				if !pause(state.ctx, delay) {
					return
				}
				delay = min(delay*2, 2*time.Minute)
				continue
			}
		}
		m.status(state.account.ID, "connecting", "Waiting for I2P destination tunnels")
		ready, ok := endpoint.(ivnp.ReadyDestinationEndpoint)
		if !ok {
			m.status(state.account.ID, "error", errNoReadiness.Error())
			return
		}
		stage := "destination readiness"
		readyCtx, cancel := context.WithTimeout(state.ctx, 5*time.Minute)
		err := ready.WaitReady(readyCtx)
		cancel()
		if err == nil {
			stage = "IRC dial"
			m.status(state.account.ID, "connecting", "Connecting to IRC over I2P")
			dialCtx, dialCancel := context.WithTimeout(state.ctx, 2*time.Minute)
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
		if state.ctx.Err() != nil {
			return
		}
		slog.Warn("IRC connection attempt failed", "account_id", state.account.ID, "stage", stage, "error", err)
		m.status(state.account.ID, "disconnected", "IRC connection failed during "+stage+"; reconnecting without replaying messages")
		if !pause(state.ctx, delay) {
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
	stopping := state != nil && state.closing
	m.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if state == nil || stopping {
		return ErrNotConnected
	}
	if err := state.ctx.Err(); err != nil {
		return err
	}
	writeCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(state.ctx, cancel)
	defer stop()
	defer cancel()
	state.mu.Lock()
	conn := state.connection
	joined := state.joined[fold(canonical)]
	state.mu.Unlock()
	if conn == nil || !joined {
		return ErrNotConnected
	}
	return conn.write(writeCtx, line)
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
		for _, state := range m.accounts {
			m.stopAccountLocked(state)
		}
		m.mu.Unlock()
		if started {
			m.wg.Wait()
			m.accountWG.Wait()
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
