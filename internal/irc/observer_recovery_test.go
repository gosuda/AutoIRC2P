package irc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/foundation"
)

type recoveryAttempt struct {
	peer       net.Conn
	reader     *bufio.Reader
	endpoint   *recoveryEndpoint
	address    string
	signingErr error
}

type recoveryEndpoint struct {
	ivnp.DestinationEndpoint
	local    *foundation.LocalDestination
	attempts chan<- recoveryAttempt
	mu       sync.Mutex
	failure  error
	streams  []net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (e *recoveryEndpoint) WaitReady(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return errors.Join(ctx.Err(), e.failure)
}

func (e *recoveryEndpoint) DialI2P(ctx context.Context, _ string) (net.Conn, error) {
	e.mu.Lock()
	if err := errors.Join(ctx.Err(), e.failure); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	client, peer := net.Pipe()
	e.streams = append(e.streams, client, peer)
	_, signingErr := e.local.Sign([]byte("observer reconnect"))
	attempt := recoveryAttempt{peer: peer, reader: bufio.NewReader(peer), endpoint: e, address: e.local.B32(), signingErr: signingErr}
	e.mu.Unlock()
	select {
	case e.attempts <- attempt:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = peer.Close()
		return nil, ctx.Err()
	}
}

func (e *recoveryEndpoint) Close() error {
	e.once.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.failure == nil {
			e.failure = net.ErrClosed
		}
		for _, stream := range e.streams {
			_ = stream.Close()
		}
		close(e.closed)
	})
	return nil
}

type recoveryEvents struct {
	mu     sync.Mutex
	events []Event
}

func (events *recoveryEvents) statusCount(state string) int {
	events.mu.Lock()
	defer events.mu.Unlock()
	count := 0
	for _, event := range events.events {
		if event.Kind == "status" && event.State == state {
			count++
		}
	}
	return count
}

func newRecoveryManager(t *testing.T) (*Manager, <-chan recoveryAttempt, *recoveryEvents) {
	t.Helper()
	attempts := make(chan recoveryAttempt, 8)
	manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
		return &recoveryEndpoint{local: spec.Local, attempts: attempts, closed: make(chan struct{})}, nil
	})
	manager.cfg.Rooms = []string{"#first"}
	events := &recoveryEvents{}
	manager.onEvent = func(event Event) {
		events.mu.Lock()
		defer events.mu.Unlock()
		events.events = append(events.events, event)
	}
	return manager, attempts, events
}

func receiveRecoveryAttempt(t *testing.T, attempts <-chan recoveryAttempt, account Account) recoveryAttempt {
	t.Helper()
	var attempt recoveryAttempt
	select {
	case attempt = <-attempts:
	case <-time.After(time.Minute):
		t.Fatal("IRC attempt did not start")
	}
	if attempt.address != account.Identity.Address || attempt.signingErr != nil {
		t.Fatalf("reconnect identity = %q, signing error = %v; want usable %q", attempt.address, attempt.signingErr, account.Identity.Address)
	}
	if err := attempt.peer.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	readRecoveryLine(t, attempt, "NICK "+account.Nick+"\r\n")
	if _, err := attempt.reader.ReadString('\n'); err != nil {
		t.Fatalf("read IRC user registration: %v", err)
	}
	return attempt
}

func readRecoveryLine(t *testing.T, attempt recoveryAttempt, want string) {
	t.Helper()
	if got, err := attempt.reader.ReadString('\n'); err != nil || got != want {
		t.Fatalf("IRC frame = %q, %v; want %q", got, err, want)
	}
}

func writeRecoveryLine(t *testing.T, attempt recoveryAttempt, line string) {
	t.Helper()
	if _, err := io.WriteString(attempt.peer, line+"\r\n"); err != nil {
		t.Fatal(err)
	}
}

func retryRecoveryAttempt(t *testing.T, attempts <-chan recoveryAttempt, account Account, delay time.Duration) recoveryAttempt {
	t.Helper()
	synctest.Wait()
	<-time.After(delay - time.Nanosecond)
	synctest.Wait()
	select {
	case <-attempts:
		t.Fatalf("IRC retried before %s backoff elapsed", delay)
	default:
	}
	<-time.After(time.Nanosecond)
	synctest.Wait()
	if len(attempts) == 0 {
		t.Fatalf("IRC did not retry after %s backoff", delay)
	}
	return receiveRecoveryAttempt(t, attempts, account)
}

func joinRecoveryRoom(t *testing.T, manager *Manager, attempt recoveryAttempt, account Account) {
	t.Helper()
	writeRecoveryLine(t, attempt, ":irc.example.i2p 001 "+account.Nick+" :Welcome")
	readRecoveryLine(t, attempt, "WHOIS NickServ\r\n")
	readRecoveryLine(t, attempt, "JOIN #first\r\n")
	writeRecoveryLine(t, attempt, ":"+account.Nick+"!u@host JOIN :#first")
	synctest.Wait()
	if got := manager.RoomState(account.ID, "#first"); got != RoomReady {
		t.Fatalf("room after own JOIN = %s, want ready", got)
	}
}

