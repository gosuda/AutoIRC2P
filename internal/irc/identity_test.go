package irc

import (
	"bytes"
	"errors"
	"testing"
)

func TestIdentityRestorePreservesSigningAndAddress(t *testing.T) {
	identity, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(identity.Keys)
	first, err := restoreIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer first.ReleaseSensitive()
	second, err := restoreIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer second.ReleaseSensitive()
	if first.B32() != identity.Address || second.B32() != identity.Address {
		t.Fatal("restoration changed persistent I2P address")
	}
	message := []byte("offline identity continuity")
	firstSignature, err := first.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	secondSignature, err := second.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstSignature, secondSignature) {
		t.Fatal("restoration changed the signing identity")
	}
}

func TestDifferentAccountsHaveDifferentDestinations(t *testing.T) {
	first, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Keys)
	second, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second.Keys)
	if first.Address == second.Address {
		t.Fatal("new accounts share an I2P destination")
	}
	first.Address = second.Address
	if _, err := restoreIdentity(first); !errors.Is(err, errIdentityMismatch) {
		t.Fatalf("mismatched persisted identity accepted: %v", err)
	}
}

func TestAccountReleaseSensitiveErasesEveryCallerKey(t *testing.T) {
	account := pooledLeaseAccount(t, 1, "alice")
	identities := make([]Identity, DestinationPoolSize)
	for i := range identities {
		identities[i] = account.identityAt(i)
	}
	account.ReleaseSensitive()
	if account.Password != "" {
		t.Error("released account retains its password")
	}
	for i, identity := range identities {
		for _, value := range identity.Keys {
			if value != 0 {
				t.Errorf("released account retains private bytes in destination slot %d", i)
				break
			}
		}
		if account.identityAt(i).Address != identity.Address {
			t.Errorf("releasing private bytes changed destination slot %d metadata", i)
		}
		if local, err := restoreIdentity(identity); err == nil {
			local.ReleaseSensitive()
			t.Errorf("released caller key in destination slot %d remains usable", i)
		}
	}
}
