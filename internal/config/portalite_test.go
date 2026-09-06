package config

import (
	"testing"

	"gosuda.org/portalite"
)

func TestPortaliteUsesRelayOriginsWithoutPublicHost(t *testing.T) {
	productionTestEnv(t)
	t.Setenv("PORTALITE", "1")
	t.Setenv("APP_ORIGIN", "")
	t.Setenv("PUBLIC_HOST", "")
	t.Setenv("LISTEN_ADDR", "0.0.0.0:8080")
	t.Setenv("PORTALITE_RELAYS", "https://relay-a.example:443, https://relay-b.example:8443,https://relay-a.example/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Portalite || !cfg.SecureCookies || cfg.Origin != "" {
		t.Fatalf("Portalite must use secure cookies and relay origins: enabled=%t secure=%t origin=%q", cfg.Portalite, cfg.SecureCookies, cfg.Origin)
	}
	if len(cfg.PortaliteRelays) != 2 || cfg.PortaliteRelays[0] != "https://relay-a.example" || cfg.PortaliteRelays[1] != "https://relay-b.example:8443" {
		t.Fatalf("relay selection = %v", cfg.PortaliteRelays)
	}
	// A native deployment's stale APP_ORIGIN must not become another trusted origin.
	t.Setenv("APP_ORIGIN", "https://old.example")
	cfg, err = Load()
	if err != nil || cfg.Origin != "" {
		t.Fatalf("native origin leaked into Portalite: origin=%q error=%v", cfg.Origin, err)
	}
}

func TestPortaliteRejectsInvalidEnvironment(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"PORTALITE", "true"},
		{"PORTALITE_NAME", "bad.name"},
		{"PORTALITE_NAME", "-bad"},
		{"PORTALITE_RELAYS", "http://relay.example"},
		{"PORTALITE_RELAYS", "https://relay.example,"},
		{"PORTALITE_RELAYS", "https://user:password@relay.example"},
		{"PORTALITE_RELAYS", "https://relay.example?token=secret"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			productionTestEnv(t)
			t.Setenv("PORTALITE", "1")
			t.Setenv(tc.name, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid Portalite configuration accepted")
			}
		})
	}
}

func TestPortaliteAcceptsMixedCaseIdentityName(t *testing.T) {
	productionTestEnv(t)
	t.Setenv("PORTALITE", "1")
	t.Setenv("PORTALITE_NAME", "My-CHAT")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := portalite.GenerateIdentity(cfg.PortaliteName)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Name() != "my-chat" {
		t.Fatalf("relay DNS name = %q, want my-chat", identity.Name())
	}
}
