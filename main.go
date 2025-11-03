// Package gohttpcl provides a robust and configurable HTTP client with support for retries,
// exponential backoff, jitter, circuit breaker, body buffering,
// dynamic rate‑limit adjustment, metrics, context‑aware logging with golog,
// per‑request timeouts, idempotency keys, response validation, and optional JSON response unmarshalling.
package gohttpcl

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
	neturl "net/url"
	"strconv"
	"sync"
	"time"

	"github.com/evdnx/golog"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// -----------------------------------------------------------------------------
// Types & Interfaces
// -----------------------------------------------------------------------------

// Option configures a Client.
type Option func(*Client)

// MetricsCollector defines an interface for collecting metrics.
type MetricsCollector interface {
	IncRequests(method, url string)
	IncRetries(method, url string, attempt int)
	IncFailures(method, url string, statusCode int)
	ObserveLatency(method, url string, duration time.Duration)
}

// BackoffStrategy identifies the algorithm used to compute delays between retries.
type BackoffStrategy string

const (
	BackoffExponential BackoffStrategy = "exponential"
	BackoffLinear      BackoffStrategy = "linear"
	BackoffFibonacci   BackoffStrategy = "fibonacci"
	BackoffConstant    BackoffStrategy = "constant"
)

// contextKey is a private type for context values.
type contextKey string

// requestIDKey is the context key used to store a request ID.
const requestIDKey contextKey = "requestID"

// -----------------------------------------------------------------------------
// Client definition
// -----------------------------------------------------------------------------

// Client extends http.Client with advanced features such as retries, rate‑limiting,
// circuit breaking, dynamic rate adjustments, metrics, and structured logging.
type Client struct {
	client                *http.Client
	maxRetries            int
	minBackoff            time.Duration
	maxBackoff            time.Duration
	backoffFactor         float64
	backoffStrategy       BackoffStrategy
	jitter                bool
	retryable             func(*http.Response, error) bool
	defaultHeaders        map[string]string
	limiter               *rate.Limiter
	limiterMu             sync.Mutex
	logger                *golog.Logger
	circuitBreaker        *CircuitBreaker
	dynamicRateAdjustment bool
	rateWindow            time.Duration
	metrics               MetricsCollector
	retryBudget           *RetryBudget
	maxBufferSize         int64
	rateLimitHeaderPrefix string
	idempotencyMethods    map[string]bool
	validateResponse      func(*http.Response) error
}

// defaultRetryable determines whether a request should be retried.
// It retries on network errors and on HTTP status codes that typically indicate a transient problem.
func defaultRetryable(resp *http.Response, err error) bool {
	if err != nil {
		// Network‑level errors (including context cancellation) are considered retryable.
		return true
	}
	if resp == nil {
		return false
	}
	// Retry on server errors (5xx) and on rate‑limit responses.
	return resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 503
}

