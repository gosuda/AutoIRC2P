package irc

// MembershipState describes a room on one account's current connection.
type MembershipState string

const (
	RoomPreparing   MembershipState = "preparing"
	RoomReady       MembershipState = "ready"
	RoomUnavailable MembershipState = "unavailable"
)

func (m *Manager) RoomState(accountID int64, room string) MembershipState {
	canonical, ok := m.rooms[fold(room)]
	if !ok {
		return RoomUnavailable
	}
	m.mu.Lock()
	state := m.accounts[accountID]
	closed, started, lifetime := m.closed, m.started, m.ctx
	m.mu.Unlock()
	if closed {
		return RoomUnavailable
	}
	if lifetime != nil && lifetime.Err() != nil {
		return RoomUnavailable
	}
	if !started || state == nil {
		return RoomPreparing
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.connection == nil {
		return RoomPreparing
	}
	if state.joined[fold(canonical)] {
		return RoomReady
	}
	if state.unavailable[fold(canonical)] {
		return RoomUnavailable
	}
	return RoomPreparing
}

func (m *Manager) setRoomState(account *accountConnection, room string, state MembershipState) {
	canonical, ok := m.rooms[fold(room)]
	if !ok {
		return
	}
	key := fold(canonical)
	account.mu.Lock()
	if state == RoomReady {
		if account.joined == nil {
			account.joined = make(map[string]bool)
		}
		account.joined[key] = true
		delete(account.unavailable, key)
	} else {
		delete(account.joined, key)
		if account.unavailable == nil {
			account.unavailable = make(map[string]bool)
		}
		account.unavailable[key] = true
	}
	account.mu.Unlock()
	m.onEvent(Event{Kind: "membership", AccountID: account.account.ID, Room: canonical, State: string(state)})
}
