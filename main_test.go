package gohttpcl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

/*
Helper -----------------------------------------------------------------------
*/

type testServer struct {
	handler http.HandlerFunc
	srv     *httptest.Server
	calls   int32
}

func newTestServer(h http.HandlerFunc) *testServer {
	ts := &testServer{handler: h}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ts.calls, 1)
		h(w, r)
	}))
	return ts
}

func (ts *testServer) URL() string { return ts.srv.URL }
func (ts *testServer) Calls() int  { return int(atomic.LoadInt32(&ts.calls)) }
func (ts *testServer) Close()      { ts.srv.Close() }

/*
Tests ------------------------------------------------------------------------
*/

// TestDefaultRetryable verifies the built‑in retry predicate behaves as expected.
func TestDefaultRetryable(t *testing.T) {
	// Network error → retry
	if !defaultRetryable(nil, errors.New("net error")) {
		t.Fatalf("expected network error to be retryable")
	}
	// Nil response & no error → not retryable
	if defaultRetryable(nil, nil) {
		t.Fatalf("expected nil response/no error to be non‑retryable")
	}
	// 5xx → retry
	resp := &http.Response{StatusCode: 502}
	if !defaultRetryable(resp, nil) {
		t.Fatalf("expected 5xx to be retryable")
	}
	// 429 → retry
	resp.StatusCode = 429
	if !defaultRetryable(resp, nil) {
		t.Fatalf("expected 429 to be retryable")
	}
	// 200 → no retry
	resp.StatusCode = 200
	if defaultRetryable(resp, nil) {
		t.Fatalf("expected 200 to be non‑retryable")
	}
}

// TestBackoffCalculation checks exponential back‑off with jitter disabled.
func TestBackoffCalculation(t *testing.T) {
	c := New()
	c.jitter = false // deterministic for the test

	tests := []struct {
		attempt   int
		wantDelay time.Duration
	}{
		{0, c.minBackoff},
		{1, time.Duration(float64(c.minBackoff) * c.backoffFactor)},
		{2, time.Duration(float64(c.minBackoff) * c.backoffFactor * c.backoffFactor)},
	}
	for _, tt := range tests {
		got := c.calculateBackoff(tt.attempt, 0)
		if got != tt.wantDelay {
			t.Fatalf("attempt %d: want %v, got %v", tt.attempt, tt.wantDelay, got)
		}
	}
}

// TestRetryOnTransientError ensures the client retries the configured number of times.
func TestRetryOnTransientError(t *testing.T) {
	var attempts int32
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		// First two calls return 502, third succeeds.
		if atomic.LoadInt32(&attempts) < 2 {
			w.WriteHeader(http.StatusBadGateway) // 502
			return
		}
		w.Header().Set("Content-Type", "application/json") // Add Content-Type
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"msg":"ok"}`))
	})
	defer ts.Close()

	c := New(WithMaxRetries(3))
	var out struct {
		Msg string `json:"msg"` // JSON tag already added
	}
	_, err := c.Get(context.Background(), ts.URL(), 0, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Msg != "ok" {
		t.Fatalf("unexpected payload: %+v", out)
	}
	if ts.Calls() != 3 {
		t.Fatalf("expected 3 attempts, got %d", ts.Calls())
	}
}

// TestCircuitBreakerOpensAfterThreshold validates state transition.
func TestCircuitBreakerOpensAfterThreshold(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500
	})
	defer ts.Close()

	cbThreshold := 2
	c := New(
		WithCircuitBreaker(cbThreshold, time.Millisecond*100),
		WithMaxRetries(0), // we rely on the CB, not client retries
	)

	// First request → failure, circuit stays closed (first failure)
	_, _ = c.Get(context.Background(), ts.URL(), 0, nil)
	if c.circuitBreaker.state != stateClosed {
		t.Fatalf("expected circuit CLOSED after first failure, got %s", c.circuitBreaker.state)
	}

	// Second request → second failure, circuit should OPEN
	_, _ = c.Get(context.Background(), ts.URL(), 0, nil)
	if c.circuitBreaker.state != stateOpen {
		t.Fatalf("expected circuit OPEN after reaching threshold, got %s", c.circuitBreaker.state)
	}

	// Third request → should be blocked by the open circuit
	_, err := c.Get(context.Background(), ts.URL(), 0, nil)
	if err == nil || err.Error() != "circuit breaker: circuit open" {
		t.Fatalf("expected circuit‑open error, got %v", err)
	}
}

// TestDynamicRateAdjustment adjusts limiter based on response headers.
func TestDynamicRateAdjustment(t *testing.T) {
	// Simulate a server that reports 2 remaining requests, resetting in 1 second,
	// and a limit (burst) of 5.
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		w.Header().Set("X-RateLimit-Remaining", "2")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now+1, 10))
		w.Header().Set("X-RateLimit-Limit", "5")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	c := New(
		WithRateLimit(10, 10), // start with generous limit
		WithDynamicRateAdjustment(),
	)

	// First request triggers adjustment.
	_, err := c.Get(context.Background(), ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After adjustment the limiter's limit should be roughly 2 requests per second.
	expected := rate.Limit(2) // 2 remaining / ~1 sec
	if c.limiter.Limit() != expected {
		t.Fatalf("expected limiter limit %v, got %v", expected, c.limiter.Limit())
	}
}

// TestIdempotencyKeyIsAddedWhenConfigured ensures the header appears.
func TestIdempotencyKeyIsAddedWhenConfigured(t *testing.T) {
	var receivedKey string
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		receivedKey = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	c := New(
		WithIdempotencyMethods(http.MethodPost),
	)

	_, err := c.Post(context.Background(), ts.URL(), nil, 0, nil)
	if err != nil {
		t.Fatalf("post error: %v", err)
	}
	if receivedKey == "" {
		t.Fatalf("expected Idempotency-Key header to be set")
	}
}

// TestApplyTimeoutNoTimeout verifies that a zero timeout returns the original context.
func TestApplyTimeoutNoTimeout(t *testing.T) {
	c := New()
	ctx, cancel := c.applyTimeout(context.Background(), 0)
	defer cancel()
	if ctx != context.Background() {
		t.Fatalf("expected original context when timeout <= 0")
	}
}

// TestApplyTimeoutWithTimeout ensures the derived context respects the deadline.
func TestApplyTimeoutWithTimeout(t *testing.T) {
	c := New()
	ctx, cancel := c.applyTimeout(context.Background(), time.Millisecond*50)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected deadline to be set")
	}
	if time.Until(deadline) > time.Millisecond*60 {
		t.Fatalf("deadline too far in the future")
	}
}

// TestJSONDecoding verifies that Get/Post correctly decode JSON bodies.
func TestJSONDecoding(t *testing.T) {
	type payload struct {
		Value string `json:"value"`
	}
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload{Value: "hello"})
	})
	defer ts.Close()

	c := New()
	var out payload
	_, err := c.Get(context.Background(), ts.URL(), 0, &out)
	if err != nil {
		t.Fatalf("get error: %v", err)
	}
	if out.Value != "hello" {
		t.Fatalf("unexpected decoded value: %s", out.Value)
	}
}

// TestBufferOverflow ensures the client returns an error when the request body exceeds maxBufferSize.
func TestBufferOverflow(t *testing.T) {
	// Create a body larger than the default 10 MiB.
	largeBody := make([]byte, 11*1024*1024)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	c := New()
	_, err := c.Post(context.Background(), ts.URL(), bytes.NewReader(largeBody), 0, nil)
	if err == nil || err.Error() != "request body exceeds max buffer size" {
		t.Fatalf("expected buffer‑size error, got %v", err)
	}
}
