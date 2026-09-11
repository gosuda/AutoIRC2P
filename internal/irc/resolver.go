package irc

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
	"gosuda.org/ivnp/foundation"
)

type addressBookResolver struct {
	addressBook *client.AddressBookService
}

func newAddressBookResolver(book *client.AddressBookService) ivnp.NameResolver {
	if book == nil {
		return nil
	}
	return &addressBookResolver{addressBook: book}
}

func (r *addressBookResolver) LookupDestination(ctx context.Context, name string) (ivnp.Hash, error) {
	if r == nil || r.addressBook == nil {
		return ivnp.Hash{}, ivnp.ErrNameResolutionUnavailable
	}
	destination, err := r.addressBook.ResolveDestination(ctx, name)
	if err != nil {
		return ivnp.Hash{}, err
	}
	if strings.HasSuffix(strings.ToLower(destination), ".b32.i2p") {
		addr, err := ivnp.ParseAddr(net.JoinHostPort(destination, "0"))
		if err != nil {
			return ivnp.Hash{}, err
		}
		return addr.Hash, nil
	}
	identity, err := foundation.ParseDestination([]byte(destination))
	if err != nil {
		return ivnp.Hash{}, fmt.Errorf("parse resolved IRC destination: %w", err)
	}
	return identity.Hash(), nil
}

func newAddressBook(stateDir string) (*client.AddressBookService, error) {
	if stateDir == "" {
		return nil, nil
	}
	return client.AddressBookNewService(client.AddressBookConfig{
		StatePath: filepath.Join(stateDir, "addressbook.json"),
		Subscriptions: []string{
			"https://raw.githubusercontent.com/i2p/i2p.i2p/master/installer/resources/hosts.txt",
		},
	})
}

func (m *Manager) dialIRC(ctx context.Context, endpoint destinationEndpoint) (net.Conn, error) {
	conn, err := endpoint.DialContext(ctx, "i2p", m.cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("dial IRC stream: %w", err)
	}
	return conn, nil
}
