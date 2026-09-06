package config

import (
	"testing"
)

func productionTestEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"IRC_OBSERVER_NICK", "IRC_OBSERVER_PASSWORD", "TRUSTED_PROXY_CIDRS", "WS_MAX_CONNECTIONS", "WS_MAX_CONNECTIONS_PER_IP", "WS_MAX_CONNECTIONS_PER_ACCOUNT",
		"WS_HANDSHAKES_PER_MINUTE", "WS_READS_PER_MINUTE", "SEND_REQUESTS_PER_MINUTE", "SEND_REQUESTS_PER_IP_MINUTE", "SEND_MAX_PENDING", "IRC_MAX_ACCOUNTS",
		"HTTP_BODY_TIMEOUT", "HTTP_WRITE_TIMEOUT", "IRC_ACCOUNT_IDLE_GRACE", "CHAT_RETENTION", "TRANSLATION_RETENTION", "SEND_PAYLOAD_RETENTION", "RETENTION_INTERVAL", "RETENTION_BATCH_SIZE",
		"TRANSLATION_INTERVAL", "TRANSLATION_COOLDOWN", "IRC_IDLE_TIMEOUT", "IRC_PONG_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("APP_ORIGIN", "http://localhost:8080")
	t.Setenv("LISTEN_ADDR", "127.0.0.1:8080")
	t.Setenv("IRC_ROOMS", "#test")
}

func TestProductionSettingsRejectUnsafeOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
	}{
		{"trust all IPv4", map[string]string{"TRUSTED_PROXY_CIDRS": "0.0.0.0/0"}},
		{"trust all IPv6", map[string]string{"TRUSTED_PROXY_CIDRS": "::/0"}},
		{"malformed proxy", map[string]string{"TRUSTED_PROXY_CIDRS": "127.0.0.1"}},
		{"disabled admission", map[string]string{"WS_MAX_CONNECTIONS": "0"}},
		{"negative body timeout", map[string]string{"HTTP_BODY_TIMEOUT": "-1s"}},
		{"write deadline interrupts sends", map[string]string{"HTTP_WRITE_TIMEOUT": "20s"}},
		{"payload retention breaks late echoes", map[string]string{"SEND_PAYLOAD_RETENTION": "10m"}},
		{"unbounded transaction batch", map[string]string{"RETENTION_BATCH_SIZE": "10001"}},
		{"implicit public origin", map[string]string{"LISTEN_ADDR": "0.0.0.0:8080", "APP_ORIGIN": ""}},
		{"credentials in origin", map[string]string{"APP_ORIGIN": "https://user@example.org"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			productionTestEnv(t)
			for name, value := range tc.values {
				t.Setenv(name, value)
			}
			if _, err := Load(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestMappedTrustedProxyMatchesIPv4Peer(t *testing.T) {
	productionTestEnv(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "::ffff:192.0.2.4/128")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.TrustedProxies) != 1 || cfg.Security.TrustedProxies[0].String() != "192.0.2.4/32" {
		t.Fatalf("proxy range was not normalized: %v", cfg.Security.TrustedProxies)
	}
}
