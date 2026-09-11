package irc

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
	"gosuda.org/ivnp"
)

const prewarmCapacity = 2

var errPrewarmCapacity = errors.New("primary destination warmup capacity reached")

type warmDestinationKey struct {
	accountID int64
	address   string
}

type warmDestination struct {
	key         warmDestinationKey
	ctx         context.Context
	cancel      context.CancelFunc
	stopCaller  func() bool
	routerReady <-chan struct{}
	claim       chan struct{}
	done        chan struct{}
	// Manager.mu guards ownership; only the warmup worker touches slot until done.
	claimed bool
	closing bool
	slot    destinationSlot
}

// Prewarm retains a private copy of the primary identity until Acquire or ctx
// cancellation. It queues tunnel readiness, not IRC registration; asynchronous
// failures are logged and leave the next Acquire on the ordinary startup path.
func (m *Manager) Prewarm(ctx context.Context, account Account) error {
	if account.ID <= 0 || account.Identity.Address == "" {
		return errInvalidAccount
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.admissionErrorLocked(ctx); err != nil {
		return err
	}
	for _, state := range m.accounts {
		if state.account.ID == account.ID {
			if state.account.Identity.Address != account.Identity.Address {
				return errInvalidAccount
			}
			return nil
		}
		for i := range len(state.account.Alternates) + 1 {
			if state.account.identityAt(i).Address == account.Identity.Address {
				return errInvalidAccount
			}
		}
	}
	key := warmDestinationKey{accountID: account.ID, address: account.Identity.Address}
	for existing, warm := range m.warm {
		if existing == key && !warm.closing && warm.ctx.Err() == nil {
			return nil
		}
		if existing.accountID == account.ID {
			warm.cancel()
		}
		if existing.address == key.address && existing.accountID != key.accountID {
			return errInvalidAccount
		}
	}
	if len(m.warm) >= prewarmCapacity || m.warm[key] != nil {
		return errPrewarmCapacity
	}
	if m.destinationCapacity > 0 && m.reservedDestinationsLocked() >= m.destinationCapacity {
		return errPrewarmCapacity
	}
	local, err := restoreIdentity(account.Identity)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(m.ctx)
	warm := &warmDestination{
		key: key, ctx: lifetime, cancel: cancel, routerReady: m.ready,
		claim: make(chan struct{}), done: make(chan struct{}), slot: destinationSlot{local: local},
	}
	warm.stopCaller = context.AfterFunc(ctx, cancel)
	if m.warm == nil {
		m.warm = make(map[warmDestinationKey]*warmDestination, prewarmCapacity)
	}
	m.warm[key] = warm
	m.warmWG.Go(func() { m.runPrewarm(warm) })
	return nil
}

func (m *Manager) runPrewarm(warm *warmDestination) {
	var err error
	defer func() { m.finishPrewarm(warm, err) }()
	select {
	case <-warm.ctx.Done():
		err = warm.ctx.Err()
		return
	case <-warm.routerReady:
	}
	readyCtx, cancel := context.WithTimeout(warm.ctx, 5*time.Minute)
	warm.slot.endpoint, err = m.createDestination(readyCtx, ivnp.DestinationConfig{Identity: warm.slot.local})
	if err == nil {
		err = warm.slot.endpoint.WaitReady(readyCtx)
	}
	cancel()
	if err != nil {
		return
	}
	warm.slot.ready = true
	select {
	case <-warm.claim:
	case <-warm.ctx.Done():
		err = warm.ctx.Err()
	}
}

func (m *Manager) finishPrewarm(warm *warmDestination, err error) {
	m.mu.Lock()
	warm.closing = true
	claimed := warm.claimed
	m.mu.Unlock()
	if err != nil && warm.ctx.Err() == nil {
		event := log.Warn().Int64("account_id", warm.key.accountID)
		event.Err(err).Msg("Primary I2P destination warmup failed; login will create a destination")
	}
	if err != nil || !claimed {
		if warm.slot.endpoint != nil {
			if closeErr := warm.slot.endpoint.Close(); closeErr != nil {
				event := log.Warn().Int64("account_id", warm.key.accountID)
				event.Err(closeErr).Msg("Prewarmed I2P endpoint cleanup failed")
			}
			warm.slot.endpoint = nil
		}
		warm.slot.ready = false
	}
	if !claimed {
		warm.slot.local.ReleaseSensitive()
	}
	warm.stopCaller()
	warm.cancel()
	m.mu.Lock()
	if m.warm[warm.key] == warm {
		delete(m.warm, warm.key)
	}
	m.mu.Unlock()
	close(warm.done)
}

func (m *Manager) matchWarmDestinationLocked(account Account) (*warmDestination, <-chan struct{}, error) {
	var match *warmDestination
	for key, warm := range m.warm {
		if key.accountID == account.ID {
			if key.address != account.Identity.Address {
				warm.cancel()
				return nil, warm.done, nil
			}
			if warm.closing || warm.ctx.Err() != nil {
				return nil, warm.done, nil
			}
			match = warm
			continue
		}
		for i := range len(account.Alternates) + 1 {
			if account.identityAt(i).Address == key.address {
				return nil, nil, errInvalidAccount
			}
		}
	}
	reserved := m.reservedDestinationsLocked() + len(account.Alternates) + 1
	if match != nil {
		reserved--
	}
	if m.destinationCapacity > 0 && reserved > m.destinationCapacity {
		for _, warm := range m.warm {
			if warm != match {
				warm.cancel()
				return nil, warm.done, nil
			}
		}
	}
	return match, nil, nil
}

func (m *Manager) claimWarmDestinationLocked(state *accountConnection, warm *warmDestination) {
	warm.claimed = true
	warm.stopCaller()
	delete(m.warm, warm.key)
	state.warm = warm
	close(warm.claim)
}

func (m *Manager) cancelWarmDestinationsLocked() []*warmDestination {
	warmups := make([]*warmDestination, 0, len(m.warm))
	for _, warm := range m.warm {
		warm.cancel()
		warmups = append(warmups, warm)
	}
	return warmups
}

func (m *Manager) reservedDestinationsLocked() int {
	reserved := len(m.warm)
	for _, state := range m.accounts {
		reserved += len(state.destinations)
	}
	return reserved
}
