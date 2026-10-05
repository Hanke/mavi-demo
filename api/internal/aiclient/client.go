// Package aiclient is the HTTP client for the Python AI service.
//
// Every call is bounded by a per-operation timeout on top of the caller's
// context, and every failure is reported as an *Error that says whether the
// service was unreachable, timed out, or answered with an error status (and
// what it said), so callers can decide between retrying, degrading and
// failing.
//
// Request and response types come from ai/openapi.json, the OpenAPI document
// the FastAPI app exports, generated into types.gen.go by oapi-codegen. A
// change to a Python model has to be re-exported (`make generate`) before
// this package compiles against it.
package aiclient

//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../../ai/openapi.json

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
// gets longer than a health probe; the service gives one file 20s to be read;
// and a parse is a chat-model call over a whole document, retried once inside
// the service when the output does not validate. The parse deadline has to
// stay under the worker's lock timeout (5m by default), which must outlast
// the slowest job.
const (
	DefaultHealthTimeout  = 3 * time.Second
	DefaultEmbedTimeout   = 15 * time.Second
	DefaultExtractTimeout = 30 * time.Second
	DefaultParseTimeout   = 4 * time.Minute
	maxErrorBody          = 4 << 10
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
	Op         string // "health", "embed", "embed-batch", "extract-text", "parse-resume", "parse-jd", "rerank"
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
	baseURL        string
	http           *http.Client
	healthTimeout  time.Duration
	embedTimeout   time.Duration
	extractTimeout time.Duration
	parseTimeout   time.Duration
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

// WithParseTimeout overrides the deadline of the chat-model calls.
func WithParseTimeout(d time.Duration) Option { return func(c *Client) { c.parseTimeout = d } }

func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		http:           &http.Client{}, // deadlines come from the per-call context
		healthTimeout:  DefaultHealthTimeout,
		embedTimeout:   DefaultEmbedTimeout,
		extractTimeout: DefaultExtractTimeout,
		parseTimeout:   DefaultParseTimeout,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Health returns nil when the AI service answers /health with status "ok".
func (c *Client) Health(ctx context.Context) error {
	var out HealthResponse
	if err := c.do(ctx, "health", http.MethodGet, "/health", nil, c.healthTimeout, &out); err != nil {
		return err
	}
	if out.Status != "ok" {
		return &Error{Op: "health", StatusCode: http.StatusOK, cause: ErrUnavailable, Detail: "status " + out.Status}
	}
	return nil
}

// EmbedItem is one input of EmbedBatch: exactly one field is set. Profile
// and Requirements carry stored JSON (a CandidateProfile, a
// RoleRequirements) as it is, which the AI service renders to the canonical
// text it embeds; a document that is not valid for its model is a 422.
type EmbedItem struct {
	Text         string          `json:"text,omitempty"`
	Profile      json.RawMessage `json:"profile,omitempty"`
	Requirements json.RawMessage `json:"requirements,omitempty"`
}

// EmbedBatch asks the AI service for one embedding per item, in order. The
// response also says which text each vector was computed from.
func (c *Client) EmbedBatch(ctx context.Context, items []EmbedItem) (EmbedBatchResponse, error) {
	var out EmbedBatchResponse
	body := struct {
		Inputs []EmbedItem `json:"inputs"`
	}{items}
	if err := c.do(ctx, "embed-batch", http.MethodPost, "/embed-batch", body, c.embedTimeout, &out); err != nil {
		return EmbedBatchResponse{}, err
	}
	if len(out.Embeddings) != len(items) {
		return EmbedBatchResponse{}, &Error{Op: "embed-batch", StatusCode: http.StatusOK, cause: ErrBadResponse,
			Detail: fmt.Sprintf("%d embeddings for %d inputs", len(out.Embeddings), len(items))}
	}
	for _, v := range out.Embeddings {
		if len(v) == 0 {
			return EmbedBatchResponse{}, &Error{Op: "embed-batch", StatusCode: http.StatusOK, cause: ErrBadResponse, Detail: "empty embedding"}
		}
	}
	return out, nil
}

// Embed asks the AI service for an embedding of text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := c.Embedding(ctx, text)
	if err != nil {
		return nil, err
	}
	return out.Embedding, nil
}

// Embedding is Embed with the whole response, including which provider
// produced the vector (recorded in the embedding_model columns).
func (c *Client) Embedding(ctx context.Context, text string) (EmbedResponse, error) {
	var out EmbedResponse
	err := c.do(ctx, "embed", http.MethodPost, "/embed", EmbedRequest{Text: text}, c.embedTimeout, &out)
	if err != nil {
		return EmbedResponse{}, err
	}
	if len(out.Embedding) == 0 {
		return EmbedResponse{}, &Error{Op: "embed", StatusCode: http.StatusOK, cause: ErrBadResponse, Detail: "empty embedding"}
	}
	return out, nil
}

// ExtractText sends an uploaded file (a PDF or a DOCX, as the request body)
// and returns its text. The service decides what the file is from its
// content and enforces the upload limits; a file it will not read comes back
// as an *Error with its status (413, 415 or 422) and a Detail that says why.
func (c *Client) ExtractText(ctx context.Context, file []byte) (ExtractTextResponse, error) {
	var out ExtractTextResponse
	err := c.send(ctx, "extract-text", http.MethodPost, "/extract-text", "application/octet-stream", bytes.NewReader(file), c.extractTimeout, &out)
	if err != nil {
		return ExtractTextResponse{}, err
	}
	if strings.TrimSpace(out.Text) == "" {
		return ExtractTextResponse{}, &Error{Op: "extract-text", StatusCode: http.StatusOK, cause: ErrBadResponse, Detail: "empty text"}
	}
	return out, nil
}