// ---------------------------------------------------------------------
// readAndDecode – reads the entire response body, unmarshals JSON into out,
// and always closes the body.
// ---------------------------------------------------------------------
func readAndDecode(resp *http.Response, out interface{}) error {
	if out == nil {
		// Caller doesn’t need the payload – just close.
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// Non‑2xx – still close the body, but don’t try to decode.
		resp.Body.Close()
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// ------------------------------------------------------------
// Per‑request functional options
// ------------------------------------------------------------

// ReqOption configures a single http.Request before it is sent.
type ReqOption func(*http.Request)

// WithHeader returns a ReqOption that sets (or overwrites) a header
// on the outgoing request.  Multiple calls add/override independently.
func WithHeader(key, value string) ReqOption {
	return func(r *http.Request) {
		r.Header.Set(key, value)
	}
}

// WithHeaders returns a ReqOption that copies every entry from the
// supplied http.Header (preserving multi‑value semantics) onto the request.
func WithHeaders(h http.Header) ReqOption {
	return func(r *http.Request) {
		for k, vv := range h {
			for _, v := range vv {
				r.Header.Add(k, v)
			}
		}
	}
}

// applyReqOptions merges the client‑wide default headers with any
// per‑request options supplied by the caller.
func (c *Client) applyReqOptions(req *http.Request, opts []ReqOption) {
	// 1️⃣ Apply client‑wide defaults first.
	for k, v := range c.defaultHeaders {
		// Only set if the caller hasn’t overridden it later.
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	// 2️⃣ Apply each functional option (they may add/override headers,
	//    inject tracing IDs, etc.).
	for _, opt := range opts {
		opt(req)
	}
}

// New creates a new Client applying any supplied Options.
func New(opts ...Option) *Client {
	c := &Client{
		client:                &http.Client{},
		maxRetries:            3,
		minBackoff:            time.Second,
		maxBackoff:            30 * time.Second,
		backoffFactor:         2.0,
		backoffStrategy:       BackoffExponential,
		jitter:                true,
		retryable:             defaultRetryable,
		defaultHeaders:        make(map[string]string),
		limiter:               nil,
		logger:                nil,
		circuitBreaker:        nil,
		dynamicRateAdjustment: false,
		rateWindow:            time.Minute,
		metrics:               nil,
		maxBufferSize:         10 * 1024 * 1024, // 10 MiB
		rateLimitHeaderPrefix: "X-RateLimit",
		idempotencyMethods:    make(map[string]bool),
		validateResponse:      nil,
	}

	for _, opt := range opts {
		opt(c)
	}

	switch rt := c.client.Transport.(type) {
	case *retryTransport:
		// Option provided a fully configured retry transport; make sure it points back to us.
		rt.client = c
		if rt.transport == nil {
			rt.transport = http.DefaultTransport
		}
	default:
		base := c.client.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		c.client.Transport = &retryTransport{
			transport: base,
			client:    c,
		}
	}

	// Seed the jitter generator once per process.
	rand.Seed(time.Now().UnixNano())

	return c
}

// -----------------------------------------------------------------------------
// Transport implementation (retry, rate‑limit, circuit breaker, body buffering)
// -----------------------------------------------------------------------------

type retryTransport struct {
	transport http.RoundTripper
	client    *Client
}

// -----------------------------------------------------------------------------
// Transport implementation (retry, rate‑limit, circuit breaker, body buffering)
// -----------------------------------------------------------------------------

// RoundTrip executes a single HTTP request, applying all of the client‑wide
// policies (circuit‑breaker, rate‑limit, retries, …) and finally returns the
// response to the caller.
func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c := rt.client

	// -----------------------------------------------------------------
	// 0️⃣  Circuit‑breaker pre‑check – reject request early if the CB is open.
	// -----------------------------------------------------------------
	if c.circuitBreaker != nil {
		if err := c.circuitBreaker.Allow(); err != nil {
			return nil, err // “circuit open”
		}
	}

	// -----------------------------------------------------------------
	// 1️⃣  Rate‑limiting – wait for a token before proceeding.
	// -----------------------------------------------------------------
	if c.limiter != nil {
		if err := c.limiter.Wait(req.Context()); err != nil {
			return nil, err
		}
	}

	// -----------------------------------------------------------------
	// 2️⃣  Prepare the request body for possible retries.
	//
	//     • If a max‑buffer size is configured we verify the body does not
	//       exceed it (and obtain a rewindable reader when possible).
	//     • The body is also buffered so that retries can resend the exact
	//       same payload.
	// -----------------------------------------------------------------
	var bodyCopy io.ReadSeeker
	if req.Body != nil {
		// Check size & obtain a seekable copy.
		b, err := bufferRequestBody(req, c.maxBufferSize)
		if err != nil {
			return nil, err
		}
		if b != nil {
			// Keep a copy for retries. bytes.NewReader tolerates zero-length slices.
			bodyCopy = bytes.NewReader(b)
		}
	}

	// -----------------------------------------------------------------
	// 3️⃣  Execute the request with retry logic.
	// -----------------------------------------------------------------
	var (
		resp       *http.Response
		err        error
		attempt    int
		start      = time.Now()
		retryAfter time.Duration
	)

	c.retryBudget.RecordRequest()

	for {
		// If we have a buffered body, rewind it before each attempt.
		if bodyCopy != nil {
			if _, err = bodyCopy.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("rewinding request body: %w", err)
			}
			req.Body = io.NopCloser(bodyCopy)
		}

		// Perform the actual HTTP request.
		resp, err = rt.transport.RoundTrip(req)

		// -----------------------------------------------------------------
		// 3a️⃣  Determine whether we should retry.
		// -----------------------------------------------------------------
		shouldRetry := false
		if err != nil {
			// Transport‑level error (e.g., network failure).
			shouldRetry = c.retryable(nil, err)
		} else {
			// HTTP response received – consult retry predicate.
			shouldRetry = c.retryable(resp, nil)
			// Respect “Retry‑After” header for 429/503 etc.
			if shouldRetry && resp != nil {
				retryAfter = parseRetryAfter(resp)
			}
		}

		// -----------------------------------------------------------------
		// 3b️⃣  Exit loop if no retry is needed or max attempts exceeded.
		// -----------------------------------------------------------------
		if !shouldRetry || attempt >= c.maxRetries {
			break
		}
		attempt++

		if !c.retryBudget.AllowRetry() {
			break
		}
		c.retryBudget.RecordRetry()

		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if c.metrics != nil {
			c.metrics.IncRetries(req.Method, req.URL.String(), attempt)
		}

		// -----------------------------------------------------------------
		// 3c️⃣  Back‑off before the next attempt.
		// -----------------------------------------------------------------
		backoff := c.calculateBackoff(attempt, retryAfter)
		retryAfter = 0
		select {
		case <-time.After(backoff):
			// continue to next attempt
		case <-req.Context().Done():
			// request cancelled / timed‑out while waiting
			return nil, req.Context().Err()
		}
	}

	// -----------------------------------------------------------------
	// 4️⃣  Post‑response housekeeping – this block **must run
	//     unconditionally** because it restores the response body for the
	//     caller and updates rate‑limit / circuit‑breaker state.
	// -----------------------------------------------------------------
	if resp != nil {
		// -------------------------------------------------------------
		// 4a️⃣ Preserve the body for the caller.
		//
		// The transport may have already consumed the body (e.g. when
		// retries were performed). We read the entire payload, close the
		// original body, and replace it with a fresh NopCloser backed by a
		// byte slice so that the caller can read it exactly once.
		// -------------------------------------------------------------
		if resp.Body != nil {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				// Record the failure (if metrics are enabled) and surface the error.
				if c.metrics != nil {
					c.metrics.IncFailures(req.Method, req.URL.String(), 0)
				}
				return nil, fmt.Errorf("reading final response body: %w", readErr)
			}
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		// -------------------------------------------------------------
		// 4b️⃣ Dynamic rate‑limit adjustment (if enabled).
		// -------------------------------------------------------------
		if c.dynamicRateAdjustment {
			c.adjustRateLimiter(resp)
		}

		// -------------------------------------------------------------
		// 4c️⃣ Circuit‑breaker state update.
		// -------------------------------------------------------------
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			if c.circuitBreaker != nil {
				c.circuitBreaker.RecordSuccess()
			}
		} else {
			recordFailure(c, req, resp.StatusCode)
		}
	}

	// -----------------------------------------------------------------
	// 5️⃣  Metrics collection – this is optional and now runs **after**
	//     the body has been restored, so the caller always receives a
	//     readable response.
	// -----------------------------------------------------------------
	if c.metrics != nil {
		latency := time.Since(start)
		c.metrics.ObserveLatency(req.Method, req.URL.String(), latency)
	}

	// -----------------------------------------------------------------
	// 6️⃣  Return the (potentially retried) response and any error.
	// -----------------------------------------------------------------
	return resp, err
}

