// Package gohttpcl provides a robust and configurable HTTP client with support for retries,
// exponential backoff, jitter, circuit breaker, body buffering,
// dynamic rate limit adjustment, metrics, context-aware logging with golog, per-request timeouts, idempotency
// keys, response validation, and optional JSON response unmarshalling.
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
	"strconv"
	"sync"
	"time"

	"github.com/evdnx/golog"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// Option is a functional option for configuring the Client.
type Option func(*Client)

// MetricsCollector defines an interface for collecting metrics.
type MetricsCollector interface {
	IncRequests(method, url string)
	IncRetries(method, url string, attempt int)
	IncFailures(method, url string, statusCode int)
	ObserveLatency(method, url string, duration time.Duration)
}

// contextKey is a type for context keys to avoid collisions.
type contextKey string

// requestIDKey is the context key for request IDs.
const requestIDKey contextKey = "requestID"

// Client is a robust HTTP client that extends the standard http.Client with advanced features.
type Client struct {
	client                *http.Client
	maxRetries            int
	minBackoff            time.Duration
	maxBackoff            time.Duration
	backoffFactor         float64
	jitter                bool
	retryable             func(*http.Response, error) bool
	defaultHeaders        map[string]string
	limiter               *rate.Limiter
	limiterMu             sync.Mutex
	logger                *golog.Logger // Changed to golog.Logger
	circuitBreaker        *CircuitBreaker
	dynamicRateAdjustment bool
	rateWindow            time.Duration
	metrics               MetricsCollector
	maxBufferSize         int64
	rateLimitHeaderPrefix string
	idempotencyMethods    map[string]bool
	validateResponse      func(*http.Response) error
}

// New creates a new Client with the given options.
func New(opts ...Option) *Client {
	c := &Client{
		client:                &http.Client{},
		maxRetries:            3,
		minBackoff:            time.Second,
		maxBackoff:            30 * time.Second,
		backoffFactor:         2.0,
		jitter:                true,
		retryable:             defaultRetryable,
		defaultHeaders:        make(map[string]string),
		limiter:               nil,
		logger:                nil,
		circuitBreaker:        nil,
		dynamicRateAdjustment: false,
		rateWindow:            time.Minute,
		metrics:               nil,
		maxBufferSize:         10 * 1024 * 1024,
		rateLimitHeaderPrefix: "X-RateLimit",
		idempotencyMethods:    make(map[string]bool),
		validateResponse:      nil,
	}

	for _, opt := range opts {
		opt(c)
	}

	c.client.Transport = &retryTransport{
		transport: http.DefaultTransport,
		client:    c,
	}

	rand.Seed(time.Now().UnixNano())

	return c
}

