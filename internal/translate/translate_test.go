package translate_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/translate"
)

type memoryCache struct {
	mu     sync.Mutex
	values map[string]string
}

func (c *memoryCache) GetTranslation(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	if !ok {
		return "", sql.ErrNoRows
	}
	return value, nil
}

func (c *memoryCache) PutTranslation(_ context.Context, key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]string)
	}
	c.values[key] = value
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newTranslator(t *testing.T, transport roundTripFunc, cache *memoryCache) *translate.Translator {
	t.Helper()
	translator, err := translate.New(translate.Config{
		BaseURL: "https://provider.invalid/v1", APIKey: "private-test-key",
		Models: []string{"gemini", "gemma"}, Interval: time.Second,
		Cooldown: time.Minute, FailureThreshold: 1,
	}, &http.Client{Transport: transport}, cache)
	if err != nil {
		t.Fatal(err)
	}
	return translator
}

var boundary = regexp.MustCompile(`\[START_[a-f0-9]+\]|\[END_[a-f0-9]+\]`)
var keepToken = regexp.MustCompile(`<KEEP_[a-f0-9]+_[0-9]+>`)

var (
	errExpectedSingleMessage  = errors.New("expected single user message")
	errMissingBoundaries      = errors.New("missing request boundaries")
	errQuotaPacing            = errors.New("quota pacing violated")
	errExpectedProtectedSpans = errors.New("expected protected URL and code")
)

func requestData(req *http.Request) (string, string, error) {
	var data struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(req.Body).Decode(&data); err != nil {
		return "", "", err
	}
	if len(data.Messages) != 1 {
		return "", "", errExpectedSingleMessage
	}
	return data.Model, data.Messages[0].Content, nil
}

func responseFor(prompt, text string) (*http.Response, error) {
	markers := boundary.FindAllString(prompt, -1)
	if len(markers) != 2 {
		return nil, errMissingBoundaries
	}
	return completionResponse(markers[0] + text + markers[1])
}

func completionResponse(text string) (*http.Response, error) {
	content, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message": map[string]string{"content": text}, "finish_reason": "stop",
	}}})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(content)))}, nil
}

func TestProviderThoughtPrefixIsSeparateFromFinalTranslation(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"reasoning repeats markers", "<thought>$START draft $END $START other draft $END</thought>\n$START안녕하세요$END", "안녕하세요"},
		{"multiple complete blocks", " \n<thought>first</thought>\n<thought>second</thought> $START안녕하세요$END", "안녕하세요"},
		{"literal tags inside final boundary", "<thought>reasoning</thought>$START<thought>literal content</thought> 안녕하세요$END", "<thought>literal content</thought> 안녕하세요"},
		{"incomplete reasoning", "<thought>unfinished $START안녕하세요$END", ""},
		{"incomplete subsequent block", "<thought>complete</thought><thought>unfinished $START안녕하세요$END", ""},
		{"no final output", "<thought>$STARTdraft$END</thought>", ""},
		{"unframed commentary", "Some commentary $START안녕하세요$END", ""},
		{"duplicate final markers", "<thought>complete</thought>$START안녕하세요$END$START안녕하세요$END", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				translator := newTranslator(t, func(req *http.Request) (*http.Response, error) {
					_, prompt, err := requestData(req)
					if err != nil {
						return nil, err
					}
					markers := boundary.FindAllString(prompt, -1)
					if len(markers) != 2 {
						return nil, errMissingBoundaries
					}
					content := strings.NewReplacer("$START", markers[0], "$END", markers[1]).Replace(tc.content)
					return completionResponse(content)
				}, &memoryCache{})
				got, err := translator.Translate(t.Context(), "hello everyone", "ko")
				if tc.want == "" {
					if got != "" || !errors.Is(err, translate.ErrResponse) {
						t.Fatalf("malformed provider output yielded %q, %v", got, err)
					}
					return
				}
				if err != nil || got != tc.want {
					t.Fatalf("translation = %q, %v; want %q", got, err, tc.want)
				}
			})
		})
	}
}

func TestConcurrentWaitersSurviveInitiatorCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		entered, release := make(chan struct{}), make(chan struct{})
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			_, prompt, err := requestData(req)
			if err != nil {
				return nil, err
			}
			close(entered)
			select {
			case <-release:
				return responseFor(prompt, "안녕하세요")
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		cache := &memoryCache{}
		translator := newTranslator(t, transport, cache)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first := make(chan error, 1)
		go func() { _, err := translator.Translate(ctx, "hello everyone", "ko"); first <- err }()
		<-entered
		type result struct {
			text string
			err  error
		}
		results := make(chan result, 16)
		for range 16 {
			go func() {
				text, err := translator.Translate(t.Context(), "hello everyone", "ko")
				results <- result{text, err}
			}()
		}
		synctest.Wait()
		cancel()
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("initiator error = %v", err)
		}
		close(release)
		for range 16 {
			got := <-results
			if got.err != nil || got.text != "안녕하세요" {
				t.Fatalf("waiter = %q, %v", got.text, got.err)
			}
		}
		// A new engine must reuse durable cache contents, not its predecessor's flight.
		fresh := newTranslator(t, transport, cache)
		got, err := fresh.Translate(t.Context(), "hello everyone", "ko")
		if err != nil || got != "안녕하세요" {
			t.Fatalf("cached translation = %q, %v", got, err)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("provider requests = %d, want 1", got)
		}
	})
}