// -----------------------------------------------------------------------------
// Helper functions used by the transport
// -----------------------------------------------------------------------------

func getRequestID(ctx context.Context) string {
	if v := ctx.Value(requestIDKey); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	// Fallback – generate a temporary ID so logs never crash.
	return uuid.New().String()
}

func logInfo(l *golog.Logger, msg string, requestID string, fields ...golog.Field) {
	if l != nil {
		l.Info(msg, append([]golog.Field{golog.String("requestID", requestID)}, fields...)...)
	}
}

func recordFailure(c *Client, req *http.Request, statusCode int) {
	if c.circuitBreaker != nil {
		c.circuitBreaker.RecordFailure()
	}
	if c.metrics != nil {
		c.metrics.IncFailures(req.Method, req.URL.String(), statusCode)
	}
}

// bufferRequestBody reads the request body up to maxSize.
// It returns the raw bytes (so the body can be re‑used) or an error.
func bufferRequestBody(req *http.Request, maxSize int64) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	original := req.Body
	defer func() {
		if original != nil {
			_ = original.Close()
		}
	}()

	var (
		bodyBytes []byte
		err       error
	)

	if maxSize > 0 {
		limit := maxSize
		checkOverflow := true
		if maxSize < math.MaxInt64 {
			limit++
		} else {
			checkOverflow = false
		}
		bodyBytes, err = io.ReadAll(io.LimitReader(original, limit))
		if err != nil {
			return nil, fmt.Errorf("buffering body: %w", err)
		}
		if checkOverflow && int64(len(bodyBytes)) > maxSize {
			return nil, errors.New("request body exceeds max buffer size")
		}
	} else {
		bodyBytes, err = io.ReadAll(original)
		if err != nil {
			return nil, fmt.Errorf("buffering body: %w", err)
		}
	}

	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes, nil
}

