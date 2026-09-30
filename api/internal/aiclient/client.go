// Package aiclient is the HTTP client for the Python AI service.
//
// Every call is bounded by a per-operation timeout on top of the caller's
// context, and every failure is reported as an *Error that says whether the
// service was unreachable, timed out, or answered with an error status (and
// what it said), so callers can decide between retrying, degrading and
// failing.
package aiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default per-operation timeouts. Embedding calls out to a provider, so it
// gets longer than a health probe.
const (
	DefaultHealthTimeout = 3 * time.Second
	DefaultEmbedTimeout  = 15 * time.Second
	maxErrorBody         = 4 << 10
)

// Sentinel causes an *Error can wrap; test with errors.Is.
var (
	ErrUnavailable = errors.New("ai service unavailable") // connection refused, DNS, 502/503/504
	ErrTimeout     = errors.New("ai service timed out")
	ErrBadRequest  = errors.New("ai service rejected the request") // 4xx
	ErrBadResponse = errors.New("ai service returned an unexpected response")
)

// Error is the failure type for every client call.
type Error struct {
	Op         string // "health", "embed"
	StatusCode int    // 0 when no response was received
	Detail     string // the service's `detail` field or body excerpt, if any
	cause      error  // one of the sentinels
	wrapped    error  // the underlying transport / decode error, if any
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "aiclient %s: %v", e.Op, e.cause)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (status %d)", e.StatusCode)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, ": %s", e.Detail)
	}
	if e.wrapped != nil && e.Detail == "" {
		fmt.Fprintf(&b, ": %v", e.wrapped)
	}
	return b.String()
}

// Is lets errors.Is(err, ErrTimeout) etc. work through the wrapper.
func (e *Error) Is(target error) bool { return target == e.cause }

func (e *Error) Unwrap() error { return e.wrapped }

// Client is a thin HTTP client for the Python AI service.
type Client struct {
	baseURL       string
	http          *http.Client
	healthTimeout time.Duration
	embedTimeout  time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the transport (tests, custom TLS, tracing).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithTimeouts overrides the per-operation deadlines.
func WithTimeouts(health, embed time.Duration) Option {
	return func(c *Client) {
		c.healthTimeout = health
		c.embedTimeout = embed
	}
}

func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		http:          &http.Client{}, // deadlines come from the per-call context
		healthTimeout: DefaultHealthTimeout,
		embedTimeout:  DefaultEmbedTimeout,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Health returns nil when the AI service reports healthy.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, "health", http.MethodGet, "/health", nil, c.healthTimeout, nil)
}

// EmbedResult is what /embed returns.
type EmbedResult struct {
	Embedding []float32 `json:"embedding"`
	Dim       int       `json:"dim"`
	Provider  string    `json:"provider"`
}

// Embed asks the AI service for an embedding of text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	var out EmbedResult
	err := c.do(ctx, "embed", http.MethodPost, "/embed", map[string]string{"text": text}, c.embedTimeout, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, &Error{Op: "embed", StatusCode: http.StatusOK, cause: ErrBadResponse, Detail: "empty embedding"}
	}
	return out.Embedding, nil
}

// do performs one JSON round trip. body is marshalled when non-nil; the
// response is decoded into out when non-nil.
func (c *Client) do(parent context.Context, op, method, path string, body any, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return &Error{Op: op, cause: ErrBadRequest, wrapped: err}
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return &Error{Op: op, cause: ErrBadRequest, wrapped: err}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if perr := parent.Err(); perr != nil {
			return perr // the caller's own cancellation or deadline; not the service's fault
		}
		cause := ErrUnavailable
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cause = ErrTimeout
		}
		return &Error{Op: op, cause: cause, wrapped: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return &Error{Op: op, StatusCode: resp.StatusCode, cause: classify(resp.StatusCode), Detail: readDetail(resp.Body)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Op: op, StatusCode: resp.StatusCode, cause: ErrBadResponse, wrapped: err}
	}
	return nil
}

// classify picks the sentinel for a non-2xx status. Overload and gateway
// failures are transient, so they count as unavailable and worth a retry.
func classify(status int) error {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return ErrUnavailable
	}
	switch {
	case status >= 500:
		return ErrBadResponse
	default:
		return ErrBadRequest
	}
}

// readDetail extracts FastAPI's {"detail": ...} when present, otherwise a
// bounded excerpt of the body.
func readDetail(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	var body struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Detail) > 0 {
		var s string
		if json.Unmarshal(body.Detail, &s) == nil {
			return s
		}
		return string(body.Detail)
	}
	return strings.TrimSpace(string(raw))
}
