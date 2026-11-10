package irc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gosuda.org/ivnp"
	"gosuda.org/ivnp/foundation"
)

type destinationSlot struct {
	local       *foundation.LocalDestination
	endpoint    destinationEndpoint
	ready       bool
	creationErr error
}

func restoreDestinations(account Account, first int) ([]destinationSlot, error) {
	slots := make([]destinationSlot, len(account.Alternates)+1)
	for i := first; i < len(slots); i++ {
		local, err := restoreIdentity(account.identityAt(i))
		if err != nil {
			for j := first; j < i; j++ {
				slots[j].local.ReleaseSensitive()
			}
			return nil, fmt.Errorf("restore I2P destination slot %d: %w", i, err)
		}
		slots[i].local = local
	}
	return slots, nil
}

func (m *Manager) createAccountDestination(state *accountConnection, slot *destinationSlot) error {
	ctx, cancel := context.WithTimeout(state.ctx, 5*time.Minute)
	defer cancel()
	endpoint, err := m.createDestination(ctx, ivnp.DestinationConfig{Identity: slot.local})
	slot.endpoint = endpoint
	if err != nil {
		m.closeAccountDestination(state, slot)
	}
	return err
}

func (m *Manager) closeAccountDestination(state *accountConnection, slot *destinationSlot) {
	if slot.endpoint != nil {
		if err := slot.endpoint.Close(); err != nil {
			event := log.Warn().Int64("account_id", state.account.ID)
			event.Err(err).Msg("I2P endpoint cleanup failed")
		}
		slot.endpoint = nil
	}
	slot.ready = false
}

func (m *Manager) rotateObserverDestination(state *accountConnection, slot *destinationSlot) {
	m.closeAccountDestination(state, slot)
	local, err := foundation.GenerateLegacyLocalDestination()
	if err != nil {
		return
	}
	if slot.local != nil {
		slot.local.ReleaseSensitive()
	}
	slot.local = local
}

func isObserverDestinationBanned(stage string, err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if stage == "IRC dial" {
		return strings.Contains(text, "peer reset the connection")
	}
	if stage == "IRC session" {
		return strings.Contains(text, "permanently banned") ||
			strings.Contains(text, "k-lined") ||
			strings.Contains(text, "g-lined") ||
			strings.Contains(text, "z-lined")
	}
	return false
}