// retryTransport is a custom RoundTripper that implements retry, rate limiting, circuit breaker, and body buffering.
type retryTransport struct {
	transport http.RoundTripper
	client    *Client
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	requestID := req.Context().Value(requestIDKey).(string)
	if t.client.metrics != nil {
		t.client.metrics.IncRequests(req.Method, req.URL.String())
	}

	if t.client.circuitBreaker != nil {
		if err := t.client.circuitBreaker.Allow(); err != nil {
			if t.client.logger != nil {
				t.client.logger.Error("circuit breaker open", golog.Error(err), golog.String("requestID", requestID))
			}
			if t.client.metrics != nil {
				t.client.metrics.IncFailures(req.Method, req.URL.String(), 0)
			}
			return nil, fmt.Errorf("circuit breaker: %w", err)
		}
	}

	if t.client.limiter != nil {
		t.client.limiterMu.Lock()
		err := t.client.limiter.Wait(req.Context())
		t.client.limiterMu.Unlock()
		if err != nil {
			if t.client.circuitBreaker != nil {
				t.client.circuitBreaker.RecordFailure()
			}
			if t.client.metrics != nil {
				t.client.metrics.IncFailures(req.Method, req.URL.String(), 0)
			}
			return nil, fmt.Errorf("rate limiter: %w", err)
		}
	}

	var bodyBytes []byte
	var originalBody io.ReadCloser
	if req.Body != nil {
		var err error
		if t.client.maxBufferSize > 0 {
			bodyBytes, err = io.ReadAll(io.LimitReader(req.Body, t.client.maxBufferSize))
			if err != nil {
				return nil, fmt.Errorf("buffering body: %w", err)
			}
			if int64(len(bodyBytes)) >= t.client.maxBufferSize {
				return nil, errors.New("request body exceeds max buffer size")
			}
		} else {
			bodyBytes, err = io.ReadAll(req.Body)
			if err != nil {
				return nil, fmt.Errorf("buffering body: %w", err)
			}
		}
		req.Body.Close()
		originalBody = io.NopCloser(bytes.NewReader(bodyBytes))
		req.Body = originalBody
	}

	var resp *http.Response
	var err error
	var retryAfter time.Duration

	for attempt := 0; attempt <= t.client.maxRetries; attempt++ {
		if attempt > 0 {
			if bodyBytes != nil {
				req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}

			wait := t.client.calculateBackoff(attempt-1, retryAfter)
			if t.client.logger != nil {
				t.client.logger.Info("retrying", golog.Duration("wait", wait), golog.Int("attempt", attempt), golog.String("requestID", requestID))
			}
			if t.client.metrics != nil {
				t.client.metrics.IncRetries(req.Method, req.URL.String(), attempt)
			}
			select {
			case <-time.After(wait):
			case <-req.Context().Done():
				if t.client.circuitBreaker != nil {
					t.client.circuitBreaker.RecordFailure()
				}
				if t.client.metrics != nil {
					t.client.metrics.IncFailures(req.Method, req.URL.String(), 0)
				}
				return nil, fmt.Errorf("context: %w", req.Context().Err())
			}
		}

		resp, err = t.transport.RoundTrip(req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if t.client.circuitBreaker != nil {
					t.client.circuitBreaker.RecordFailure()
				}
				if t.client.metrics != nil {
					t.client.metrics.IncFailures(req.Method, req.URL.String(), 0)
				}
				return nil, fmt.Errorf("transport: %w", err)
			}
		}

		retryAfter = parseRetryAfter(resp)

		if !t.client.retryable(resp, err) {
			break
		}

		if resp != nil && resp.Body != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	if t.client.dynamicRateAdjustment && t.client.limiter != nil && resp != nil {
		t.client.adjustRateLimiter(resp)
	}

	if t.client.validateResponse != nil && resp != nil {
		if err := t.client.validateResponse(resp); err != nil {
			if t.client.logger != nil {
				t.client.logger.Error("response validation failed", golog.Error(err), golog.String("requestID", requestID))
			}
			if t.client.circuitBreaker != nil {
				t.client.circuitBreaker.RecordFailure()
			}
			if t.client.metrics != nil {
				t.client.metrics.IncFailures(req.Method, req.URL.String(), resp.StatusCode)
			}
			return nil, fmt.Errorf("response validation: %w", err)
		}
	}

	if err != nil || (resp != nil && resp.StatusCode >= 400) {
		if t.client.circuitBreaker != nil {
			t.client.circuitBreaker.RecordFailure()
		}
		if t.client.metrics != nil {
			t.client.metrics.IncFailures(req.Method, req.URL.String(), resp.StatusCode)
		}
	} else {
		if t.client.circuitBreaker != nil {
			t.client.circuitBreaker.RecordSuccess()
		}
	}

	if t.client.metrics != nil {
		t.client.metrics.ObserveLatency(req.Method, req.URL.String(), time.Since(start))
	}

	return resp, err
}

func (c *Client) calculateBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}

	backoff := float64(c.minBackoff) * math.Pow(c.backoffFactor, float64(attempt))
	if backoff > float64(c.maxBackoff) {
		backoff = float64(c.maxBackoff)
	}

	if c.jitter {
		backoff = rand.Float64() * backoff
	}

	return time.Duration(backoff)
}

