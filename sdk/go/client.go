package jdix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the control plane's address when none is configured.
const DefaultBaseURL = "https://api.jdix.example.com"

// Client talks to the jdix-sandbox control plane.
type Client struct {
	baseURL   string
	apiKey    string
	http      *http.Client
	userAgent string
	maxRetry  int
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the credential. Keys look like jdix_sk_<id>_<secret>.
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithBaseURL points the client at a different control plane.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithHTTPClient supplies your own transport, for proxies or instrumentation.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithUserAgent adds your application's name to the User-Agent, which is what
// makes a support conversation about "unusual traffic" tractable.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// NewClient builds a client. It fails only when the configuration is unusable;
// no network call is made here.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		baseURL:   DefaultBaseURL,
		http:      &http.Client{Timeout: 90 * time.Second},
		userAgent: "jdix-go/0.1",
		maxRetry:  3,
	}
	for _, o := range opts {
		o(c)
	}
	if c.apiKey == "" {
		return nil, errors.New("jdix: an API key is required (jdix.WithAPIKey)")
	}
	return c, nil
}

// Error is a structured failure from the control plane or a sandbox.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("jdix: %s (%d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("jdix: request failed with %d", e.StatusCode)
}

// Sentinel errors let callers switch on the common cases with errors.Is.
var (
	ErrQuotaExceeded = errors.New("jdix: quota exceeded")
	ErrUnauthorized  = errors.New("jdix: unauthorized")
	ErrNotFound      = errors.New("jdix: not found")
	ErrExpired       = errors.New("jdix: sandbox expired")
	ErrTimeout       = errors.New("jdix: timed out")
)

// Is lets errors.Is(err, ErrQuotaExceeded) work on a structured Error.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrQuotaExceeded:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrExpired:
		return e.StatusCode == http.StatusGone
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, headers map[string]string) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetry; attempt++ {
		if attempt > 0 {
			// Full jitter: a fleet of clients retrying a blip must not
			// synchronise into a second one.
			backoff := time.Duration(rand.Int63n(int64(time.Duration(1<<attempt) * 100 * time.Millisecond)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if !retryable(method, 0) {
				return err
			}
			continue
		}
		apiErr := decodeError(resp)
		if apiErr == nil {
			defer resp.Body.Close()
			if out != nil {
				return json.NewDecoder(resp.Body).Decode(out)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		resp.Body.Close()
		lastErr = apiErr
		if !retryable(method, apiErr.StatusCode) {
			return apiErr
		}
	}
	return lastErr
}

// retryable keeps retries to requests that are safe to repeat, and to failures
// that a repeat might actually fix.
//
// POST /sandboxes is excluded even though the server makes it idempotent with a
// key, because the SDK does not set one on the caller's behalf: silently
// retrying a create is how a network hiccup turns into two bills.
//
// 429 is excluded too. A quota is not going to clear in the next few hundred
// milliseconds, so retrying behind Retry-After only turns an immediate, clear
// error into a long hang. The error carries RetryAfter and the caller decides.
func retryable(method string, status int) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	switch status {
	case 0, // transport failure
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func decodeError(resp *http.Response) *Error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	e := &Error{StatusCode: resp.StatusCode}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if json.Unmarshal(b, &body) == nil {
		e.Code, e.Message = body.Code, body.Message
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(b))
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		var secs int
		if _, err := fmt.Sscanf(ra, "%d", &secs); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}
