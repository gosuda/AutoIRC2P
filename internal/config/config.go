package config

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Listen, Origin, DataDir, WebDir, IVNPConfig, IRCServer, BaseURL, APIKey string
	ObserverNick, ObserverPassword                                          string
	Models, Rooms                                                           []string
	Interval, Cooldown                                                      time.Duration
	IRCIdleTimeout, IRCPongTimeout                                          time.Duration
	Security                                                                Security
	Retention                                                               Retention
	HTTPBodyTimeout, HTTPWriteTimeout, IRCAccountIdleGrace                  time.Duration
	IRCMaxAccounts                                                          int
	SecureCookies, Offline                                                  bool
	TranslationEnabled                                                      bool
	Portalite                                                               bool
	PortaliteName                                                           string
	PortaliteRelays                                                         []string
}

func LoadEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return errors.Join(fmt.Errorf("invalid environment assignment"), file.Close())
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			quote := value[0]
			if (quote == '\'' || quote == '"') && value[len(value)-1] == quote {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return errors.Join(err, file.Close())
			}
		}
	}
	return errors.Join(scanner.Err(), file.Close())
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

const defaultRooms = "#i2p,#i2p-chat,#saltr,#i2p-dev,#i2pd-dev,#i2pd,#ru,#scanners,#i2p-news,#i2c2p,#salt,#freedom,#i2pd-ru,#go-i2p-dev,#i2people,#ko,#ivnp-dev,#ivnp,#dev,#i2p-design,#i2p-de,#go-i2p"

func Load() (Config, error) {
	cfg := Config{Listen: env("LISTEN_ADDR", "127.0.0.1:8080"), Origin: env("APP_ORIGIN", "http://localhost:8080"), DataDir: env("DATA_DIR", "data"), WebDir: env("WEB_DIR", "web/build"), IVNPConfig: env("IVNP_CONFIG", "data/ivnp.conf"), IRCServer: env("IRC_SERVER", "irc.postman.i2p:6667"), Rooms: strings.Split(env("IRC_ROOMS", defaultRooms), ","), Offline: os.Getenv("IRC_OFFLINE") == "1"}
	switch os.Getenv("TRANSLATION_ENABLED") {
	case "", "1":
		cfg.TranslationEnabled = true
	case "0":
	default:
		return cfg, fmt.Errorf("TRANSLATION_ENABLED must be 0 or 1")
	}
	if err := cfg.loadPortalite(); err != nil {
		return cfg, err
	}
	cfg.ObserverNick = os.Getenv("IRC_OBSERVER_NICK")
	cfg.ObserverPassword = os.Getenv("IRC_OBSERVER_PASSWORD")
	if (cfg.ObserverNick == "") != (cfg.ObserverPassword == "") {
		return cfg, fmt.Errorf("IRC_OBSERVER_NICK and IRC_OBSERVER_PASSWORD must be set together")
	}
	var err error
	if cfg.Portalite {
		cfg.Origin = ""
		cfg.SecureCookies = true
	} else {
		parsed, parseErr := url.Parse(cfg.Origin)
		if parseErr != nil {
			return cfg, fmt.Errorf("invalid APP_ORIGIN: %w", parseErr)
		}
		validScheme := parsed.Scheme == "https" || parsed.Scheme == "http"
		invalidOrigin := parsed.Host == "" || !validScheme || parsed.Path != ""
		if invalidOrigin || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return cfg, fmt.Errorf("APP_ORIGIN must be an http(s) origin without trailing slash")
		}
		cfg.SecureCookies = parsed.Scheme == "https"
		if !cfg.SecureCookies && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
			return cfg, fmt.Errorf("public APP_ORIGIN requires HTTPS")
		}
	}
	for i, room := range cfg.Rooms {
		room = strings.TrimSpace(room)
		if !strings.HasPrefix(room, "#") || len(room) < 2 || len(room) > 64 || strings.ContainsAny(room, " ,\r\n\x00\x07:") {
			return cfg, fmt.Errorf("invalid IRC_ROOMS channel")
		}
		cfg.Rooms[i] = room
	}
	if cfg.TranslationEnabled {
		cfg.BaseURL = os.Getenv("OPENAI_BASE_URL")
		cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		cfg.Models = []string{env("OPENAI_MODEL_0", "gemma-4-31b-it"), env("OPENAI_MODEL_1", "gemma-4-26b-a4b-it")}
		cfg.Interval, err = time.ParseDuration(env("TRANSLATION_INTERVAL", "5s"))
		if err != nil || cfg.Interval <= 0 {
			return cfg, fmt.Errorf("TRANSLATION_INTERVAL must be positive duration")
		}
		cfg.Cooldown, err = time.ParseDuration(env("TRANSLATION_COOLDOWN", "60s"))
		if err != nil || cfg.Cooldown <= 0 {
			return cfg, fmt.Errorf("TRANSLATION_COOLDOWN must be positive duration")
		}
	}
	cfg.IRCIdleTimeout, err = time.ParseDuration(env("IRC_IDLE_TIMEOUT", "20m"))
	if err != nil || cfg.IRCIdleTimeout <= 0 {
		return cfg, fmt.Errorf("IRC_IDLE_TIMEOUT must be a positive duration")
	}
	cfg.IRCPongTimeout, err = time.ParseDuration(env("IRC_PONG_TIMEOUT", "2m"))
	if err != nil || cfg.IRCPongTimeout <= 0 {
		return cfg, fmt.Errorf("IRC_PONG_TIMEOUT must be a positive duration")
	}
	if err := cfg.loadProduction(); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func Key(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("application key must be a private regular file (0600)")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(b) != 32 {
			return nil, fmt.Errorf("invalid application key length")
		}
		return b, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	b := make([]byte, 32)
	rand.Read(b)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(b)
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	return b, nil
}
