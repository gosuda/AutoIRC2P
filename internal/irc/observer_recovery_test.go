package irc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/dataplane"
	"gosuda.org/ivnp/foundation"
)

type recoveryAttempt struct {
	peer       net.Conn
	reader     *bufio.Reader
	endpoint   *recoveryEndpoint
	address    string
	nick       string
	signingErr error
}

type recoveryEndpoint struct {
	destinationEndpoint
	local       *foundation.LocalDestination
	attempts    chan<- recoveryAttempt
	mu          sync.Mutex
	failure     error
	dialFailure error
	readiness   <-chan struct{}
	streams     []net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (e *recoveryEndpoint) WaitReady(ctx context.Context) error {
	e.mu.Lock()
	readiness, failure := e.readiness, e.failure
	e.mu.Unlock()
	if readiness != nil {
		select {
		case <-readiness:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(ctx.Err(), failure)
}

func (e *recoveryEndpoint) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	e.mu.Lock()
	if err := errors.Join(ctx.Err(), e.failure, e.dialFailure); err != nil {
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

func newRecoveryManager(t *testing.T) (*Manager, chan recoveryAttempt, *recoveryEvents) {
	t.Helper()
	attempts := make(chan recoveryAttempt, 8)
	manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
		return &recoveryEndpoint{local: spec.Identity, attempts: attempts, closed: make(chan struct{})}, nil
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
	attempt.nick = readSessionNick(t, attempt.reader)
	if account.ID == 0 && account.Password == "" {
		if matched, err := regexp.MatchString(`^Irc2PGuest[0-9]{5}$`, attempt.nick); err != nil || !matched {
			t.Fatalf("guest nickname = %q, want Irc2PGuest and five digits: %v", attempt.nick, err)
		}
	} else if attempt.nick != account.Nick {
		t.Fatalf("authenticated nickname = %q, want %q", attempt.nick, account.Nick)
	}
	readRecoveryLine(t, attempt, "USER "+attempt.nick+" 0 * :HexChat\r\n")
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
	writeRecoveryLine(t, attempt, ":irc.example.i2p 001 "+attempt.nick+" :Welcome")
	if account.ID != 0 || account.Password != "" {
		readRecoveryLine(t, attempt, "WHOIS NickServ\r\n")
	}
	readRecoveryLine(t, attempt, "JOIN #first\r\n")
	writeRecoveryLine(t, attempt, ":"+attempt.nick+"!u@host JOIN :#first")
	synctest.Wait()
	if got := manager.RoomState(account.ID, "#first"); got != RoomReady {
		t.Fatalf("room after own JOIN = %s, want ready", got)
	}
}

func TestObserverRetriesNicknameRejectionWithoutStopping(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	observer.Password = ""
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		first := receiveRecoveryAttempt(t, attempts, observer)
		writeRecoveryLine(t, first, ":irc.example.i2p 433 * "+first.nick+" :Nickname in use")
		second := retryRecoveryAttempt(t, attempts, observer, time.Second)
		if second.nick == first.nick {
			t.Fatalf("nickname rejection reused %q", first.nick)
		}
		joinRecoveryRoom(t, manager, second, observer)
		writeRecoveryLine(t, second, "ERROR :Disconnected")
		third := retryRecoveryAttempt(t, attempts, observer, time.Second)
		if third.nick == second.nick {
			t.Fatalf("reconnect reused %q", second.nick)
		}
		joinRecoveryRoom(t, manager, third, observer)
		if got := events.statusCount("stopped"); got != 0 {
			t.Fatalf("observer emitted %d stopped events during nickname recovery", got)
		}
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatalf("observer lost its pinned account after recovery: %v", err)
		}
	})
}

func TestObserverRetriesEverySecondBeforeAndAfterWelcome(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		first := receiveRecoveryAttempt(t, attempts, observer)
		_ = first.peer.Close()
		second := retryRecoveryAttempt(t, attempts, observer, time.Second)
		writeRecoveryLine(t, second, ":irc.example.i2p 001 someoneelse :Wrong recipient")
		writeRecoveryLine(t, second, ":mallory!u@host 001 observer :Not a server")
		_ = second.peer.Close()
		third := retryRecoveryAttempt(t, attempts, observer, time.Second)
		joinRecoveryRoom(t, manager, third, observer)
		_ = third.peer.Close()
		fourth := retryRecoveryAttempt(t, attempts, observer, time.Second)
		_ = fourth.peer.Close()
		fifth := retryRecoveryAttempt(t, attempts, observer, time.Second)
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
		<-time.After(500 * time.Millisecond)
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

func TestRegisteredAccountRecoversWhenStaleNicknameExpires(t *testing.T) {
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
		second := retryRecoveryAttempt(t, attempts, account, time.Second)
		joinRecoveryRoom(t, manager, second, account)
		if got := events.statusCount("stopped"); got != 0 {
			t.Fatalf("nickname recovery stopped the leased account %d times", got)
		}
		retain, err := manager.Retain(t.Context(), account.ID)
		if err != nil {
			t.Fatalf("recovered account cannot send: %v", err)
		}
		retain()
	})
}

