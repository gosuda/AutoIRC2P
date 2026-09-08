package irc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/state"
)

const maxRouterDestinations = 256

func loadRouterConfig(path string, destinationCapacity int) (ivnp.Config, error) {
	if err := createRouterConfig(path); err != nil {
		return ivnp.Config{}, err
	}
	configuration, err := ivnp.LoadConfig(path)
	if err != nil {
		return ivnp.Config{}, err
	}
	file, _, err := state.FilesystemStoreOpenRegular(path)
	if err != nil {
		return ivnp.Config{}, err
	}
	contents, readErr := state.FilesystemStoreReadBoundedFile(file, 1<<20)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return ivnp.Config{}, err
	}
	if !routerSettingConfigured(string(contents), "tunnel", "hops") {
		configuration.Tunnel.Hops = 1
	}
	if !routerSettingConfigured(string(contents), "state", "max_destinations") {
		configuration.State.MaxDestinations = max(configuration.State.MaxDestinations, destinationCapacity)
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
