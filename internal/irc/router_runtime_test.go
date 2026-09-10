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
	readinessErr error
	closeErr     error
	exited       chan struct{}
	closing      chan struct{}
	drained      chan struct{}
	closeGate    <-chan struct{}
	closeOnce    sync.Once
}

func newRecoveryNode() *recoveryNode {
	return &recoveryNode{exited: make(chan struct{}), closing: make(chan struct{}), drained: make(chan struct{})}
}

func (n *recoveryNode) WaitReady(ctx context.Context) error {
	select {
	case <-n.exited:
		return net.ErrClosed
	case <-n.closing:
		return net.ErrClosed
	default:
		return errors.Join(ctx.Err(), n.readinessErr)
	}
}

func (n *recoveryNode) Close() error {
	n.closeOnce.Do(func() {
		close(n.closing)
		if n.closeGate != nil {
			<-n.closeGate
		}
		close(n.drained)
	})
	return n.closeErr
}

func (n *recoveryNode) runtime() *routerRuntime {
	return &routerRuntime{node: n, createDestination: func(ctx context.Context, _ ivnp.DestinationConfig) (destinationEndpoint, error) {
		if err := n.WaitReady(ctx); err != nil {
			return nil, err
		}
		return &routerRecoveryEndpoint{node: n}, nil
	}}
}

type routerRecoveryEndpoint struct {
	destinationEndpoint
	node *recoveryNode
}

func (e *routerRecoveryEndpoint) WaitReady(ctx context.Context) error { return e.node.WaitReady(ctx) }
func (e *routerRecoveryEndpoint) Close() error                        { return nil }

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

func TestRouterReadinessFailureDrainsBeforeReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newRecoveryNode(), newRecoveryNode()
		first.readinessErr = errors.New("router failed before readiness")
		first.closeErr = first.readinessErr
		gate := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(gate) })
		defer unblock()
		first.closeGate = gate
		opened := make(chan struct{}, 1)
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			opened <- struct{}{}
			return second.runtime(), nil
		})
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-first.closing
		synctest.Wait()
		select {
		case <-opened:
			t.Fatal("replacement opened before Close joined the old router")
		case <-m.ready:
			t.Fatal("failed readiness admitted destinations")
		default:
		}
		unblock()
		<-first.drained
		<-time.After(time.Second)
		<-opened
		<-m.ready
		if err := m.Close(); err != nil {
			t.Fatalf("recovered failure poisoned shutdown: %v", err)
		}
		select {
		case <-second.drained:
		default:
			t.Fatal("shutdown returned before router Close finished")
		}
	})
}

func TestRouterRestartsAfterTerminalReadinessError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newRecoveryNode(), newRecoveryNode()
		gate := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(gate) })
		defer unblock()
		first.closeGate = gate
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) { return second.runtime(), nil })
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-m.ready
		endpoint, err := m.createRouterDestination(t.Context(), ivnp.DestinationConfig{})
		if err != nil {
			t.Fatal(err)
		}
		close(first.exited)
		<-first.closing
		if err := endpoint.(destinationReadiness).WaitReady(t.Context()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("closed router destination remained ready: %v", err)
		}
		if _, err := m.createRouterDestination(t.Context(), ivnp.DestinationConfig{}); !errors.Is(err, ErrNotStarted) {
			t.Fatalf("replacement gap admitted a destination: %v", err)
		}
		unblock()
		<-time.After(time.Second)
		m.mu.Lock()
		ready := m.ready
		m.mu.Unlock()
		<-ready
		if _, err := m.createRouterDestination(t.Context(), ivnp.DestinationConfig{}); err != nil {
			t.Fatalf("replacement unavailable: %v", err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRouterCancellationStopsReplacementBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := newRecoveryNode()
		first.readinessErr = errors.New("readiness failed")
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) {
			t.Error("canceled retry opened a router")
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
	})
}

func TestRouterRecoveredFailurePreservesCleanupError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newRecoveryNode(), newRecoveryNode()
		first.readinessErr = errors.New("readiness failed")
		cleanupFailure := errors.New("state close failed")
		first.closeErr = errors.Join(first.readinessErr, cleanupFailure)
		m := recoveryManager(t, first.runtime(), func() (*routerRuntime, error) { return second.runtime(), nil })
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-m.ready
		err := m.Close()
		if !errors.Is(err, cleanupFailure) || errors.Is(err, first.readinessErr) {
			t.Fatalf("shutdown did not preserve only the cleanup failure: %v", err)
		}
	})
}

func TestRouterReadinessWaitsForAddressBookStartup(t *testing.T) {
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
			t.Fatal("addressbook startup failure admitted destinations")
		default:
		}
		<-time.After(time.Second)
		<-m.ready
		if _, err := m.createRouterDestination(t.Context(), ivnp.DestinationConfig{}); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
