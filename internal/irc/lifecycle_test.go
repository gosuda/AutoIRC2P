package irc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/foundation"
)

type leaseEndpoint struct {
	destinationEndpoint
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

func (e *leaseEndpoint) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
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

func pooledLeaseAccount(t *testing.T, id int64, nick string) Account {
	t.Helper()
	account := leaseAccount(t, id, nick)
	for range DestinationPoolSize - 1 {
		identity, err := GenerateIdentity()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { clear(identity.Keys) })
		account.Alternates = append(account.Alternates, identity)
	}
	return account
}

func leaseManager(t *testing.T, capacity int, create func(context.Context, ivnp.DestinationConfig) (destinationEndpoint, error)) *Manager {
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

func waitingEndpoint(spec ivnp.DestinationConfig) *leaseEndpoint {
	return &leaseEndpoint{local: spec.Identity, closing: make(chan struct{}), closed: make(chan struct{})}
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
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
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
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
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
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
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
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			endpoint.stream = client
			created <- endpoint
			return endpoint, nil
		})
		manager.cfg.AccountIdleGrace = 50 * time.Millisecond
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
		manager := leaseManager(t, 1, func(context.Context, ivnp.DestinationConfig) (destinationEndpoint, error) {
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
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
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

func TestPoolGraceExpiryReleasesEveryDestination(t *testing.T) {
	account := pooledLeaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		created := make(chan *leaseEndpoint, DestinationPoolSize)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			created <- endpoint
			return endpoint, nil
		})
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		endpoints := make(map[string]*leaseEndpoint, DestinationPoolSize)
		for range DestinationPoolSize {
			endpoint := <-created
			endpoints[endpoint.local.B32()] = endpoint
		}
		for i := range DestinationPoolSize {
			if endpoints[account.identityAt(i).Address] == nil {
				t.Fatalf("pool slot %d lost its persisted identity", i)
			}
		}
		release()
		for _, endpoint := range endpoints {
			<-endpoint.closed
		}
		synctest.Wait()
		for address, endpoint := range endpoints {
			if _, err := endpoint.local.Sign([]byte("expired pool")); err == nil {
				t.Errorf("expired pool identity %s still owns private keys", address)
			}
		}
		if _, err := manager.Retain(t.Context(), account.ID); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("retain after pool expiry = %v, want not connected", err)
		}
	})
}

func TestAdmissionRejectsDuplicateAndOversizedDestinationPools(t *testing.T) {
	account := pooledLeaseAccount(t, 1, "alice")
	other := pooledLeaseAccount(t, 2, "bob")
	for _, tc := range []struct {
		name       string
		alternates []Identity
	}{
		{name: "primary repeated", alternates: []Identity{account.Identity}},
		{name: "alternate repeated", alternates: []Identity{other.Identity, other.Identity}},
		{name: "pool over capacity", alternates: []Identity{other.Identity, other.Alternates[0], other.Alternates[1]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := leaseManager(t, 1, func(context.Context, ivnp.DestinationConfig) (destinationEndpoint, error) {
					t.Error("invalid pool created an endpoint")
					return nil, net.ErrClosed
				})
				candidate := account
				candidate.Alternates = tc.alternates
				if release, err := manager.Acquire(t.Context(), candidate); !errors.Is(err, errInvalidAccount) {
					if release != nil {
						release()
					}
					t.Fatalf("invalid pool admission = %v, want invalid account", err)
				}
			})
		})
	}
}

func TestAdmissionKeepsAccountAndObserverPoolsDisjoint(t *testing.T) {
	observer := pooledLeaseAccount(t, 0, "observer")
	account := pooledLeaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			return waitingEndpoint(spec), nil
		})
		if err := manager.ConnectObserver(t.Context(), observer); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			identity  Identity
			alternate Identity
		}{
			{name: "primary collides with alternate", identity: observer.Alternates[0], alternate: account.Alternates[0]},
			{name: "alternate collides with primary", identity: account.Identity, alternate: observer.Identity},
			{name: "alternates collide", identity: account.Identity, alternate: observer.Alternates[1]},
		} {
			candidate := account
			candidate.Identity = tc.identity
			candidate.Alternates = []Identity{tc.alternate}
			if release, err := manager.Acquire(t.Context(), candidate); !errors.Is(err, errInvalidAccount) {
				if release != nil {
					release()
				}
				t.Fatalf("%s admission = %v, want invalid account", tc.name, err)
			}
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatalf("disjoint account pool admission: %v", err)
		}
		defer release()
		changed := account
		changed.Alternates = []Identity{account.Alternates[1], account.Alternates[0]}
		if extra, err := manager.Acquire(t.Context(), changed); !errors.Is(err, errInvalidAccount) {
			if extra != nil {
				extra()
			}
			t.Fatalf("changed retry order on reacquisition = %v, want invalid account", err)
		}
	})
}

func TestAdmissionRejectsUnrestorableAlternateWithoutReservingCapacity(t *testing.T) {
	account := pooledLeaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		created := make(chan *leaseEndpoint, DestinationPoolSize)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			created <- endpoint
			return endpoint, nil
		})
		invalid := account
		invalid.Alternates = []Identity{account.Alternates[0], {Address: account.Alternates[1].Address, Keys: []byte("invalid private destination")}}
		if release, err := manager.Acquire(t.Context(), invalid); err == nil {
			release()
			t.Fatal("unrestorable alternate admitted")
		}
		select {
		case <-created:
			t.Fatal("partially restored pool created an endpoint")
		default:
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatalf("failed restoration retained capacity: %v", err)
		}
		defer release()
		for i := range DestinationPoolSize {
			endpoint := <-created
			if _, err := endpoint.local.Sign([]byte("after rejected restoration")); err != nil {
				t.Errorf("admission corrupted caller identity %d: %v", i, err)
			}
		}
	})
}

func TestManagerRejectsInsufficientDestinationCapacity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		capacity    int
		maxAccounts int
	}{
		{name: "default accounts exceed configured capacity", capacity: 50},
		{name: "account limit cannot overflow pool budget", capacity: 64, maxAccounts: int(^uint(0) >> 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ivnp.conf")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("[state]\nmax_destinations = %d\n", tc.capacity)), 0600); err != nil {
				t.Fatal(err)
			}
			manager, err := New(Config{ConfigPath: path, Server: "offline.b32.i2p:6667", Rooms: []string{"#first"}, MaxAccounts: tc.maxAccounts}, func(Event) {})
			if manager != nil {
				if closeErr := manager.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if !errors.Is(err, errInvalidConfig) {
				t.Fatalf("insufficient destination capacity = %v, want invalid configuration", err)
			}
		})
	}
}
