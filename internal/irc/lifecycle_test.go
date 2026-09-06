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

type leaseEndpoint struct {
	ivnp.DestinationEndpoint
	local   *foundation.LocalDestination
	stream  net.Conn
	closing chan struct{}
	closed  chan struct{}
	drain   <-chan struct{}
}

func (e *leaseEndpoint) WaitReady(ctx context.Context) error {
	if e.stream != nil {
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (e *leaseEndpoint) DialI2P(ctx context.Context, _ string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.stream, nil
}

func (e *leaseEndpoint) Close() error {
	close(e.closing)
	if e.drain != nil {
		<-e.drain
	}
	close(e.closed)
	return nil
}

func leaseAccount(t *testing.T, id int64, nick string) Account {
	t.Helper()
	identity, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(identity.Keys) })
	return Account{ID: id, Nick: nick, Password: "offline-only-password", Identity: identity}
}

func leaseManager(t *testing.T, capacity int, create func(context.Context, ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error)) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	close(ready)
	manager := &Manager{
		cfg: Config{Server: "offline.b32.i2p:6667", Rooms: []string{"#first", "#second"}, MaxAccounts: capacity, AccountIdleGrace: time.Minute},
		ctx: ctx, cancel: cancel, started: true, ready: ready,
		rooms:             map[string]string{"#first": "#first", "#second": "#second"},
		accounts:          make(map[int64]*accountConnection),
		createDestination: create, onEvent: func(Event) {},
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	return manager
}

func waitingEndpoint(spec ivnp.DestinationSpec) *leaseEndpoint {
	return &leaseEndpoint{local: spec.Local, closing: make(chan struct{}), closed: make(chan struct{})}
}

func assertEndpointOpen(t *testing.T, endpoint *leaseEndpoint) {
	t.Helper()
	select {
	case <-endpoint.closing:
		t.Fatal("leased endpoint started closing")
	default:
	}
}

func TestAccountLeasesPreserveIdentityAcrossGraceAndReplacement(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		created := make(chan *leaseEndpoint, 4)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			created <- endpoint
			return endpoint, nil
		})
		first, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := <-created
		second, err := manager.Retain(t.Context(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		first()
		first()
		<-time.After(2 * time.Minute)
		assertEndpointOpen(t, endpoint)
		second()
		<-time.After(30 * time.Second)
		third, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		<-time.After(time.Minute)
		assertEndpointOpen(t, endpoint)
		third()
		<-endpoint.closed
		synctest.Wait()
		if _, err := endpoint.local.Sign([]byte("released owner")); err == nil {
			t.Fatal("expired account still owns usable private keys")
		}
		if _, err := manager.Retain(t.Context(), account.ID); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("retain after teardown = %v, want not connected", err)
		}
		replacementRelease, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		replacement := <-created
		if replacement.local.B32() != account.Identity.Address {
			t.Fatal("reattach replaced the persisted identity")
		}
		if _, err := replacement.local.Sign([]byte("restored owner")); err != nil {
			t.Fatalf("reattach lost private signing key: %v", err)
		}
		first()
		second()
		third()
		<-time.After(2 * time.Minute)
		assertEndpointOpen(t, replacement)
		replacementRelease()
		<-replacement.closed
	})
}

func TestAccountClosingKeepsCapacityAndAllowsCancelableReacquisition(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	other := leaseAccount(t, 2, "bob")
	synctest.Test(t, func(t *testing.T) {
		created := make(chan *leaseEndpoint, 4)
		drain := make(chan struct{})
		var drainOnce sync.Once
		unblock := func() { drainOnce.Do(func() { close(drain) }) }
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			endpoint.drain = drain
			created <- endpoint
			return endpoint, nil
		})
		t.Cleanup(unblock)
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := <-created
		release()
		<-endpoint.closing
		if _, err := manager.Acquire(t.Context(), other); !errors.Is(err, ErrAccountCapacity) {
			t.Fatalf("admission during endpoint drain = %v, want capacity error", err)
		}
		if _, err := manager.Retain(t.Context(), account.ID); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("retain closing account = %v, want not connected", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			release, err := manager.Acquire(ctx, account)
			if release != nil {
				release()
			}
			result <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel admission waiting for teardown = %v", err)
		}
		acquired := make(chan func(), 1)
		go func() {
			release, err := manager.Acquire(t.Context(), account)
			result <- err
			acquired <- release
		}()
		synctest.Wait()
		select {
		case <-created:
			t.Fatal("replacement destination overlapped closing identity")
		default:
		}
		unblock()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		replacementRelease := <-acquired
		replacement := <-created
		release()
		<-time.After(2 * time.Minute)
		assertEndpointOpen(t, replacement)
		replacementRelease()
		<-replacement.closed
		synctest.Wait()
		otherRelease, err := manager.Acquire(t.Context(), other)
		if err != nil {
			t.Fatalf("capacity was not returned after actual teardown: %v", err)
		}
		otherRelease()
	})
}

