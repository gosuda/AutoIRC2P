package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type Security struct {
	TrustedProxies                                                  []netip.Prefix
	MaxWebSockets, MaxWebSocketsPerIP, MaxWebSocketsPerAccount      int
	WSHandshakesPerMinute, CursorUpdatesPerMinute                   int
	SendRequestsPerMinute, SendRequestsPerIPMinute, MaxPendingSends int
}

type Retention struct {
	Messages, Translations, SendPayloads, Interval time.Duration
	BatchSize                                      int
}

func positiveInt(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
func duration(name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	invalid := err != nil || value < 0
	zeroDenied := !allowZero && value == 0
	if invalid || zeroDenied {
		return 0, fmt.Errorf("%s must be a valid duration", name)
	}
	return value, nil
}
func trustedProxies(raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS contains an invalid CIDR")
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS contains an invalid mapped IPv4 range")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Bits() == 0 {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS must not trust all addresses")
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}
func (c *Config) loadProduction() error {
	var err error
	c.Security.TrustedProxies, err = trustedProxies(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return err
	}
	integers := []struct {
		name     string
		fallback int
		target   *int
	}{
		{"WS_MAX_CONNECTIONS", 512, &c.Security.MaxWebSockets},
		{"WS_MAX_CONNECTIONS_PER_IP", 8, &c.Security.MaxWebSocketsPerIP},
		{"WS_MAX_CONNECTIONS_PER_ACCOUNT", 4, &c.Security.MaxWebSocketsPerAccount},
		{"WS_HANDSHAKES_PER_MINUTE", 30, &c.Security.WSHandshakesPerMinute},
		{"WS_READS_PER_MINUTE", 120, &c.Security.CursorUpdatesPerMinute},
		{"SEND_REQUESTS_PER_MINUTE", 20, &c.Security.SendRequestsPerMinute},
		{"SEND_REQUESTS_PER_IP_MINUTE", 60, &c.Security.SendRequestsPerIPMinute},
		{"SEND_MAX_PENDING", 32, &c.Security.MaxPendingSends},
		{"IRC_MAX_ACCOUNTS", 16, &c.IRCMaxAccounts},
		{"RETENTION_BATCH_SIZE", 500, &c.Retention.BatchSize},
	}
	for _, option := range integers {
		*option.target, err = positiveInt(option.name, option.fallback)
		if err != nil {
			return err
		}
	}
	durations := []struct {
		name      string
		fallback  time.Duration
		allowZero bool
		target    *time.Duration
	}{
		{"HTTP_BODY_TIMEOUT", 15 * time.Second, false, &c.HTTPBodyTimeout},
		{"HTTP_WRITE_TIMEOUT", 150 * time.Second, false, &c.HTTPWriteTimeout},
		{"IRC_ACCOUNT_IDLE_GRACE", 2 * time.Minute, false, &c.IRCAccountIdleGrace},
		{"CHAT_RETENTION", 30 * 24 * time.Hour, true, &c.Retention.Messages},
		{"TRANSLATION_RETENTION", 30 * 24 * time.Hour, true, &c.Retention.Translations},
		{"SEND_PAYLOAD_RETENTION", 30 * 24 * time.Hour, true, &c.Retention.SendPayloads},
		{"RETENTION_INTERVAL", time.Hour, false, &c.Retention.Interval},
	}
	for _, option := range durations {
		*option.target, err = duration(option.name, option.fallback, option.allowZero)
		if err != nil {
			return err
		}
	}
	if c.HTTPWriteTimeout < c.HTTPBodyTimeout+125*time.Second {
		return fmt.Errorf("HTTP_WRITE_TIMEOUT must allow HTTP_BODY_TIMEOUT plus 125s for sending")
	}
	if c.Retention.SendPayloads > 0 && c.Retention.SendPayloads < time.Hour {
		return fmt.Errorf("SEND_PAYLOAD_RETENTION must be at least 1h or zero")
	}
	if c.Retention.BatchSize > 10000 {
		return fmt.Errorf("RETENTION_BATCH_SIZE must not exceed 10000")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR must include a host and port")
	}
	if os.Getenv("APP_ORIGIN") == "" && host != "localhost" {
		ip, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !ip.IsLoopback() {
			return fmt.Errorf("APP_ORIGIN must be explicitly set for a non-loopback listener")
		}
	}
	return nil
}
