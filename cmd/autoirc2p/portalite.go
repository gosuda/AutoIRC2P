package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/AutoIRC2P/internal/config"
	"gosuda.org/portalite"
)

func openHTTPListener(ctx context.Context, cfg config.Config) (net.Listener, func(string) bool, error) {
	if !cfg.Portalite {
		listener, err := net.Listen("tcp", cfg.Listen)
		return listener, func(origin string) bool { return origin == cfg.Origin }, err
	}
	identity, err := loadPortaliteIdentity(filepath.Join(cfg.DataDir, "portalite-identity.json"), cfg.PortaliteName)
	if err != nil {
		return nil, nil, err
	}
	exposure, err := portalite.Expose(ctx, portalite.ExposeConfig{Identity: identity, Relays: cfg.PortaliteRelays})
	if err != nil {
		return nil, nil, err
	}
	return exposure, func(origin string) bool {
		// Lease refreshes can change ports without emitting another ready update.
		return portaliteOriginAllowed(exposure.Relays(), origin)
	}, nil
}

func portaliteOriginAllowed(relays []portalite.RelayStatus, origin string) bool {
	for _, relay := range relays {
		if relay.State == portalite.RelayReady && relay.PublicURL != "" && origin == relay.PublicURL {
			return true
		}
	}
	return false
}

func logPortaliteUpdates(exposure *portalite.Exposure) {
	for status := range exposure.Updates() {
		switch status.State {
		case portalite.RelayReady:
			slog.Info("Portalite URL ready", "url", status.PublicURL, "relay", status.RelayURL)
		case portalite.RelayFailed:
			slog.Error("Portalite relay failed", "relay", status.RelayURL, "error", status.Err)
		}
	}
}

func readPortaliteIdentity(path string) (portalite.Identity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return portalite.Identity{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return portalite.Identity{}, fmt.Errorf("Portalite identity must be a regular file with mode 0600: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return portalite.Identity{}, err
	}
	identity, err := portalite.ParseIdentity(data)
	if err != nil {
		return portalite.Identity{}, fmt.Errorf("parse Portalite identity %s: %w", path, err)
	}
	return identity, nil
}

func loadPortaliteIdentity(path, name string) (_ portalite.Identity, err error) {
	identity, err := readPortaliteIdentity(path)
	if !errors.Is(err, os.ErrNotExist) {
		return identity, err
	}
	if name == "" {
		suffix := rand.Text()
		name = "autoirc2p-" + strings.ToLower(suffix)
	}
	identity, err = portalite.GenerateIdentity(name)
	if err != nil {
		return portalite.Identity{}, err
	}
	data, err := identity.MarshalJSON()
	if err != nil {
		return portalite.Identity{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return portalite.Identity{}, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".portalite-identity-*")
	if err != nil {
		return portalite.Identity{}, err
	}
	defer func() { err = errors.Join(err, os.Remove(file.Name())) }()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return portalite.Identity{}, err
	}
	// Publish complete key material atomically without replacing another writer's identity.
	if err := os.Link(file.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readPortaliteIdentity(path)
		}
		return portalite.Identity{}, err
	}
	return identity, nil
}
