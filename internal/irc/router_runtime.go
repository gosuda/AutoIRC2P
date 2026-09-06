package irc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
)

var errRouterStopped = errors.New("embedded I2P router stopped unexpectedly")

type routerNode interface {
	Start(context.Context) error
	Close() error
	Wait() error
}

type routerRuntime struct {
	node              routerNode
	addressBook       *client.AddressBookService
	createDestination func(context.Context, ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error)
}

func openRouterRuntime(configuration ivnp.Config) (*routerRuntime, error) {
	addressBook, err := newAddressBook(configuration)
	if err != nil {
		return nil, fmt.Errorf("open I2P addressbook: %w", err)
	}
	// Account endpoints share this resolver, not the node's subscription writer.
	configuration.AddressBook.Enabled = false
	configuration.SAM.Enabled = false
	configuration.HTTPProxy.Enabled = false
	configuration.SOCKS5.Enabled = false
	configuration.Control.Enabled = false
	configuration.Metrics.Enabled = false
	node, err := ivnp.New(configuration, ivnp.Options{})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open embedded IVNP router: %w", err), addressBook.Close(), addressBook.Wait())
	}
	return &routerRuntime{node: node, addressBook: addressBook, createDestination: node.DestinationController().CreateDestination}, nil
}

func (r *routerRuntime) start(ctx context.Context) error {
	if err := r.node.Start(ctx); err != nil {
		return fmt.Errorf("start embedded I2P router: %w", err)
	}
	if r.addressBook == nil {
		return nil
	}
	if err := r.addressBook.Start(ctx); err != nil {
		return fmt.Errorf("start I2P addressbook: %w", err)
	}
	return nil
}

func (r *routerRuntime) closeResources() error {
	return errors.Join(r.addressBook.Close(), r.node.Close())
}

func (r *routerRuntime) close() error {
	if r == nil {
		return nil
	}
	return errors.Join(r.closeResources(), r.addressBook.Wait(), r.node.Wait())
}

// IVNP repeats the same operational error in the joined Close/Wait results.
func routerCleanupError(err, recovered error) error {
	if err == nil || errors.Is(recovered, err) {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	causes := joined.Unwrap()
	remaining := make([]error, 0, len(causes))
	for _, cause := range causes {
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
	var nodeDone chan struct{}
	var nodeWaitErr error
	var runtimeFailure error
	closeRuntime := func(recovered error) error {
		if runtime == nil {
			return nil
		}
		m.mu.Lock()
		m.routerReady = false
		m.router = nil
		m.mu.Unlock()
		cleanupErr := errors.Join(runtime.closeResources(), runtime.addressBook.Wait())
		if nodeDone != nil {
			<-nodeDone
		} else {
			nodeWaitErr = runtime.node.Wait()
		}
		cleanupErr = errors.Join(cleanupErr, nodeWaitErr)
		if recovered != nil {
			cleanupErr = routerCleanupError(cleanupErr, recovered)
		}
		m.mu.Lock()
		if m.closeErr == nil {
			m.closeErr = cleanupErr
		}
		m.mu.Unlock()
		runtime, nodeDone, nodeWaitErr = nil, nil, nil
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

	delay := 5 * time.Second
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
		if err == nil {
			nodeDone = make(chan struct{})
			go func() {
				defer close(nodeDone)
				nodeWaitErr = runtime.node.Wait()
			}()
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
			slog.Info("Embedded I2P router started")
			m.status(0, "connecting", "I2P router started; building anonymous tunnels")
			delay = 5 * time.Second
			select {
			case <-m.ctx.Done():
				return
			case <-nodeDone:
				err = errors.Join(errRouterStopped, nodeWaitErr)
			}
		}
		runtimeFailure = err
		if m.ctx.Err() != nil {
			return
		}
		err = errors.Join(err, closeRuntime(err))
		slog.Warn("Embedded I2P router unavailable", "error", err, "retry_delay", delay)
		m.status(0, "connecting", "Embedded I2P router unavailable; retrying in "+delay.String())
		if !pause(m.ctx, delay) {
			return
		}
		delay = min(delay*2, 2*time.Minute)
	}
}

func (m *Manager) createRouterDestination(ctx context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
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
	// The SDK serializes creation against Close and retires admitted endpoints.
	return runtime.createDestination(ctx, spec)
}