func TestObserverRetriesNicknameRejectionWithoutStopping(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		first := receiveRecoveryAttempt(t, attempts, observer)
		writeRecoveryLine(t, first, ":irc.example.i2p 433 * observer :Nickname in use")
		second := retryRecoveryAttempt(t, attempts, observer, 5*time.Second)
		joinRecoveryRoom(t, manager, second, observer)
		if got := events.statusCount("stopped"); got != 0 {
			t.Fatalf("observer emitted %d stopped events during nickname recovery", got)
		}
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatalf("observer lost its pinned account after recovery: %v", err)
		}
	})
}

func TestObserverBackoffResetsOnlyAfterValidatedWelcome(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		first := receiveRecoveryAttempt(t, attempts, observer)
		_ = first.peer.Close()
		second := retryRecoveryAttempt(t, attempts, observer, 5*time.Second)
		writeRecoveryLine(t, second, ":irc.example.i2p 001 someoneelse :Wrong recipient")
		writeRecoveryLine(t, second, ":mallory!u@host 001 observer :Not a server")
		_ = second.peer.Close()
		third := retryRecoveryAttempt(t, attempts, observer, 10*time.Second)
		joinRecoveryRoom(t, manager, third, observer)
		_ = third.peer.Close()
		fourth := retryRecoveryAttempt(t, attempts, observer, 5*time.Second)
		_ = fourth.peer.Close()
		fifth := retryRecoveryAttempt(t, attempts, observer, 10*time.Second)
		joinRecoveryRoom(t, manager, fifth, observer)
		if got := events.statusCount("stopped"); got != 0 {
			t.Fatalf("observer emitted %d stopped events during reconnects", got)
		}
	})
}

func TestObserverRecreatesRouterInvalidatedEndpoint(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{name: "router canceled", cause: context.Canceled},
		{name: "endpoint closed", cause: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager, attempts, events := newRecoveryManager(t)
				if err := manager.ConnectObserver(t.Context(), observer); err != nil {
					t.Fatal(err)
				}
				first := receiveRecoveryAttempt(t, attempts, observer)
				joinRecoveryRoom(t, manager, first, observer)
				first.endpoint.mu.Lock()
				first.endpoint.failure = tc.cause
				first.endpoint.mu.Unlock()
				if err := first.endpoint.Close(); err != nil {
					t.Fatal(err)
				}
				second := receiveRecoveryAttempt(t, attempts, observer)
				joinRecoveryRoom(t, manager, second, observer)
				if got := events.statusCount("stopped"); got != 0 {
					t.Fatalf("observer emitted %d stopped events while replacing its endpoint", got)
				}
			})
		})
	}
}

func TestObserverShutdownDuringBackoffClosesResources(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		first := receiveRecoveryAttempt(t, attempts, observer)
		writeRecoveryLine(t, first, ":irc.example.i2p 433 * observer :Nickname in use")
		synctest.Wait()
		if got := events.statusCount("stopped"); got != 0 {
			t.Fatalf("observer stopped before shutdown during recoverable rejection: %d events", got)
		}
		<-time.After(4 * time.Second)
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-first.endpoint.closed:
		default:
			t.Fatal("shutdown returned without closing observer endpoint")
		}
		if _, err := first.reader.ReadString('\n'); err == nil {
			t.Fatal("observer stream remained readable after shutdown")
		}
		<-time.After(3 * time.Minute)
		synctest.Wait()
		select {
		case <-attempts:
			t.Fatal("observer retried after shutdown canceled its backoff")
		default:
		}
		if got := events.statusCount("stopped"); got != 1 {
			t.Fatalf("shutdown emitted %d stopped events, want one", got)
		}
	})
}

func TestRegisteredAccountNicknameRejectionRemainsTerminal(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	account.Registered = true
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		first := receiveRecoveryAttempt(t, attempts, account)
		writeRecoveryLine(t, first, ":irc.example.i2p 433 * alice :Nickname in use")
		synctest.Wait()
		if got := events.statusCount("stopped"); got != 1 {
			t.Fatalf("registered account rejection emitted %d stopped events, want one", got)
		}
		if _, err := manager.Retain(t.Context(), account.ID); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("retaining rejected account = %v, want not connected", err)
		}
		<-time.After(3 * time.Minute)
		synctest.Wait()
		select {
		case <-attempts:
			t.Fatal("registered account automatically retried unavailable nickname")
		default:
		}
		select {
		case <-first.endpoint.closed:
		default:
			t.Fatal("terminal nickname rejection retained its endpoint")
		}
	})
}
