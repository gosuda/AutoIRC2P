package irc

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
)

type recoveryNode struct {
	startErr  error
	closeErr  error
	waitErr   error
	started   chan struct{}
	closing   chan struct{}
	exited    chan struct{}
	drained   chan struct{}
	closeGate <-chan struct{}
	drainGate <-chan struct{}
	closeOnce sync.Once
	exitOnce  sync.Once
	waitOnce  sync.Once
}

func newRecoveryNode() *recoveryNode {
	return &recoveryNode{
		started: make(chan struct{}), closing: make(chan struct{}),
		exited: make(chan struct{}), drained: make(chan struct{}),
	}
}

func (n *recoveryNode) Start(context.Context) error {
	close(n.started)
	return n.startErr
}

func (n *recoveryNode) Close() error {
	n.closeOnce.Do(func() {
		close(n.closing)
		if n.closeGate != nil {
			<-n.closeGate
		}
		n.exitOnce.Do(func() { close(n.exited) })
	})
	return n.closeErr
}

func (n *recoveryNode) Wait() error {
	<-n.exited
	if n.drainGate != nil {
		<-n.drainGate
	}
	n.waitOnce.Do(func() { close(n.drained) })
	return n.waitErr
}

func (n *recoveryNode) runtime() *routerRuntime {
	return &routerRuntime{node: n, createDestination: func(ctx context.Context, _ ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
		endpoint := &routerRecoveryEndpoint{node: n}
		if err := endpoint.WaitReady(ctx); err != nil {
			return nil, err
		}
		return endpoint, nil
	}}
}

type routerRecoveryEndpoint struct {
	ivnp.DestinationEndpoint
	node *recoveryNode
}

func (e *routerRecoveryEndpoint) WaitReady(ctx context.Context) error {
	select {
	case <-e.node.exited:
		return net.ErrClosed
	default:
		return ctx.Err()
	}
}

func (e *routerRecoveryEndpoint) Close() error { return nil }

func recoveryManager(t *testing.T, runtime *routerRuntime, factory func() (*routerRuntime, error)) *Manager {
	t.Helper()
	m := &Manager{
		cfg:    Config{Server: "offline.b32.i2p:6667", Rooms: []string{"#test"}, MaxAccounts: 1},
		router: runtime, newRouter: factory, ready: make(chan struct{}),
		accounts: make(map[int64]*accountConnection), rooms: map[string]string{"#test": "#test"},
		onEvent: func(Event) {},
	}
	m.createDestination = m.createRouterDestination
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestRouterStartupRetriesAfterClosingAndDrainingPreviousRuntime(t *testing.T) {
	account := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		first, second := newRecoveryNode(), newRecoveryNode()
		first.startErr = errors.New("startup failed")
		first.waitErr = first.startErr
		first.closeErr = first.startErr
		closeGate, drainGate := make(chan struct{}), make(chan struct{})
		unblockClose := sync.OnceFunc(func() { close(closeGate) })
		unblockDrain := sync.OnceFunc(func() { close(drainGate) })
		defer unblockClose()
		defer unblockDrain()
		first.closeGate, first.drainGate = closeGate, drainGate
		opened := make(chan struct{}, 1)
		created := make(chan *leaseEndpoint, 1)
		fresh := second.runtime()
		fresh.createDestination = func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := waitingEndpoint(spec)
			created <- endpoint
			return endpoint, nil
		}
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			opened <- struct{}{}
			return fresh, nil
		})
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.ConnectObserver(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		<-first.closing
		synctest.Wait()
		select {
		case <-opened:
			t.Fatal("replacement opened before previous Close completed")
		default:
		}
		unblockClose()
		synctest.Wait()
		select {
		case <-opened:
			t.Fatal("replacement opened before previous workers drained")
		default:
		}
		unblockDrain()
		synctest.Wait()
		select {
		case <-m.ready:
			t.Fatal("failed startup released initial readiness waiters")
		case <-created:
			t.Fatal("observer created an endpoint before successful startup")
		default:
		}
		<-time.After(time.Second)
		<-opened
		endpoint := <-created
		if err := m.Close(); err != nil {
			t.Fatalf("recovered startup failure poisoned shutdown: %v", err)
		}
		select {
		case <-endpoint.closed:
		default:
			t.Fatal("shutdown returned before observer endpoint cleanup")
		}
		select {
		case <-second.drained:
		default:
			t.Fatal("shutdown returned before router workers drained")
		}
	})
}

