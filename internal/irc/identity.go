package irc

import (
	"errors"
	"fmt"

	"gosuda.org/ivnp/foundation"
)

var errIdentityMismatch = errors.New("I2P identity address does not match its private keys")

// Identity contains private IVNP state. Its owner must encrypt Keys at rest.
type Identity struct {
	Keys    []byte
	Address string
}

// GenerateIdentity performs key generation only; it opens no router or connection.
func GenerateIdentity() (Identity, error) {
	// IVNP Streaming currently requires an ElGamal identity with an LS2 X25519 key.
	local, err := foundation.GenerateLegacyLocalDestination()
	if err != nil {
		return Identity{}, fmt.Errorf("generate I2P destination: %w", err)
	}
	defer local.ReleaseSensitive()
	keys := make([]byte, local.PrivateEncodedLen())
	n, err := local.MarshalPrivateTo(keys)
	if err != nil {
		clear(keys)
		return Identity{}, fmt.Errorf("serialize I2P destination: %w", err)
	}
	return Identity{Keys: keys[:n], Address: local.B32()}, nil
}

func restoreIdentity(identity Identity) (*foundation.LocalDestination, error) {
	local, err := foundation.ImportLocalDestination(identity.Keys)
	if err != nil {
		return nil, fmt.Errorf("restore I2P destination: %w", err)
	}
	if local.B32() != identity.Address {
		local.ReleaseSensitive()
		return nil, errIdentityMismatch
	}
	return local, nil
}
