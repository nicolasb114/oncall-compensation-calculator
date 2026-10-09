// Package pd is a small, polite PagerDuty REST client: it paginates until the API says
// there is no more data, paces its calls, and retries transient failures.
package pd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Settings control pacing and retries (adjustable in the UI).
type Settings struct {
	DelayMS    int `json:"delayMs"`    // minimum gap between calls
	MaxRetries int `json:"maxRetries"` // retries per call
	TimeoutS   int `json:"timeoutS"`   // per-request timeout
}

// DefaultSettings are safe defaults, well under PagerDuty's published limits.
func DefaultSettings() Settings { return Settings{DelayMS: 500, MaxRetries: 6, TimeoutS: 30} }

// Event is a progress notification.
type Event struct {
	Kind string // "call", "retry", "wait", "page"
	Msg  string
}

// Client talks to the PagerDuty REST API.
type Client struct {
	BaseURL string
	Token   string
	S       Settings
	HTTP    *http.Client
	OnEvent func(Event)

	mu            sync.Mutex
	last          time.Time
	Calls         int
	Retries       int
	Waits         int
	sleep         func(context.Context, time.Duration) error
	paused        bool
	pauseCh       chan struct{}
	analyticsPath string
}

// New creates a client. region is "us" or "eu".
func New(token, region string, s Settings) *Client {
	base := "https://api.pagerduty.com"
	if region == "eu" {
		base = "https://api.eu.pagerduty.com"
	}
	if v := os.Getenv("ONCALL_TEST_PD_URL"); v != "" { // for local testing against a fake API
		base = v
	}
	if s.DelayMS < 0 {
		s.DelayMS = 0
	}
	if s.MaxRetries < 0 {
		s.MaxRetries = 0
	}
	if s.TimeoutS <= 0 {
		s.TimeoutS = 30
	}
	return &Client{BaseURL: base, Token: token, S: s, HTTP: &http.Client{Timeout: time.Duration(s.TimeoutS) * time.Second}, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) emit(kind, msg string) {
	if c.OnEvent != nil {
		c.OnEvent(Event{kind, msg})
	}
}

// Pause / Resume stop and restart issuing new calls.
func (c *Client) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		c.paused = true
		c.pauseCh = make(chan struct{})
	}
}
func (c *Client) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		c.paused = false
		close(c.pauseCh)
	}
}

func (c *Client) waitIfPaused(ctx context.Context) error {
	c.mu.Lock()
	ch, p := c.pauseCh, c.paused
	c.mu.Unlock()
	if !p {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) pace(ctx context.Context) error {
	if err := c.waitIfPaused(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	wait := time.Duration(c.S.DelayMS)*time.Millisecond - time.Since(c.last)
	c.last = time.Now().Add(max(wait, 0))
	c.mu.Unlock()
	if wait > 0 {
		return c.sleep(ctx, wait)
	}
	return nil
}

// APIError is a non-retryable API failure.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("PagerDuty API returned %d: %s", e.Status, truncate(e.Body, 300))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// IsEntitlement reports whether err means the account/key lacks access (401/402/403).
func IsEntitlement(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 402 || ae.Status == 403)
}

func retryable(status int) bool { return status == 429 || status >= 500 }

// Do performs a request with pacing and retries and returns the response body.
func (c *Client) Do(ctx context.Context, method, path string, query map[string][]string, body any) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	url := c.BaseURL + path
	var lastErr error
	for attempt := 0; attempt <= c.S.MaxRetries; attempt++ {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		q := req.URL.Query()
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		req.URL.RawQuery = q.Encode()
		req.Header.Set("Authorization", "Token token="+c.Token)
		req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		c.mu.Lock()
		c.Calls++
		c.mu.Unlock()
		c.emit("call", method+" "+path)
		resp, err := c.HTTP.Do(req)
		var data []byte
		status := 0
		var retryAfter time.Duration
		if err == nil {
			status = resp.StatusCode
			data, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			retryAfter = parseRetryAfter(resp.Header)
		}
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil: // timeout / dropped connection
			lastErr = fmt.Errorf("network error: %w", err)
		case status >= 200 && status < 300:
			return data, nil
		case retryable(status):
			lastErr = &APIError{status, string(data)}
		default:
			return nil, &APIError{status, string(data)}
		}
		if attempt == c.S.MaxRetries {
			break
		}
		wait := backoff(attempt)
		if retryAfter > wait {
			wait = retryAfter
		}
		c.mu.Lock()
		c.Retries++
		if status == 429 {
			c.Waits++
		}
		c.mu.Unlock()
		c.emit("retry", fmt.Sprintf("%s %s failed (%v); retry %d/%d in %s", method, path, shortErr(lastErr), attempt+1, c.S.MaxRetries, wait.Round(100*time.Millisecond)))
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("gave up after %d attempts: %w", c.S.MaxRetries+1, lastErr)
}

func shortErr(e error) string {
	var ae *APIError
	if errors.As(e, &ae) {
		return strconv.Itoa(ae.Status)
	}
	return truncate(e.Error(), 80)
}

// backoff grows exponentially with jitter: ~2s, 4s, 8s ... capped at 60s.
func backoff(attempt int) time.Duration {
	d := math.Min(60, 2*math.Pow(2, float64(attempt)))
	j := 0.75 + rand.Float64()*0.5
	return time.Duration(d * j * float64(time.Second))
}

// parseRetryAfter honors Retry-After and PagerDuty's ratelimit-reset (seconds) headers.
func parseRetryAfter(h http.Header) time.Duration {
	for _, k := range []string{"Retry-After", "Ratelimit-Reset"} {
		if v := h.Get(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return time.Duration(n+1) * time.Second
			}
		}
	}
	return 0
}

// Paginate follows classic offset pagination until `more` is false (or a short/empty page),
// calling fn with the raw JSON of each page. A short page is never taken as the end by itself:
// only the API's own `more` flag, or an empty page, ends the loop.
func (c *Client) Paginate(ctx context.Context, path string, query map[string][]string, fn func(page json.RawMessage) error) error {
	const limit = 100
	q := map[string][]string{}
	for k, v := range query {
		q[k] = v
	}
	q["limit"] = []string{strconv.Itoa(limit)}
	offset := 0
	for pageNo := 1; ; pageNo++ {
		q["offset"] = []string{strconv.Itoa(offset)}
		data, err := c.Do(ctx, "GET", path, q, nil)
		if err != nil {
			return err
		}
		var env struct {
			More   bool `json:"more"`
			Limit  int  `json:"limit"`
			Offset int  `json:"offset"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("unexpected response from %s: %w", path, err)
		}
		c.emit("page", fmt.Sprintf("%s page %d", path, pageNo))
		if err := fn(data); err != nil {
			return err
		}
		if !env.More {
			return nil
		}
		// The server may return fewer rows than we asked for; advance by what it says it returned.
		step := env.Limit
		if step <= 0 {
			step = limit
		}
		n := countRows(data)
		if n == 0 {
			return nil
		}
		if n < step {
			step = n
		}
		offset += step
		if offset >= 9900 {
			return fmt.Errorf("%s: too many results for offset pagination; narrow the filters or period", path)
		}
	}
}

// countRows finds the length of the first array field in the response.
func countRows(data []byte) int {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return 0
	}
	for _, v := range m {
		var arr []json.RawMessage
		if json.Unmarshal(v, &arr) == nil {
			return len(arr)
		}
	}
	return 0
}

func asAPIError(err error, target **APIError) bool { return errors.As(err, target) }