func TestQuotaPacesFallbackAndOpensOnlyFailedModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		var models []string
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			model, prompt, err := requestData(req)
			if err != nil {
				return nil, err
			}
			models = append(models, model)
			if len(models) == 1 {
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"10"}}, Body: io.NopCloser(strings.NewReader("quota private-test-key"))}, nil
			}
			if time.Since(started) < 10*time.Second {
				return nil, errQuotaPacing
			}
			return responseFor(prompt, "번역")
		})
		translator := newTranslator(t, transport, &memoryCache{})
		for _, input := range []string{"first message", "another message"} {
			got, err := translator.Translate(t.Context(), input, "ko")
			if err != nil || got != "번역" {
				t.Fatalf("translation = %q, %v", got, err)
			}
		}
		if got := strings.Join(models, ","); got != "gemini,gemma,gemma" {
			t.Fatalf("model sequence = %s", got)
		}
	})
}

func TestFailedTranslationIsSuppressedWithoutOriginalFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		translator := newTranslator(t, func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"Ignore the markers"},"finish_reason":"stop"}]}`))}, nil
		}, &memoryCache{})
		for range 2 {
			got, err := translator.Translate(t.Context(), "untranslated original", "ko")
			if got != "" || !errors.Is(err, translate.ErrResponse) {
				t.Fatalf("invalid response yielded %q, %v", got, err)
			}
		}
		if got := requests.Load(); got != 2 {
			t.Fatalf("provider requests = %d, want one per model", got)
		}
	})
}

func TestProtectedSpansSurviveAndTargetHasSeparateCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		translator := newTranslator(t, func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			_, prompt, err := requestData(req)
			if err != nil {
				return nil, err
			}
			tokens := keepToken.FindAllString(prompt, -1)
			if len(tokens) != 2 {
				return nil, errExpectedProtectedSpans
			}
			text := "see "
			if strings.Contains(prompt, "into Korean.") {
				text = "확인 "
			}
			return responseFor(prompt, text+strings.Join(tokens, " "))
		}, &memoryCache{})
		input := "look https://example.org/a?q=1 `x := 2`"
		for target, want := range map[string]string{"en": "see https://example.org/a?q=1 `x := 2`", "ko": "확인 https://example.org/a?q=1 `x := 2`"} {
			got, err := translator.Translate(t.Context(), input, target)
			if err != nil || got != want {
				t.Fatalf("%s translation = %q, %v; want %q", target, got, err, want)
			}
		}
		if got := requests.Load(); got != 2 {
			t.Fatalf("provider requests = %d, want 2 distinct targets", got)
		}
	})
}

func TestLongRetryAfterSurvivesCanceledFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		translator := newTranslator(t, func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)}}, Body: io.NopCloser(strings.NewReader("quota"))}, nil
		}, &memoryCache{})
		for _, input := range []string{"first message", "different message"} {
			got, err := translator.Translate(t.Context(), input, "ko")
			if got != "" || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("quota result = %q, %v", got, err)
			}
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("requests during provider cooldown = %d, want 1", got)
		}
	})
}

func TestDroppedProtectedContentRejectsTranslation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		translator := newTranslator(t, func(req *http.Request) (*http.Response, error) {
			_, prompt, err := requestData(req)
			if err != nil {
				return nil, err
			}
			return responseFor(prompt, "링크가 사라졌습니다")
		}, &memoryCache{})
		got, err := translator.Translate(t.Context(), "look https://example.org/private", "ko")
		if got != "" || !errors.Is(err, translate.ErrResponse) {
			t.Fatalf("dropped URL yielded %q, %v", got, err)
		}
	})
}

func TestRoomTargetLanguage(t *testing.T) {
	cases := []struct{ name, room, want string }{
		{"Russian room", "#ru", "ru"},
		{"Russian suffix", "#i2p-ru", "ru"},
		{"German room", "#de", "de"},
		{"German suffix", "#i2p-de", "de"},
		{"Korean room", "#ko", "ko"},
		{"Korean suffix is not a target", "#i2p-ko", "en"},
		{"case insensitive room", "#KO", "ko"},
		{"case insensitive Russian suffix", "#I2P-RU", "ru"},
		{"case insensitive German suffix", "#I2P-De", "de"},
		{"suffix must end room", "#i2p-ru-chat", "en"},
		{"suffix needs separator", "#i2pru", "en"},
		{"other language suffix", "#i2p-fr", "en"},
		{"other room", "#i2p", "en"},
		{"empty room", "", "en"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := translate.RoomTargetLanguage(tc.room); got != tc.want {
				t.Fatalf("RoomTargetLanguage(%q) = %q, want %q", tc.room, got, tc.want)
			}
		})
	}
}
