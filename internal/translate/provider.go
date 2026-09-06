package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type providerError struct {
	status int
}

func (e *providerError) Error() string {
	return fmt.Sprintf("translation provider returned HTTP %d", e.status)
}

func (e *providerError) Unwrap() error { return ErrProvider }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

func (t *Translator) generate(ctx context.Context, text, target string) (string, error) {
	prompt, err := makePrompt(text, target)
	if err != nil {
		return "", err
	}
	tried := make([]bool, len(t.models))
	lastErr := error(ErrUnavailable)
	for range t.models {
		select {
		case t.gate <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		value, index, err := t.attempt(ctx, prompt, tried)
		<-t.gate
		if index < 0 {
			if err != nil && err != ErrUnavailable {
				return "", err
			}
			return "", lastErr
		}
		tried[index] = true
		if err == nil {
			return value, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

// Called only while holding gate; all pacing and model state share that owner.
func (t *Translator) attempt(ctx context.Context, prompt translationPrompt, tried []bool) (string, int, error) {
	if err := waitUntil(ctx, t.next); err != nil {
		return "", -1, err
	}
	now := time.Now()
	index := -1
	for offset := range t.models {
		i := (t.cursor + offset) % len(t.models)
		if !tried[i] && !now.Before(t.models[i].until) {
			index = i
			break
		}
	}
	if index < 0 {
		return "", -1, ErrUnavailable
	}
	t.cursor = (index + 1) % len(t.models)
	model := &t.models[index]
	t.next = now.Add(t.cfg.Interval)
	value, status, retryAt, err := t.complete(ctx, model.name, prompt)
	if retryAt.After(t.next) {
		t.next = retryAt
	}
	if err == nil {
		model.failures = 0
		model.until = time.Time{}
		return value, index, nil
	}
	if ctx.Err() != nil {
		return "", index, ctx.Err()
	}
	model.failures++
	if status == http.StatusTooManyRequests || model.failures >= t.cfg.FailureThreshold {
		model.until = time.Now().Add(t.cfg.Cooldown)
		if retryAt.After(model.until) {
			model.until = retryAt
		}
	}
	if status == http.StatusTooManyRequests && retryAt.IsZero() {
		// Quota failures without guidance must not immediately hit another model.
		t.next = model.until
	}
	return "", index, err
}

func (t *Translator) complete(ctx context.Context, model string, prompt translationPrompt) (string, int, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(completionRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: prompt.text}},
	})
	if err != nil {
		return "", 0, time.Time{}, ErrProvider
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, time.Time{}, ErrProvider
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	resp, err := t.client.Do(req)
	if err != nil {
		return "", 0, time.Time{}, safeContextError(ctx, ErrProvider)
	}
	retryAt := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	closeErr := resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Provider bodies may echo credentials or private input; never surface them.
		return "", resp.StatusCode, retryAt, &providerError{status: resp.StatusCode}
	}
	if readErr != nil || closeErr != nil || len(data) > 128*1024 {
		return "", resp.StatusCode, retryAt, safeContextError(ctx, ErrResponse)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &completion); err != nil || len(completion.Choices) != 1 {
		return "", resp.StatusCode, retryAt, ErrResponse
	}
	choice := completion.Choices[0]
	if choice.Message.Refusal != "" || (choice.FinishReason != "" && choice.FinishReason != "stop") {
		return "", resp.StatusCode, retryAt, ErrResponse
	}
	value, err := prompt.extract(choice.Message.Content)
	return value, resp.StatusCode, retryAt, err
}

func parseRetryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		// Saturate durations rather than overflow into an immediate retry.
		const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
		return now.Add(time.Duration(min(seconds, maxSeconds)) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date
	}
	return time.Time{}
}
