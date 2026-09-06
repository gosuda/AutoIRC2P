package config

import (
	"fmt"
	"os"
	"strings"

	"gosuda.org/portalite"
)

func (c *Config) loadPortalite() error {
	switch os.Getenv("PORTALITE") {
	case "", "0":
	case "1":
		c.Portalite = true
	default:
		return fmt.Errorf("PORTALITE must be 0 or 1")
	}
	c.PortaliteName = os.Getenv("PORTALITE_NAME")
	name := c.PortaliteName
	if len(name) > 63 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("PORTALITE_NAME must be a lowercase DNS label of 1–63 characters")
	}
	for _, char := range name {
		letter := char >= 'a' && char <= 'z'
		digit := char >= '0' && char <= '9'
		if !letter && !digit && char != '-' {
			return fmt.Errorf("PORTALITE_NAME must contain only lowercase ASCII letters, digits and interior hyphens")
		}
	}
	c.PortaliteRelays = portalite.DefaultRelays()
	if raw := os.Getenv("PORTALITE_RELAYS"); raw != "" {
		relays := strings.Split(raw, ",")
		for _, relay := range relays {
			if !strings.HasPrefix(strings.TrimSpace(relay), "https://") {
				return fmt.Errorf("PORTALITE_RELAYS must be a comma-separated list of HTTPS relay URLs")
			}
		}
		var err error
		c.PortaliteRelays, err = portalite.NormalizeRelays(relays)
		if err != nil {
			return fmt.Errorf("PORTALITE_RELAYS: %w", err)
		}
	}
	return nil
}