func TestObserverKeepsReceivingWithoutUserLeases(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, events := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		connection := receiveRecoveryAttempt(t, attempts, observer)
		joinRecoveryRoom(t, manager, connection, observer)
		for range 45 {
			if err := connection.peer.SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			readRecoveryLine(t, connection, "PING :autoirc2p\r\n")
			if elapsed := time.Since(before); elapsed != 30*time.Second {
				t.Fatalf("keepalive interval = %s, want 30s", elapsed)
			}
			writeRecoveryLine(t, connection, ":irc.example.i2p PONG irc.example.i2p :autoirc2p")
		}
		writeRecoveryLine(t, connection, ":alice!u@host PRIVMSG #first :still receiving")
		synctest.Wait()
		if got := manager.RoomState(0, "#first"); got != RoomReady {
			t.Fatalf("observer room after idle period = %s, want ready", got)
		}
		events.mu.Lock()
		defer events.mu.Unlock()
		for _, event := range events.events {
			if event.Kind == "message" && event.Room == "#first" && event.Text == "still receiving" {
				return
			}
		}
		t.Fatal("observer did not receive chat after 22 minutes without user leases")
	})
}

func TestObserverReconnectsWhenServerStopsResponding(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	observer.Password = ""
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		manager.cfg.IdleTimeout = 75 * time.Second
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		connection := receiveRecoveryAttempt(t, attempts, observer)
		if err := connection.peer.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			t.Fatal(err)
		}
		writeRecoveryLine(t, connection, ":irc.example.i2p 001 "+connection.nick+" :Welcome")
		readRecoveryLine(t, connection, "JOIN #first\r\n")
		readRecoveryLine(t, connection, "PING :autoirc2p\r\n")
		readRecoveryLine(t, connection, "PING :autoirc2p\r\n")
		if line, err := connection.reader.ReadString('\n'); !errors.Is(err, io.EOF) {
			t.Fatalf("unresponsive server connection = %q, %v; want EOF", line, err)
		}
		reconnected := retryRecoveryAttempt(t, attempts, observer, time.Second)
		if reconnected.nick == connection.nick {
			t.Fatalf("idle timeout reused %q", connection.nick)
		}
	})
}

func TestChannelTemporarilyUnavailableKeepsIRCConnection(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		connection := receiveRecoveryAttempt(t, attempts, observer)
		joinRecoveryRoom(t, manager, connection, observer)
		writeRecoveryLine(t, connection, ":irc.example.i2p 437 observer #first :Channel temporarily unavailable")
		synctest.Wait()
		if got := manager.RoomState(observer.ID, "#first"); got != RoomUnavailable {
			t.Fatalf("rejected channel = %s, want unavailable", got)
		}
		writeRecoveryLine(t, connection, "PING :still-connected")
		readRecoveryLine(t, connection, "PONG :still-connected\r\n")
	})
}

