package translate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	promptVersion    = "irc-v1"
	operationTimeout = 90 * time.Second
	failureTTL       = 15 * time.Second
	maxFailures      = 512
)

var (
	ErrConfig      = errors.New("translation configuration is invalid")
	ErrInput       = errors.New("translation input or target language is invalid")
	ErrUnavailable = errors.New("translation models are temporarily unavailable")
	ErrProvider    = errors.New("translation provider request failed")
	ErrResponse    = errors.New("translation provider returned an invalid translation")
	ErrCache       = errors.New("translation cache operation failed")
)

type Config struct {
	BaseURL, APIKey string
	Models          []string
	// Zero selects defaults: 2 seconds, 1 minute, and 3 failures.
	Interval, Cooldown time.Duration
	FailureThreshold   int
}

type Cache interface {
	GetTranslation(ctx context.Context, key string) (string, error)
	PutTranslation(ctx context.Context, key, value string) error
}

type modelState struct {
	name     string
	failures int
	until    time.Time
}

type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   string
	err     error
}

type failure struct {
	until time.Time
	err   error
}

// Translator is concurrency-safe. Its client and cache remain caller-owned.
// Work is canceled when its last waiter leaves, or after 90 seconds.
// Every model is attempted at most once per uncached translation.
type Translator struct {
	cfg      Config
	endpoint string
	client   *http.Client
	cache    Cache
	mu       sync.Mutex
	flights  map[string]*flight
	failed   map[string]failure
	// gate serializes provider calls so a Retry-After applies before any next call.
	gate   chan struct{}
	models []modelState
	cursor int
	next   time.Time
}

func New(cfg Config, client *http.Client, cache Cache) (*Translator, error) {
	if client == nil || cache == nil || len(cfg.Models) == 0 {
		return nil, ErrConfig
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.ContainsAny(cfg.APIKey, "\r\n\x00") {
		return nil, ErrConfig
	}
	if cfg.Interval < 0 || cfg.Cooldown < 0 || cfg.FailureThreshold < 0 {
		return nil, ErrConfig
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, ErrConfig
	}
	if u.Host == "" || u.User != nil {
		return nil, ErrConfig
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrConfig
	}
	if !allowedTransport(u) {
		return nil, ErrConfig
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/chat/completions") {
		u.Path += "/chat/completions"
	}
	if cfg.Interval == 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = time.Minute
	}
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 3
	}
	models := make([]modelState, 0, len(cfg.Models))
	seen := make(map[string]bool, len(cfg.Models))
	for _, name := range cfg.Models {
		if strings.TrimSpace(name) != name || name == "" || strings.ContainsAny(name, "\r\n\x00") || seen[name] {
			return nil, ErrConfig
		}
		seen[name] = true
		models = append(models, modelState{name: name})
	}
	cfg.Models = nil
	ownedClient := *client
	// Redirect targets are not trusted with either the API key or chat text.
	ownedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Translator{
		cfg: cfg, endpoint: u.String(), client: &ownedClient, cache: cache,
		flights: make(map[string]*flight), failed: make(map[string]failure),
		gate: make(chan struct{}, 1), models: models,
	}, nil
}

func allowedTransport(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

func (t *Translator) Translate(ctx context.Context, text, target string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, ok := languageNames[target]; !ok || strings.TrimSpace(text) == "" || len(text) > 16384 || !utf8.ValidString(text) || strings.ContainsAny(text, "\x00\x01") {
		return "", ErrInput
	}
	digest := sha256.Sum256([]byte(promptVersion + "\x00" + target + "\x00" + text))
	key := hex.EncodeToString(digest[:])
	t.mu.Lock()
	now := time.Now()
	if f, ok := t.failed[key]; ok && now.Before(f.until) {
		t.mu.Unlock()
		return "", f.err
	}
	f, ok := t.flights[key]
	if !ok {
		// The initiating request does not own other callers' cancellation.
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		t.flights[key] = f
		go t.run(workCtx, key, text, target, f)
	}
	f.waiters++
	t.mu.Unlock()
	select {
	case <-ctx.Done():
		t.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
			if t.flights[key] == f {
				delete(t.flights, key)
			}
		}
		t.mu.Unlock()
		return "", ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

func (t *Translator) run(ctx context.Context, key, text, target string, f *flight) {
	defer f.cancel()
	value, err := t.cachedTranslate(ctx, key, text, target)
	t.mu.Lock()
	defer t.mu.Unlock()
	f.value, f.err = value, err
	if t.flights[key] == f {
		delete(t.flights, key)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.cacheFailure(key, err)
		}
	}
	close(f.done)
}

// Called with mu held, after an owned flight fails.
func (t *Translator) cacheFailure(key string, err error) {
	now := time.Now()
	var oldestKey string
	var oldest time.Time
	for k, previous := range t.failed {
		if !now.Before(previous.until) {
			delete(t.failed, k)
			continue
		}
		if oldest.IsZero() || previous.until.Before(oldest) {
			oldestKey, oldest = k, previous.until
		}
	}
	if len(t.failed) >= maxFailures {
		delete(t.failed, oldestKey)
	}
	t.failed[key] = failure{until: now.Add(failureTTL), err: err}
}

func (t *Translator) cachedTranslate(ctx context.Context, key, text, target string) (string, error) {
	value, err := t.cache.GetTranslation(ctx, key)
	if err == nil && strings.TrimSpace(value) != "" {
		return value, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", safeContextError(ctx, ErrCache)
	}
	value, err = t.generate(ctx, text, target)
	if err != nil {
		return "", err
	}
	if err := t.cache.PutTranslation(ctx, key, value); err != nil {
		return "", safeContextError(ctx, ErrCache)
	}
	return value, nil
}

func safeContextError(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fallback
}

func waitUntil(ctx context.Context, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := time.Until(until)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