func TestRouterRestartsAfterUnexpectedNodeExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newRecoveryNode(), newRecoveryNode()
		closeGate := make(chan struct{})
		unblockClose := sync.OnceFunc(func() { close(closeGate) })
		defer unblockClose()
		first.closeGate = closeGate
		opened := make(chan struct{}, 1)
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			opened <- struct{}{}
			return second.runtime(), nil
		})
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-m.ready
		endpoint, err := m.createRouterDestination(t.Context(), ivnp.DestinationSpec{})
		if err != nil {
			t.Fatal(err)
		}
		first.exitOnce.Do(func() { close(first.exited) })
		<-first.closing
		if err := endpoint.(ivnp.ReadyDestinationEndpoint).WaitReady(t.Context()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("dead router endpoint remained ready: %v", err)
		}
		if _, err := m.createRouterDestination(t.Context(), ivnp.DestinationSpec{}); !errors.Is(err, ErrNotStarted) {
			t.Fatalf("admitted a destination during router replacement: %v", err)
		}
		unblockClose()
		<-time.After(time.Second)
		<-opened
		synctest.Wait()
		endpoint, err = m.createRouterDestination(t.Context(), ivnp.DestinationSpec{})
		if err != nil {
			t.Fatalf("replacement did not admit destinations: %v", err)
		}
		if err := endpoint.(ivnp.ReadyDestinationEndpoint).WaitReady(t.Context()); err != nil {
			t.Fatalf("replacement endpoint is unavailable: %v", err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRouterCancellationStopsBackoffBeforeOpeningReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := newRecoveryNode()
		first.startErr = errors.New("startup failed")
		first.waitErr = first.startErr
		opened := make(chan struct{}, 1)
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			opened <- struct{}{}
			return newRecoveryNode().runtime(), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		<-first.drained
		synctest.Wait()
		cancel()
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		<-time.After(2 * time.Minute)
		select {
		case <-opened:
			t.Fatal("canceled retry opened a replacement router")
		default:
		}
	})
}

func TestRouterShutdownJoinsAccountsWaitingForInitialReadiness(t *testing.T) {
	account := leaseAccount(t, 0, "observer")
	synctest.Test(t, func(t *testing.T) {
		first := newRecoveryNode()
		first.startErr = errors.New("startup failed")
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			t.Error("shutdown opened a replacement router")
			return newRecoveryNode().runtime(), nil
		})
		stopping, releaseWorker := make(chan struct{}), make(chan struct{})
		unblockWorker := sync.OnceFunc(func() { close(releaseWorker) })
		defer unblockWorker()
		m.onEvent = func(event Event) {
			if event.State == "stopped" {
				close(stopping)
				<-releaseWorker
			}
		}
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.ConnectObserver(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		<-first.drained
		synctest.Wait()
		closed := make(chan error, 1)
		go func() { closed <- m.Close() }()
		<-stopping
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatalf("shutdown returned before account worker exited: %v", err)
		default:
		}
		unblockWorker()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	})
}

func TestRouterRecoveredFailurePreservesCleanupError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second, third := newRecoveryNode(), newRecoveryNode(), newRecoveryNode()
		first.startErr = errors.New("startup failed")
		first.waitErr = first.startErr
		cleanupFailure := errors.New("state close failed")
		first.closeErr = errors.Join(first.startErr, cleanupFailure)
		second.startErr = first.startErr
		second.waitErr = second.startErr
		second.closeErr = errors.New("later state close failed")
		attempt := 0
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			attempt++
			if attempt == 1 {
				return second.runtime(), nil
			}
			return third.runtime(), nil
		})
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-m.ready
		err := m.Close()
		if !errors.Is(err, cleanupFailure) {
			t.Fatalf("shutdown lost cleanup failure: %v", err)
		}
		if errors.Is(err, first.startErr) {
			t.Fatalf("shutdown retained recovered operational failure: %v", err)
		}
	})
}

func TestRouterReadinessWaitsForSuccessfulAddressBookStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		book, err := client.AddressBookNewService(client.AddressBookConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if err := book.Close(); err != nil {
			t.Fatal(err)
		}
		first, second := newRecoveryNode(), newRecoveryNode()
		initial := first.runtime()
		initial.addressBook = book
		m := recoveryManager(t, initial, func() (*routerRuntime, error) { return second.runtime(), nil })
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-first.drained
		synctest.Wait()
		select {
		case <-m.ready:
			t.Fatal("router readiness ignored addressbook startup failure")
		default:
		}
		<-time.After(5 * time.Second)
		<-second.started
		synctest.Wait()
		if _, err := m.createRouterDestination(t.Context(), ivnp.DestinationSpec{}); err != nil {
			t.Fatalf("addressbook failure prevented replacement startup: %v", err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
