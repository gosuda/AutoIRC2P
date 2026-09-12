package irc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/rs/zerolog/log"
	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
)

type destinationEndpoint interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	WaitReady(context.Context) error
	Close() error
}

type routerNode interface {
	WaitReady(context.Context) error
	Close() error
}

type routerRuntime struct {
	node              routerNode
	open              func(context.Context) (routerNode, error)
	addressBook       *client.AddressBookService
	createDestination func(context.Context, ivnp.DestinationConfig) (destinationEndpoint, error)
}

func openRouterRuntime(stateDir string, destinationCapacity int) (*routerRuntime, error) {
	cfg := ivnp.DefaultRouterConfig()
	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			return nil, fmt.Errorf("create I2P state directory: %w", err)
		}
		cfg.Persistence = ivnp.DefaultPersistenceConfig(stateDir)
	}
	cfg.Limits.MaxDestinations = destinationCapacity
	cfg.Exploratory.Inbound.Hops = defaultRouterHops
	cfg.Exploratory.Outbound.Hops = defaultRouterHops

	tunnels := ivnp.DefaultDestinationConfig().Tunnels
	tunnels.Inbound.Hops = defaultRouterHops
	tunnels.Outbound.Hops = defaultRouterHops

	var addressBook *client.AddressBookService
	if stateDir != "" {
		var err error
		addressBook, err = newAddressBook(stateDir)
		if err != nil {
			return nil, fmt.Errorf("open I2P addressbook: %w", err)
		}
	}
	if addressBook != nil {
		cfg.Resolver = newAddressBookResolver(addressBook)
	}
	runtime := &routerRuntime{addressBook: addressBook}
	runtime.open = func(ctx context.Context) (routerNode, error) {
		router, err := ivnp.NewRouter(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("open embedded I2P router: %w", err)
		}
		runtime.createDestination = func(ctx context.Context, supplied ivnp.DestinationConfig) (destinationEndpoint, error) {
			destination := ivnp.DefaultDestinationConfig()
			destination.Identity = supplied.Identity
			destination.Tunnels = tunnels
			endpoint, err := router.NewDestination(ctx, destination)
			if err != nil {
				return nil, err
			}
			return endpoint, nil
		}
		return router, nil
	}
	return runtime, nil
}

func (r *routerRuntime) start(ctx context.Context) error {
	if r.node == nil {
		node, err := r.open(ctx)
		if err != nil {
			return err
		}
		r.node = node
	}
	if r.addressBook != nil {
		if err := r.addressBook.Start(ctx); err != nil {
			return fmt.Errorf("start I2P addressbook: %w", err)
		}
	}
	return r.node.WaitReady(ctx)
}

func (r *routerRuntime) close() error {
	if r == nil {
		return nil
	}
	var nodeErr error
	if r.node != nil {
		nodeErr = r.node.Close()
	}
	return errors.Join(r.addressBook.Close(), r.addressBook.Wait(), nodeErr)
}

// IVNP can return the recovered operational error again while closing resources.
func routerCleanupError(err, recovered error) error {
	if err == nil || errors.Is(recovered, err) {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var remaining []error
	for _, cause := range joined.Unwrap() {
		if cleanupErr := routerCleanupError(cause, recovered); cleanupErr != nil {
			remaining = append(remaining, cleanupErr)
		}
	}
	return errors.Join(remaining...)
}

func (m *Manager) runRouter() {
	m.mu.Lock()
	runtime := m.router
	m.mu.Unlock()
	var runtimeFailure error
	closeRuntime := func(recovered error) error {
		if runtime == nil {
			return nil
		}
		m.mu.Lock()
		var warmups []*warmDestination
		if m.routerReady {
			m.ready = make(chan struct{})
			warmups = m.cancelWarmDestinationsLocked()
		}
		m.routerReady = false
		m.router = nil
		m.mu.Unlock()
		for _, warm := range warmups {
			<-warm.done
		}
		cleanupErr := routerCleanupError(runtime.close(), recovered)
		m.mu.Lock()
		if m.closeErr == nil {
			m.closeErr = cleanupErr
		}
		m.mu.Unlock()
		runtime = nil
		runtimeFailure = nil
		return cleanupErr
	}
	defer func() {
		m.mu.Lock()
		m.routerReady = false
		for _, state := range m.accounts {
			m.stopAccountLocked(state)
		}
		m.mu.Unlock()
		m.accountWG.Wait()
		closeRuntime(runtimeFailure)
	}()

	for m.ctx.Err() == nil {
		var err error
		if runtime == nil {
			runtime, err = m.newRouter()
			if err == nil {
				m.mu.Lock()
				m.router = runtime
				m.mu.Unlock()
			}
		}
		if m.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = runtime.start(m.ctx)
		}
		if err == nil && m.ctx.Err() == nil {
			m.mu.Lock()
			if m.closed || m.ctx.Err() != nil {
				m.mu.Unlock()
				return
			}
			m.routerReady = true
			select {
			case <-m.ready:
			default:
				close(m.ready)
			}
			m.mu.Unlock()
			m.status(0, "connecting", "I2P router ready; building account tunnels")
			// The root API has no Wait method. Readiness reports terminal router closure;
			// loss of tunnels waits for recovery rather than replacing a live router.
			for pause(m.ctx, time.Second) {
				if err = runtime.node.WaitReady(m.ctx); err != nil {
					break
				}
			}
		}
		runtimeFailure = err
		if m.ctx.Err() != nil {
			return
		}
		err = errors.Join(err, closeRuntime(err))
		event := log.Warn().Err(err)
		event.Dur("retry_in_ms", reconnectDelay).Msg("Embedded I2P router unavailable")
		m.onEvent(Event{AccountID: 0, State: "connecting", Text: fmt.Sprintf("Embedded I2P router unavailable: %v; retrying in %s", err, reconnectDelay), Service: true, Kind: "status"})
		if !pause(m.ctx, reconnectDelay) {
			return
		}
	}
}

func (m *Manager) createRouterDestination(ctx context.Context, spec ivnp.DestinationConfig) (destinationEndpoint, error) {
	m.mu.Lock()
	if err := m.admissionErrorLocked(ctx); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	runtime := m.router
	ready := m.routerReady
	m.mu.Unlock()
	if !ready || runtime == nil {
		return nil, ErrNotStarted
	}
	return runtime.createDestination(ctx, spec)
}