// ParsedResume is the /parse-resume response. ProfileJSON is the profile
// exactly as the service sent it (what the API stores in JSONB, nulls and
// empty lists included); Profile is the same document decoded.
type ParsedResume struct {
	Contact     Contact
	Profile     CandidateProfile
	ProfileJSON json.RawMessage
	Provider    string
}

// ParseResume asks the AI service to extract a structured profile from a
// resume's text.
func (c *Client) ParseResume(ctx context.Context, text string) (ParsedResume, error) {
	var wire struct {
		Contact  Contact         `json:"contact"`
		Profile  json.RawMessage `json:"profile"`
		Provider string          `json:"provider"`
	}
	if err := c.do(ctx, "parse-resume", http.MethodPost, "/parse-resume", ParseResumeRequest{Text: text}, c.parseTimeout, &wire); err != nil {
		return ParsedResume{}, err
	}
	out := ParsedResume{Contact: wire.Contact, ProfileJSON: wire.Profile, Provider: wire.Provider}
	if err := json.Unmarshal(wire.Profile, &out.Profile); err != nil || !bytes.HasPrefix(bytes.TrimSpace(wire.Profile), []byte("{")) {
		return ParsedResume{}, &Error{Op: "parse-resume", StatusCode: http.StatusOK, cause: ErrBadResponse, Detail: "profile is not a CandidateProfile", wrapped: err}
	}
	return out, nil
}

// ParsedJD is the /parse-jd response. RequirementsJSON is the requirements
// exactly as the service sent them (what the API stores in JSONB);
// Requirements is the same document decoded.
type ParsedJD struct {
	Company          *string
	Requirements     RoleRequirements
	RequirementsJSON json.RawMessage
	Provider         string
}

// ParseJD asks the AI service to extract a role's structured requirements
// from a job description's text.
func (c *Client) ParseJD(ctx context.Context, text string) (ParsedJD, error) {
	var wire struct {
		Company      *string         `json:"company"`
		Requirements json.RawMessage `json:"requirements"`
		Provider     string          `json:"provider"`
	}
	if err := c.do(ctx, "parse-jd", http.MethodPost, "/parse-jd", ParseJDRequest{Text: text}, c.parseTimeout, &wire); err != nil {
		return ParsedJD{}, err
	}
	out := ParsedJD{Company: wire.Company, RequirementsJSON: wire.Requirements, Provider: wire.Provider}
	if err := json.Unmarshal(wire.Requirements, &out.Requirements); err != nil || !bytes.HasPrefix(bytes.TrimSpace(wire.Requirements), []byte("{")) {
		return ParsedJD{}, &Error{Op: "parse-jd", StatusCode: http.StatusOK, cause: ErrBadResponse, Detail: "requirements is not a RoleRequirements", wrapped: err}
	}
	return out, nil
}

// Rerank asks the AI service to score candidates against a role on the
// rerank rubric (docs/rerank-rubric.md). The response has every candidate
// exactly once, best first; one that does not is ErrBadResponse, so a caller
// never stores a ranking with somebody missing or invented. It is a
// chat-model call over every candidate's text, so it gets the parse deadline.
func (c *Client) Rerank(ctx context.Context, role string, candidates []RerankCandidate) (RerankResponse, error) {
	var out RerankResponse
	if err := c.do(ctx, "rerank", http.MethodPost, "/rerank", RerankRequest{Role: role, Candidates: candidates}, c.parseTimeout, &out); err != nil {
		return RerankResponse{}, err
	}
	sent := make(map[string]bool, len(candidates))
	for _, cand := range candidates {
		sent[cand.ID] = true
	}
	for _, r := range out.Results {
		if !sent[r.ID] {
			return RerankResponse{}, &Error{Op: "rerank", StatusCode: http.StatusOK, cause: ErrBadResponse,
				Detail: fmt.Sprintf("result for %q, which was not sent or is there twice", r.ID)}
		}
		delete(sent, r.ID)
		// matches.score takes 0 to 1; anything else would fail at the write,
		// as a database error, on every attempt.
		if !(r.Score >= 0 && r.Score <= 1) {
			return RerankResponse{}, &Error{Op: "rerank", StatusCode: http.StatusOK, cause: ErrBadResponse,
				Detail: fmt.Sprintf("score %v for %q is not between 0 and 1", r.Score, r.ID)}
		}
	}
	if len(sent) > 0 {
		return RerankResponse{}, &Error{Op: "rerank", StatusCode: http.StatusOK, cause: ErrBadResponse,
			Detail: fmt.Sprintf("%d results for %d candidates", len(out.Results), len(candidates))}
	}
	return out, nil
}

// do performs one JSON round trip. body is marshalled when non-nil; the
// response is decoded into out when non-nil.
func (c *Client) do(ctx context.Context, op, method, path string, body any, timeout time.Duration, out any) error {
	var reader io.Reader
	contentType := ""
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return &Error{Op: op, cause: ErrBadRequest, wrapped: err}
		}
		reader, contentType = bytes.NewReader(buf), "application/json"
	}
	return c.send(ctx, op, method, path, contentType, reader, timeout, out)
}

// send performs one round trip with a body that is already encoded (nil for
// none) and decodes the JSON response into out when non-nil.
func (c *Client) send(parent context.Context, op, method, path, contentType string, body io.Reader, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return &Error{Op: op, cause: ErrBadRequest, wrapped: err}
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
