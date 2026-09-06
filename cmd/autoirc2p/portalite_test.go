package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gosuda.org/portalite"
)

func TestPortaliteIdentitySurvivesRestartAndNameChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "portalite-identity.json")
	first, err := loadPortaliteIdentity(path, "autoirc2p")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadPortaliteIdentity(path, "different-name")
	if err != nil {
		t.Fatal(err)
	}
	if second.Address() != first.Address() || second.Name() != "autoirc2p" {
		t.Fatal("restart replaced the persisted identity")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("restart rewrote the identity file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("identity permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestPortaliteIdentityRejectsExistingUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"corrupt", "public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "portalite-identity.json")
			identity, err := portalite.GenerateIdentity("existing")
			if err != nil {
				t.Fatal(err)
			}
			data, err := identity.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt" {
				data = []byte("{broken")
			}
			target := path
			if kind == "symlink" {
				target = filepath.Join(dir, "target.json")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(target, data, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "public" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadPortaliteIdentity(path, "replacement"); err == nil {
				t.Fatal("unsafe identity accepted")
			}
			after, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, after) {
				t.Fatal("existing identity was overwritten")
			}
		})
	}
}

func TestConcurrentPortaliteIdentityCreationSelectsOneKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portalite-identity.json")
	const writers = 8
	addresses := make([]string, writers)
	errs := make([]error, writers)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range writers {
		workers.Go(func() {
			<-start
			identity, err := loadPortaliteIdentity(path, "")
			addresses[i], errs[i] = identity.Address(), err
		})
	}
	close(start)
	workers.Wait()
	for i := range writers {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if addresses[i] != addresses[0] {
			t.Fatalf("writers selected different identities: %q and %q", addresses[0], addresses[i])
		}
	}
}

func TestIndependentPortaliteInstallsUseDifferentNames(t *testing.T) {
	first, err := loadPortaliteIdentity(filepath.Join(t.TempDir(), "identity.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadPortaliteIdentity(filepath.Join(t.TempDir(), "identity.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name() == second.Name() {
		t.Fatal("independent installs would compete for the same relay hostname")
	}
}

func TestPortaliteOriginsFollowCurrentRelayAssignments(t *testing.T) {
	relays := []portalite.RelayStatus{
		{RelayURL: "https://relay-a.example", PublicURL: "https://autoirc2p.relay-a.example", State: portalite.RelayReady},
		{RelayURL: "https://relay-b.example", PublicURL: "https://autoirc2p.relay-b.example:8443", State: portalite.RelayReady},
		{RelayURL: "https://relay-c.example", PublicURL: "https://autoirc2p.relay-c.example", State: portalite.RelayFailed},
	}
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"https://autoirc2p.relay-a.example", true},
		{"https://autoirc2p.relay-b.example:8443", true},
		{"https://autoirc2p.relay-b.example", false},
		{"https://other-tenant.relay-a.example", false},
		{"https://autoirc2p.relay-a.example.attacker.test", false},
		{"https://autoirc2p.relay-c.example", false},
		{"http://autoirc2p.relay-a.example", false},
		{"null", false},
		{"", false},
	} {
		if got := portaliteOriginAllowed(relays, tc.origin); got != tc.want {
			t.Errorf("origin %q allowed=%t, want %t", tc.origin, got, tc.want)
		}
	}
	relays[1].PublicURL = "https://autoirc2p.relay-b.example:9443"
	if portaliteOriginAllowed(relays, "https://autoirc2p.relay-b.example:8443") || !portaliteOriginAllowed(relays, "https://autoirc2p.relay-b.example:9443") {
		t.Fatal("origin policy did not follow the refreshed relay port")
	}
}