// -----------------------------------------------------------------------------
// Back‑off calculation
// -----------------------------------------------------------------------------

func (c *Client) calculateBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	var base float64
	switch c.backoffStrategy {
	case BackoffConstant:
		base = float64(c.minBackoff)
	case BackoffLinear:
		multiplier := float64(attempt + 1)
		base = float64(c.minBackoff) * multiplier
	case BackoffFibonacci:
		n := attempt + 1
		if n < 1 {
			n = 1
		}
		base = float64(c.minBackoff) * float64(fibonacciNumber(n))
	default: // BackoffExponential (also default for unknown values)
		base = float64(c.minBackoff) * math.Pow(c.backoffFactor, float64(attempt))
	}
	if base > float64(c.maxBackoff) {
		base = float64(c.maxBackoff)
	}
	if c.jitter {
		base = rand.Float64() * base
	}
	return time.Duration(base)
}

// -----------------------------------------------------------------------------
// Retry‑after parsing
// -----------------------------------------------------------------------------

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return 0
	}
	// Seconds?
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil {
		return time.Duration(secs) * time.Second
	}
	// HTTP-date?
	if t, err := http.ParseTime(h); err == nil {
		return time.Until(t)
	}
	return 0
}

func fibonacciNumber(n int) int {
	if n <= 1 {
		return 1
	}
	a, b := 1, 1
	for i := 2; i < n; i++ {
		a, b = b, a+b
	}
	return b
}

// RetryBudget bounds the ratio of retries to requests over a sliding window.
type RetryBudget struct {
	maxRatio      float64
	resetInterval time.Duration

	mu        sync.Mutex
	requests  int64
	retries   int64
	lastReset time.Time
}

// NewRetryBudget constructs a retry budget. Ratios outside (0,1] are clamped,
// and a zero/negative reset interval falls back to one hour.
func NewRetryBudget(maxRatio float64, resetInterval time.Duration) *RetryBudget {
	switch {
	case maxRatio > 1:
		maxRatio = 1
	case maxRatio <= 0:
		maxRatio = 0.2
	}
	if resetInterval <= 0 {
		resetInterval = time.Hour
	}
	return &RetryBudget{
		maxRatio:      maxRatio,
		resetInterval: resetInterval,
		lastReset:     time.Now(),
	}
}

// RecordRequest increments the request counter.
func (b *RetryBudget) RecordRequest() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNeeded()
	b.requests++
}

// AllowRetry reports whether issuing another retry would keep the ratio within bounds.
func (b *RetryBudget) AllowRetry() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNeeded()
	if b.requests == 0 {
		return true
	}
	return float64(b.retries)/float64(b.requests) < b.maxRatio
}

// RecordRetry increments the retry counter.
func (b *RetryBudget) RecordRetry() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNeeded()
	b.retries++
}

func (b *RetryBudget) resetIfNeeded() {
	if time.Since(b.lastReset) >= b.resetInterval {
		b.requests = 0
		b.retries = 0
		b.lastReset = time.Now()
	}
}

// -----------------------------------------------------------------------------
// Dynamic rate‑limit adjustment
// -----------------------------------------------------------------------------

