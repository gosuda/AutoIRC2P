package irc

import (
	"fmt"

	"github.com/rs/zerolog/log"
	"gosuda.org/ivnp"
	"gosuda.org/ivnp/foundation"
)

type destinationSlot struct {
	local       *foundation.LocalDestination
	endpoint    ivnp.DestinationEndpoint
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
	m.status(state.account.ID, "connecting", "Creating I2P destination")
	endpoint, err := m.createDestination(state.ctx, ivnp.DestinationSpec{Local: slot.local})
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
