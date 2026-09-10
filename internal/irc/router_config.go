package irc

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/state"
)

const maxRouterDestinations = 64

func loadRouterConfig(path string, destinationCapacity int) (state.ConfigurationOperating, error) {
	if err := createRouterConfig(path); err != nil {
		return state.ConfigurationOperating{}, err
	}
	configuration, err := state.ConfigurationLoadOperating(path)
	if err != nil {
		return state.ConfigurationOperating{}, err
	}
	file, _, err := state.FilesystemStoreOpenRegular(path)
	if err != nil {
		return state.ConfigurationOperating{}, err
	}
	contents, readErr := state.FilesystemStoreReadBoundedFile(file, 1<<20)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return state.ConfigurationOperating{}, err
	}
	if !routerSettingConfigured(string(contents), "tunnel", "hops") {
		configuration.Tunnel.Hops = 1
	}
	if !routerSettingConfigured(string(contents), "state", "max_destinations") {
		configuration.State.MaxDestinations = destinationCapacity
	}
	if err := validateEmbeddedSettings(string(contents)); err != nil {
		return state.ConfigurationOperating{}, err
	}
	if configuration.State.MaxDestinations > maxRouterDestinations {
		return state.ConfigurationOperating{}, fmt.Errorf("state.max_destinations exceeds root API limit %d: %w", maxRouterDestinations, errInvalidConfig)
	}
	return configuration, nil
}

func createRouterConfig(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create IVNP configuration directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString("[tunnel]\nhops = 1\n")
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

// IVNP validates the INI but does not expose key presence or configurable defaults.
// Inspect only section/key names; values and syntax remain IVNP's responsibility.
func routerSettingConfigured(text, wantedSection, wantedKey string) bool {
	section := ""
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			name, _, _ := strings.Cut(line[1:], "]")
			section = strings.TrimSpace(name)
			continue
		}
		if section == wantedSection {
			key, _, found := strings.Cut(line, "=")
			if found && strings.TrimSpace(key) == wantedKey {
				return true
			}
		}
	}
	return false
}

func validateEmbeddedSettings(text string) error {
	section := ""
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			name, _, _ := strings.Cut(line[1:], "]")
			section = strings.TrimSpace(name)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, _, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		allowed := false
		switch section {
		case "paths", "ntcp2", "ssu2", "addressbook", "log":
			allowed = true
		case "network":
			allowed = key == "id"
		case "state":
			allowed = key == "max_destinations"
		case "netdb":
			allowed = key == "bootstrap_router_info_files"
		case "reseed":
			allowed = key == "enabled" || key == "endpoints" || key == "timeout"
		case "tunnel":
			switch key {
			case "enabled", "hops", "exploratory_inbound_target", "exploratory_outbound_target", "client_inbound_target", "client_outbound_target", "renew_before":
				allowed = true
			}
		}
		if !allowed {
			return fmt.Errorf("IVNP root API cannot represent [%s] %s; configuration was not changed: %w", section, key, errInvalidConfig)
		}
	}
	return nil
}

func embeddedRouterConfig(source state.ConfigurationOperating) (ivnp.RouterConfig, ivnp.TunnelPoolConfig, error) {
	cfg := ivnp.DefaultRouterConfig()
	var empty ivnp.TunnelPoolConfig
	if !source.Tunnel.Enabled {
		return cfg, empty, fmt.Errorf("IVNP root API requires tunnels: %w", errInvalidConfig)
	}
	directory := filepath.Clean(source.StateDir)
	if filepath.Clean(source.StatePath) != filepath.Join(directory, "router.state") || filepath.Clean(source.KeyPath) != filepath.Join(directory, "router.keys") {
		return cfg, empty, fmt.Errorf("IVNP root API requires router.state and router.keys in state_dir; existing files were not moved: %w", errInvalidConfig)
	}
	cfg.Persistence = &ivnp.PersistenceConfig{Directory: directory}
	cfg.NetworkID = source.Network.ID
	cfg.Limits.MaxDestinations = source.State.MaxDestinations
	var err error
	cfg.NTCP2, err = embeddedTransport(source.NTCP2)
	if err != nil {
		return cfg, empty, fmt.Errorf("NTCP2 configuration: %w", err)
	}
	cfg.SSU2, err = embeddedTransport(source.SSU2)
	if err != nil {
		return cfg, empty, fmt.Errorf("SSU2 configuration: %w", err)
	}
	if source.Reseed.Enabled {
		cfg.Bootstrap.ReseedURLs = source.Reseed.Endpoints
		cfg.Bootstrap.ReseedTimeout = source.Reseed.Timeout
	} else {
		cfg.Bootstrap.ReseedURLs = nil
	}
	for _, path := range source.NetDB.BootstrapRouterInfoPaths {
		file, _, err := state.FilesystemStoreOpenRegular(path)
		if err != nil {
			return cfg, empty, fmt.Errorf("open bootstrap router info: %w", err)
		}
		data, readErr := state.FilesystemStoreReadBoundedFile(file, 1<<20)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return cfg, empty, fmt.Errorf("read bootstrap router info: %w", err)
		}
		cfg.Bootstrap.RouterInfos = append(cfg.Bootstrap.RouterInfos, data)
	}
	tunnel := source.Tunnel
	cfg.Exploratory = ivnp.TunnelPoolConfig{
		Inbound:     ivnp.TunnelDirectionConfig{Hops: tunnel.Hops, Count: tunnel.ExploratoryInboundTarget},
		Outbound:    ivnp.TunnelDirectionConfig{Hops: tunnel.Hops, Count: tunnel.ExploratoryOutboundTarget},
		RenewBefore: tunnel.RenewBefore,
	}
	clientTunnels := ivnp.TunnelPoolConfig{
		Inbound:     ivnp.TunnelDirectionConfig{Hops: tunnel.Hops, Count: tunnel.ClientInboundTarget},
		Outbound:    ivnp.TunnelDirectionConfig{Hops: tunnel.Hops, Count: tunnel.ClientOutboundTarget},
		RenewBefore: tunnel.RenewBefore,
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(source.Log.Level)); err != nil {
		return cfg, empty, fmt.Errorf("log level: %w", err)
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, options)
	if source.Log.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	}
	cfg.Logger = slog.New(handler)
	return cfg, clientTunnels, nil
}

func embeddedTransport(source state.ConfigurationTransport) (ivnp.TransportConfig, error) {
	if !source.Enabled {
		return ivnp.TransportConfig{}, nil
	}
	cfg := ivnp.TransportConfig{Enabled: true, MaxSessions: source.MaxSessions, IdleTimeout: source.IdleTimeout}
	var err error
	cfg.Bind, err = netip.ParseAddrPort(source.Bind.String())
	if err != nil {
		return cfg, fmt.Errorf("root API requires a numeric bind address: %w", err)
	}
	if source.Advertised.Host != "" || source.Advertised.Port != 0 {
		cfg.Advertised, err = netip.ParseAddrPort(source.Advertised.String())
		if err != nil {
			return cfg, fmt.Errorf("root API requires a numeric advertised address: %w", err)
		}
	}
	return cfg, nil
}