func (c *Client) adjustRateLimiter(resp *http.Response) {
	if !c.dynamicRateAdjustment || c.limiter == nil {
		return
	}
	remainingStr := resp.Header.Get(c.rateLimitHeaderPrefix + "-Remaining")
	resetStr := resp.Header.Get(c.rateLimitHeaderPrefix + "-Reset")
	limitStr := resp.Header.Get(c.rateLimitHeaderPrefix + "-Limit")

	if remainingStr == "" && limitStr == "" {
		return
	}
	var (
		remaining    float64
		remainingSet bool
	)
	if remainingStr != "" {
		val, err := strconv.ParseFloat(remainingStr, 64)
		if err != nil {
			return
		}
		remaining = val
		remainingSet = true
	}

	var (
		limit    float64
		limitSet bool
	)
	if limitStr != "" {
		val, err := strconv.ParseFloat(limitStr, 64)
		if err == nil {
			limit = val
			limitSet = true
		}
	}

	window := c.rateWindow
	if window <= 0 {
		window = time.Second
	}
	now := time.Now()
	if resetStr != "" {
		if dur, ok := parseResetWindow(resetStr, now); ok && dur > 0 {
			window = dur
		}
	}
	if window <= 0 {
		window = time.Second
	}

	rateValue := 0.0
	if remainingSet && remaining > 0 {
		rateValue = remaining / window.Seconds()
	} else if limitSet && limit > 0 {
		rateValue = limit / window.Seconds()
	}
	if rateValue <= 0 {
		return
	}

	newRate := rate.Limit(rateValue)

	var newBurst int
	if limitStr != "" {
		if burst, err := strconv.Atoi(limitStr); err == nil {
			newBurst = burst
		}
	}

	requestID := ""
	if resp.Request != nil {
		requestID = getRequestID(resp.Request.Context())
	}

	c.limiterMu.Lock()
	if newBurst > 0 {
		c.limiter.SetBurst(newBurst)
	}
	if newRate > 0 {
		c.limiter.SetLimit(newRate)
	}
	c.limiterMu.Unlock()

	if newRate > 0 {
		logInfo(c.logger, "adjusted rate limit", requestID,
			golog.Float64("rate", float64(newRate)))
	}
}

func parseResetWindow(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if secs > 0 && secs < 1e9 {
			return time.Duration(secs * float64(time.Second)), true
		}
		if secs >= 1e9 {
			secPart, frac := math.Modf(secs)
			resetTime := time.Unix(int64(secPart), int64(frac*float64(time.Second)))
			if resetTime.After(now) {
				return resetTime.Sub(now), true
			}
		}
	}
	if ts, err := http.ParseTime(value); err == nil && ts.After(now) {
		return ts.Sub(now), true
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// Public API (Do, Get, Post, Put, Delete, etc.)
// -----------------------------------------------------------------------------

// Do sends an HTTP request, applying default headers, idempotency keys,
// circuit‑breaker checks, rate‑limiting, and finally returns the response
// with a reusable body.
//
// All higher‑level helpers (`Get`, `Post`, `Put`, `Delete`) rely on this
// method, so we make sure the body is not consumed here.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// -----------------------------------------------------------------
	// 0️⃣ Apply default headers (if any)
	// -----------------------------------------------------------------
	for k, v := range c.defaultHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	// -----------------------------------------------------------------
	// 1️⃣ Idempotency key handling (only for configured methods)
	// -----------------------------------------------------------------
	if c.idempotencyMethods[req.Method] {
		if req.Header.Get("Idempotency-Key") == "" {
			req.Header.Set("Idempotency-Key", getRequestID(req.Context()))
		}
	}

	// -----------------------------------------------------------------
	// 2️⃣ Execute the underlying http.Client request (the heavy lifting
	//    – retries, rate‑limit, circuit‑breaker – lives in the transport).
	// -----------------------------------------------------------------
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------
	// 3️⃣ Optional logging / metrics can be added here without touching
	//    the body again.
	// -----------------------------------------------------------------
	if c.logger != nil {
		logInfo(c.logger, "request completed", getRequestID(req.Context()),
			golog.String("method", req.Method),
			golog.String("url", req.URL.String()),
			golog.Int("status", resp.StatusCode))
	}
	if c.metrics != nil {
		c.metrics.IncRequests(req.Method, req.URL.String())
	}

	return resp, nil
}