func defaultRetryable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 503
}

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		return 0
	}

	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil {
		return time.Duration(seconds) * time.Second
	}

	if t, err := http.ParseTime(retryAfter); err == nil {
		return time.Until(t)
	}

	return 0
}

func (c *Client) adjustRateLimiter(resp *http.Response) {
	remainingStr := resp.Header.Get(c.rateLimitHeaderPrefix + "-Remaining")
	resetStr := resp.Header.Get(c.rateLimitHeaderPrefix + "-Reset")
	limitStr := resp.Header.Get(c.rateLimitHeaderPrefix + "-Limit")

	if remainingStr == "" || resetStr == "" {
		return
	}

	remaining, err := strconv.ParseFloat(remainingStr, 64)
	if err != nil {
		return
	}

	resetUnix, err := strconv.ParseInt(resetStr, 10, 64)
	if err != nil {
		return
	}
	resetTime := time.Unix(resetUnix, 0)
	timeToReset := time.Until(resetTime)
	if timeToReset <= 0 {
		timeToReset = c.rateWindow
	}

	newRate := rate.Limit(remaining / timeToReset.Seconds())

	if limitStr != "" {
		burst, err := strconv.Atoi(limitStr)
		if err == nil {
			c.limiterMu.Lock()
			c.limiter.SetBurst(burst)
			c.limiterMu.Unlock()
		}
	}

	if newRate > 0 {
		c.limiterMu.Lock()
		c.limiter.SetLimit(newRate)
		c.limiterMu.Unlock()
		if c.logger != nil {
			c.logger.Info("adjusted rate limit", golog.Float64("rate", float64(newRate)), golog.String("requestID", resp.Request.Context().Value(requestIDKey).(string)))
		}
	} else if remaining == 0 {
		if c.logger != nil {
			c.logger.Info("rate limit exhausted", golog.Duration("wait", timeToReset), golog.String("requestID", resp.Request.Context().Value(requestIDKey).(string)))
		}
		time.Sleep(timeToReset)
	}
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	requestID := uuid.New().String()
	ctx := context.WithValue(req.Context(), requestIDKey, requestID)
	req = req.WithContext(ctx)

	if c.idempotencyMethods[req.Method] && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", requestID)
	}

	for k, v := range c.defaultHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	if c.logger != nil {
		c.logger.Debug("sending request", golog.String("method", req.Method), golog.String("url", req.URL.String()), golog.String("requestID", requestID))
	}
	return c.client.Do(req)
}

func (c *Client) Get(ctx context.Context, url string, timeout time.Duration, out interface{}) (*http.Response, error) {
	ctx, cancel := c.applyTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("unmarshalling response: %w", err)
		}
	}
	return resp, nil
}

func (c *Client) Post(ctx context.Context, url string, body io.Reader, timeout time.Duration, out interface{}) (*http.Response, error) {
	ctx, cancel := c.applyTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("unmarshalling response: %w", err)
		}
	}
	return resp, nil
}

func (c *Client) Put(ctx context.Context, url string, body io.Reader, timeout time.Duration, out interface{}) (*http.Response, error) {
	ctx, cancel := c.applyTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("unmarshalling response: %w", err)
		}
	}
	return resp, nil
}

func (c *Client) Delete(ctx context.Context, url string, timeout time.Duration, out interface{}) (*http.Response, error) {
	ctx, cancel := c.applyTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("unmarshalling response: %w", err)
		}
	}
	return resp, nil
}

func (c *Client) applyTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}

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

func NewCircuitBreaker(failureThreshold int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            stateClosed,
		failureThreshold: failureThreshold,
		resetTimeout:     resetTimeout,
	}
}

func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case stateOpen:
		if time.Since(cb.lastFailureTime) > cb.resetTimeout {
			cb.state = stateHalfOpen
			return nil
		}
		return errors.New("circuit open")
	case stateHalfOpen, stateClosed:
		return nil
	}
	return nil
}

func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures = 0
	cb.state = stateClosed
}

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
	return func(c *Client) { c.client.Transport = &retryTransport{transport: tr, client: c} }
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
