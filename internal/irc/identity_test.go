package irc

import (
	"bytes"
	"context"
	"errors"
	"gosuda.org/ivnp"
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

func TestGeneratedIdentityOpensStreamingDestination(t *testing.T) {
	identity, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(identity.Keys)
	local, err := restoreIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer local.ReleaseSensitive()
	cfg, err := ivnp.ParseConfig("[paths]\ndata_dir = "+t.TempDir()+"\n", "test.conf")
	if err != nil {
		t.Fatal(err)
	}
	node, err := ivnp.New(cfg, ivnp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := node.Close(); err != nil {
			t.Error(err)
		}
	}()
	endpoint, err := node.DestinationController().CreateDestination(context.Background(), ivnp.DestinationSpec{Local: local})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	}()
	if endpoint.B32() != identity.Address {
		t.Fatal("streaming endpoint changed identity")
	}
}