// applyTimeout returns a derived context that enforces the supplied timeout.
// If `timeout` is zero or negative, the original parent context is returned unchanged.
// The returned `CancelFunc` must be called by the caller (usually via `defer`) to
// release resources associated with the derived context.
//
// This helper centralises timeout handling for all request‑making methods
// (Get, Post, Put, Delete, etc.) so that each method can simply call:
//
//	ctx, cancel := c.applyTimeout(ctx, timeout)
//	defer cancel()
func (c *Client) applyTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		// Create a child context that will be cancelled automatically after the
		// specified duration. The caller is responsible for invoking the
		// returned CancelFunc to free resources early (e.g., when the request
		// finishes before the deadline).
		return context.WithTimeout(parent, timeout)
	}
	// No timeout requested – return the original context and a no‑op cancel
	// function so callers can still `defer cancel()` without special‑casing.
	return parent, func() {}
}

// Get performs an HTTP GET with optional per‑request options.
func (c *Client) Get(
	ctx context.Context,
	url string,
	timeout time.Duration,
	out interface{},
	opts ...ReqOption,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.applyReqOptions(req, opts)

	// Apply per‑request timeout.
	ctx, cancel := c.applyTimeout(req.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.Do(req)
	if err != nil {
		// Preserve original behaviour for url.Error unwrapping.
		if ue, ok := err.(*neturl.Error); ok {
			if ue.Err != nil {
				return nil, ue.Err
			}
			return nil, ue
		}
		return nil, err
	}
	if out != nil {
		if err = readAndDecode(resp, out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// Post performs an HTTP POST with optional per‑request options.
func (c *Client) Post(
	ctx context.Context,
	url string,
	body io.Reader,
	timeout time.Duration,
	out interface{},
	opts ...ReqOption,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	c.applyReqOptions(req, opts)

	ctx, cancel := c.applyTimeout(req.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.Do(req)
	if err != nil {
		if ue, ok := err.(*neturl.Error); ok {
			if ue.Err != nil {
				return nil, ue.Err
			}
			return nil, ue
		}
		return nil, err
	}
	if out != nil {
		if err = readAndDecode(resp, out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// Put performs an HTTP PUT with optional per‑request options.
func (c *Client) Put(
	ctx context.Context,
	url string,
	body io.Reader,
	timeout time.Duration,
	out interface{},
	opts ...ReqOption,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return nil, err
	}
	c.applyReqOptions(req, opts)

	ctx, cancel := c.applyTimeout(req.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.Do(req)
	if err != nil {
		if ue, ok := err.(*neturl.Error); ok {
			if ue.Err != nil {
				return nil, ue.Err
			}
			return nil, ue
		}
		return nil, err
	}
	if out != nil {
		if err = readAndDecode(resp, out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// Delete performs an HTTP DELETE with optional per‑request options.
func (c *Client) Delete(
	ctx context.Context,
	url string,
	timeout time.Duration,
	out interface{},
	opts ...ReqOption,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	c.applyReqOptions(req, opts)

	ctx, cancel := c.applyTimeout(req.Context(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.Do(req)
	if err != nil {
		if ue, ok := err.(*neturl.Error); ok {
			if ue.Err != nil {
				return nil, ue.Err
			}
			return nil, ue
		}
		return nil, err
	}
	if out != nil {
		if err = readAndDecode(resp, out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// -----------------------------------------------------------------------------
// Circuit breaker implementation
// -----------------------------------------------------------------------------

type CircuitBreaker struct {
	mu                  sync.Mutex
	state               string
	failureThreshold    int
	resetTimeout        time.Duration
	consecutiveFailures int
	lastFailureTime     time.Time
}

const (
	stateClosed   = "closed"
	stateOpen     = "open"
	stateHalfOpen = "half-open"
)

// NewCircuitBreaker creates a circuit breaker that opens after failureThreshold
// consecutive failures and stays open for resetTimeout before transitioning to half‑open.
func NewCircuitBreaker(failureThreshold int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            stateClosed,
		failureThreshold: failureThreshold,
		resetTimeout:     resetTimeout,
	}
}

// Allow returns nil if a request may proceed, otherwise an error indicating the circuit is open.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateOpen:
		if time.Since(cb.lastFailureTime) > cb.resetTimeout {
			cb.state = stateHalfOpen
			return nil
		}
		return errors.New("circuit breaker: circuit open")
	case stateHalfOpen, stateClosed:
		return nil
	}
	return nil
}

// RecordSuccess resets the failure counter and closes the circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFailures = 0
	cb.state = stateClosed
}

// RecordFailure increments the failure counter and opens the circuit if the threshold is reached.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures++
	cb.lastFailureTime = time.Now()

	if cb.consecutiveFailures >= cb.failureThreshold {
		cb.state = stateOpen
	} else if cb.state == stateHalfOpen {
		cb.state = stateOpen
	}
}

// -----------------------------------------------------------------------------
// Functional options – configuration helpers
// -----------------------------------------------------------------------------

func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

func WithMinBackoff(d time.Duration) Option {
	return func(c *Client) { c.minBackoff = d }
}

func WithMaxBackoff(d time.Duration) Option {
	return func(c *Client) { c.maxBackoff = d }
}

func WithBackoffFactor(f float64) Option {
	return func(c *Client) { c.backoffFactor = f }
}

// WithBackoffStrategy selects the algorithm used to compute retry delays.
func WithBackoffStrategy(strategy BackoffStrategy) Option {
	return func(c *Client) {
		switch strategy {
		case BackoffConstant, BackoffLinear, BackoffFibonacci, BackoffExponential:
			c.backoffStrategy = strategy
		default:
			// Ignore unsupported values to leave the existing configuration in place.
		}
	}
}

// WithRetryBudget enforces a retry-to-request ratio across a rolling time window.
// Supplying a zero or negative ratio disables the budget.
func WithRetryBudget(maxRatio float64, window time.Duration) Option {
	return func(c *Client) {
		if maxRatio <= 0 {
			c.retryBudget = nil
			return
		}
		c.retryBudget = NewRetryBudget(maxRatio, window)
	}
}

func WithJitter(b bool) Option {
	return func(c *Client) { c.jitter = b }
}

func WithRetryable(f func(*http.Response, error) bool) Option {
	return func(c *Client) { c.retryable = f }
}

func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.client.Timeout = d }
}

func WithTransport(tr http.RoundTripper) Option {
	return func(c *Client) {
		if tr == nil {
			tr = http.DefaultTransport
		}
		c.client.Transport = tr
	}
}

func WithDefaultHeader(key, value string) Option {
	return func(c *Client) { c.defaultHeaders[key] = value }
}

func WithDefaultHeaders(headers map[string]string) Option {
	return func(c *Client) {
		for k, v := range headers {
			c.defaultHeaders[k] = v
		}
	}
}

func WithRateLimit(limit float64, burst int) Option {
	return func(c *Client) {
		c.limiter = rate.NewLimiter(rate.Limit(limit), burst)
	}
}

func WithGologLogger(logger *golog.Logger) Option {
	return func(c *Client) { c.logger = logger }
}

func WithCircuitBreaker(failureThreshold int, resetTimeout time.Duration) Option {
	return func(c *Client) {
		c.circuitBreaker = NewCircuitBreaker(failureThreshold, resetTimeout)
	}
}

func WithDynamicRateAdjustment() Option {
	return func(c *Client) { c.dynamicRateAdjustment = true }
}

func WithRateWindow(d time.Duration) Option {
	return func(c *Client) { c.rateWindow = d }
}

func WithMetrics(metrics MetricsCollector) Option {
	return func(c *Client) { c.metrics = metrics }
}

func WithMaxBufferSize(size int64) Option {
	return func(c *Client) { c.maxBufferSize = size }
}

func WithRateLimitHeaderPrefix(prefix string) Option {
	return func(c *Client) { c.rateLimitHeaderPrefix = prefix }
}

func WithIdempotencyMethods(methods ...string) Option {
	return func(c *Client) {
		for _, m := range methods {
			c.idempotencyMethods[m] = true
		}
	}
}

func WithResponseValidation(validate func(*http.Response) error) Option {
	return func(c *Client) { c.validateResponse = validate }
}
