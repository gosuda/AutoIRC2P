package irc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"gosuda.org/ivnp/client"
	"gosuda.org/ivnp/foundation"
	"gosuda.org/ivnp/state"
)

var errAddressBookDisabled = errors.New("IRC hostname resolution requires an enabled I2P addressbook")

type i2pDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func newAddressBook(cfg state.ConfigurationOperating) (*client.AddressBookService, error) {
	book := cfg.AddressBook
	if !book.Enabled {
		return nil, nil
	}
	return client.AddressBookNewService(client.AddressBookConfig{
		PrivateHostsPath: book.PrivateHostsPath, UserHostsPath: book.UserHostsPath, HostsPath: book.HostsPath, StatePath: book.StatePath,
		Subscriptions: book.Subscriptions, RefreshInterval: book.RefreshInterval, RetryInterval: book.RetryInterval, RequestTimeout: book.RequestTimeout,
		MaxEntries: book.MaxEntries, MaxFileBytes: book.MaxFileBytes, MaxResponseBytes: book.MaxResponseBytes, MaxRedirects: book.MaxRedirects,
	})
}

func (m *Manager) dialIRC(ctx context.Context, endpoint i2pDialer) (net.Conn, error) {
	host, port, err := net.SplitHostPort(m.cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("parse IRC server: %w", err)
	}
	if !strings.HasSuffix(strings.ToLower(host), ".b32.i2p") {
		m.mu.Lock()
		router := m.router
		m.mu.Unlock()
		if router == nil {
			return nil, ErrNotStarted
		}
		if router.addressBook == nil {
			return nil, errAddressBookDisabled
		}
		host, err = router.addressBook.ResolveDestination(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve IRC hostname: %w", err)
		}
	}
	// Resolve to B32 for the root network API while retaining this account's identity.
	if !strings.HasSuffix(strings.ToLower(host), ".b32.i2p") {
		identity, err := foundation.ParseDestination([]byte(host))
		if err != nil {
			return nil, fmt.Errorf("parse resolved IRC destination: %w", err)
		}
		host = foundation.B32(identity.Hash())
	}
	conn, err := endpoint.DialContext(ctx, "i2p", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("dial IRC stream: %w", err)
	}
	return conn, nil
}
