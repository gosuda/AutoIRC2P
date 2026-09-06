package irc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
)

var errAddressBookDisabled = errors.New("IRC hostname resolution requires an enabled I2P addressbook")

type i2pDialer interface {
	DialI2P(context.Context, string) (net.Conn, error)
}

func newAddressBook(cfg ivnp.Config) (*client.AddressBookService, error) {
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
		if m.addressBook == nil {
			return nil, errAddressBookDisabled
		}
		host, err = m.addressBook.ResolveDestination(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve IRC hostname: %w", err)
		}
	}
	// Endpoint dialing preserves the account identity; the node's default dialer does not.
	conn, err := endpoint.DialI2P(ctx, net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("dial IRC stream: %w", err)
	}
	return conn, nil
}