func TestObserverReplacesStaleTunnelRouteWithoutChangingIdentity(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		first := receiveRecoveryAttempt(t, attempts, observer)
		joinRecoveryRoom(t, manager, first, observer)
		first.endpoint.mu.Lock()
		first.endpoint.dialFailure = dataplane.TunnelErrCircuitNotFound
		first.endpoint.mu.Unlock()
		if err := first.peer.Close(); err != nil {
			t.Fatal(err)
		}
		second := retryRecoveryAttempt(t, attempts, observer, 2*time.Second)
		joinRecoveryRoom(t, manager, second, observer)
	})
}

func TestReconnectReusesReadyDestinationDuringLeaseSetRenewal(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		first := receiveRecoveryAttempt(t, attempts, account)
		joinRecoveryRoom(t, manager, first, account)
		first.endpoint.mu.Lock()
		first.endpoint.readiness = make(chan struct{})
		first.endpoint.mu.Unlock()
		if err := first.peer.Close(); err != nil {
			t.Fatal(err)
		}
		second := retryRecoveryAttempt(t, attempts, account, time.Second)
		if second.endpoint != first.endpoint {
			t.Fatal("IRC reconnect replaced a reusable I2P session")
		}
		joinRecoveryRoom(t, manager, second, account)
	})
}

func TestReconnectRotatesMaintainedDestinationsBeforeReuse(t *testing.T) {
	account := pooledLeaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		created := make(chan *recoveryEndpoint, DestinationPoolSize)
		manager.createDestination = func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			endpoint := &recoveryEndpoint{local: spec.Identity, attempts: attempts, closed: make(chan struct{})}
			created <- endpoint
			return endpoint, nil
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		account.ReleaseSensitive()
		first := receiveRecoveryAttempt(t, attempts, account)
		endpoints := make(map[string]*recoveryEndpoint, DestinationPoolSize)
		for range DestinationPoolSize {
			endpoint := <-created
			endpoints[endpoint.local.B32()] = endpoint
		}
		current := first
		for _, selected := range []int{1, 2, 0, 1} {
			joinRecoveryRoom(t, manager, current, account)
			<-time.After(2 * time.Second)
			synctest.Wait()
			select {
			case <-attempts:
				t.Fatal("opened another IRC socket while the selected session was active")
			default:
			}
			current.endpoint.mu.Lock()
			current.endpoint.readiness = make(chan struct{})
			current.endpoint.mu.Unlock()
			writeRecoveryLine(t, current, "ERROR :Disconnected")
			previous := current
			expected := account
			expected.Identity = account.identityAt(selected)
			current = retryRecoveryAttempt(t, attempts, expected, time.Second)
			if _, err := previous.reader.ReadString('\n'); err == nil {
				t.Fatal("previous IRC socket remained open after reconnect")
			}
			if current.endpoint != endpoints[account.identityAt(selected).Address] {
				t.Fatalf("slot %d recreated a maintained destination", selected)
			}
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		for address, endpoint := range endpoints {
			select {
			case <-endpoint.closed:
			default:
				t.Errorf("shutdown left destination %s open", address)
			}
			if _, err := endpoint.local.Sign([]byte("after shutdown")); err == nil {
				t.Errorf("shutdown left destination %s private keys usable", address)
			}
		}
	})
}