func TestObserverIsPinnedOutsideRegisteredAccountCapacity(t *testing.T) {
	observer := leaseAccount(t, 0, "observer")
	account := leaseAccount(t, 1, "alice")
	other := leaseAccount(t, 2, "bob")
	synctest.Test(t, func(t *testing.T) {
		created := make(chan *leaseEndpoint, 4)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			created <- endpoint
			return endpoint, nil
		})
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		observerEndpoint := <-created
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatalf("observer consumed registered capacity: %v", err)
		}
		accountEndpoint := <-created
		if _, err := manager.Acquire(t.Context(), other); !errors.Is(err, ErrAccountCapacity) {
			t.Fatalf("registered capacity overflow = %v", err)
		}
		if _, err := manager.Retain(t.Context(), 0); !errors.Is(err, ErrObserverReadOnly) {
			t.Fatalf("retaining observer for send = %v", err)
		}
		if err := manager.Send(t.Context(), 0, "#first", "never written"); !errors.Is(err, ErrObserverReadOnly) {
			t.Fatalf("observer send = %v", err)
		}
		release()
		<-accountEndpoint.closed
		<-time.After(2 * time.Minute)
		assertEndpointOpen(t, observerEndpoint)
		var closes sync.WaitGroup
		for range 4 {
			closes.Go(func() {
				if err := manager.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		closes.Wait()
		<-observerEndpoint.closed
		if _, err := manager.Acquire(t.Context(), account); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("admission after shutdown = %v", err)
		}
	})
}

func TestAccountGraceClosesStreamAndCancelsPendingJoins(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	account.Password = "offline-only-password"
	synctest.Test(t, func(t *testing.T) {
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		created := make(chan *leaseEndpoint, 1)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			endpoint.stream = client
			created <- endpoint
			return endpoint, nil
		})
		manager.cfg.AccountIdleGrace = time.Second
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := <-created
		reader := bufio.NewReader(peer)
		for range 2 {
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.WriteString(peer, ":irc.example.i2p 001 alice :Welcome\r\n"); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"WHOIS NickServ\r\n", "JOIN #first\r\n"} {
			if got, err := reader.ReadString('\n'); err != nil || got != want {
				t.Fatalf("registration = %q, %v; want %q", got, err, want)
			}
		}
		release()
		remaining, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 0 {
			t.Fatalf("account emitted traffic after grace: %q", remaining)
		}
		<-endpoint.closed
		synctest.Wait()
		if err := manager.ctx.Err(); err != nil {
			t.Fatalf("account expiry stopped shared router: %v", err)
		}
		if got := manager.RoomState(account.ID, "#first"); got == RoomReady {
			t.Fatal("expired connection left room ready")
		}
	})
}

func TestAccountExpiryCancelsRouterStartupWait(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		manager := leaseManager(t, 1, func(context.Context, ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			t.Error("created a destination before the router was ready")
			return nil, net.ErrClosed
		})
		manager.ready = make(chan struct{})
		stopped := make(chan struct{})
		manager.onEvent = func(event Event) {
			if event.State == "stopped" {
				close(stopped)
			}
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		release()
		<-stopped
		synctest.Wait()
		if _, err := manager.Retain(t.Context(), account.ID); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("startup-waiting account survived grace: %v", err)
		}
		if err := manager.ctx.Err(); err != nil {
			t.Fatalf("expiring startup waiter canceled router: %v", err)
		}
	})
}

func TestShutdownCancelsBlockedAccountSend(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			endpoint.stream = client
			return endpoint, nil
		})
		manager.cfg.Rooms = []string{"#first"}
		joined := make(chan struct{})
		manager.onEvent = func(event Event) {
			if event.Kind == "membership" && event.State == string(RoomReady) {
				close(joined)
			}
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		reader := bufio.NewReader(peer)
		for range 2 {
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.WriteString(peer, ":irc.example.i2p 001 alice :Welcome\r\n"); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.WriteString(peer, ":alice!u@host JOIN :#first\r\n"); err != nil {
			t.Fatal(err)
		}
		<-joined
		result := make(chan error, 1)
		go func() { result <- manager.Send(t.Context(), account.ID, "#first", "local pipe only") }()
		synctest.Wait()
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err == nil {
			t.Fatal("blocked channel write succeeded after shutdown")
		}
		if got := manager.RoomState(account.ID, "#first"); got != RoomUnavailable {
			t.Fatalf("shutdown room state = %s", got)
		}
	})
}