func TestPoolReplacesOnlyStaleSelectedDestination(t *testing.T) {
	observer := pooledLeaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		created := make(chan *recoveryEndpoint, DestinationPoolSize+1)
		manager.createDestination = func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			endpoint := &recoveryEndpoint{local: spec.Identity, attempts: attempts, closed: make(chan struct{})}
			created <- endpoint
			return endpoint, nil
		}
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		current := receiveRecoveryAttempt(t, attempts, observer)
		endpoints := make(map[string]*recoveryEndpoint, DestinationPoolSize)
		for range DestinationPoolSize {
			endpoint := <-created
			endpoints[endpoint.local.B32()] = endpoint
		}
		primary := current.endpoint
		primary.mu.Lock()
		primary.dialFailure = dataplane.TunnelErrCircuitNotFound
		primary.mu.Unlock()
		for i, selected := range []int{1, 2, 1, 2, 0} {
			writeRecoveryLine(t, current, ":irc.example.i2p 433 * observer :Nickname in use")
			expected := observer
			expected.Identity = observer.identityAt(selected)
			delay := time.Second
			if i == 2 {
				delay = 2 * time.Second
			}
			current = retryRecoveryAttempt(t, attempts, expected, delay)
			if selected != 0 && current.endpoint != endpoints[observer.identityAt(selected).Address] {
				t.Fatalf("stale primary replaced healthy slot %d", selected)
			}
		}
		if current.endpoint == primary {
			t.Fatal("stale destination endpoint was not replaced")
		}
		if replacement := <-created; replacement != current.endpoint || len(created) != 0 {
			t.Fatal("stale primary replacement recreated more than the selected destination")
		}
		select {
		case <-primary.closed:
		default:
			t.Fatal("stale primary endpoint was not closed")
		}
		joinRecoveryRoom(t, manager, current, observer)
	})
}

var errDestinationCreation = errors.New("destination temporarily unavailable")

func TestPoolCreationFailureDoesNotDiscardHealthyDestinations(t *testing.T) {
	observer := pooledLeaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		manager, attempts, _ := newRecoveryManager(t)
		created := make(chan *recoveryEndpoint, DestinationPoolSize+1)
		var failPrimary sync.Once
		manager.createDestination = func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			endpoint := &recoveryEndpoint{local: spec.Identity, attempts: attempts, closed: make(chan struct{})}
			created <- endpoint
			fail := false
			if spec.Identity.B32() == observer.Identity.Address {
				failPrimary.Do(func() { fail = true })
			}
			if fail {
				return endpoint, errDestinationCreation
			}
			return endpoint, nil
		}
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		expected := observer
		expected.Identity = observer.Alternates[0]
		current := receiveRecoveryAttempt(t, attempts, expected)
		endpoints := make(map[string]*recoveryEndpoint, DestinationPoolSize)
		for range DestinationPoolSize {
			endpoint := <-created
			endpoints[endpoint.local.B32()] = endpoint
		}
		if current.endpoint != endpoints[observer.Alternates[0].Address] {
			t.Fatal("initial creation failure prevented using an already-created alternate")
		}
		select {
		case <-endpoints[observer.Identity.Address].closed:
		default:
			t.Fatal("endpoint returned with a creation error leaked")
		}
		for _, selected := range []int{2, 0, 1} {
			writeRecoveryLine(t, current, ":irc.example.i2p 433 * observer :Nickname in use")
			expected.Identity = observer.identityAt(selected)
			current = retryRecoveryAttempt(t, attempts, expected, time.Second)
		}
		replacement := <-created
		if current.endpoint != endpoints[observer.Alternates[0].Address] || replacement.local.B32() != observer.Identity.Address || len(created) != 0 {
			t.Fatal("partial creation recovery discarded a healthy alternate")
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []*recoveryEndpoint{replacement, endpoints[observer.Identity.Address], endpoints[observer.Alternates[0].Address], endpoints[observer.Alternates[1].Address]} {
			select {
			case <-endpoint.closed:
			default:
				t.Errorf("partial creation cleanup left endpoint %s open", endpoint.local.B32())
			}
			if _, err := endpoint.local.Sign([]byte("after partial creation cleanup")); err == nil {
				t.Errorf("partial creation cleanup left endpoint %s private keys usable", endpoint.local.B32())
			}
		}
	})
}
