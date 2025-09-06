package gohttpcl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// ---------------------------------------------------------------------
// Existing tests (unchanged) – kept for reference
// ---------------------------------------------------------------------

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

func TestRetryOnTransientError(t *testing.T) {
	var attempts int32
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&attempts, 1)
		if cur <= 2 {
			w.WriteHeader(http.StatusBadGateway) // 502
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"msg":"ok"}`))
	})
	defer ts.Close()

	c := New(WithMaxRetries(3))
	var out struct {
		Msg string `json:"msg"`
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

func TestCircuitBreakerOpensAfterThreshold(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500
	})
	defer ts.Close()

	cbThreshold := 2
	c := New(
		WithCircuitBreaker(cbThreshold, time.Millisecond*100),
		WithMaxRetries(0), // rely on CB only
	)

	// First request → failure, circuit stays closed
	_, _ = c.Get(context.Background(), ts.URL(), 0, nil)
	if c.circuitBreaker.state != stateClosed {
		t.Fatalf("expected circuit CLOSED after first failure, got %s", c.circuitBreaker.state)
	}

	// Second request → second failure, circuit should OPEN
	_, _ = c.Get(context.Background(), ts.URL(), 0, nil)
	if c.circuitBreaker.state != stateOpen {
		t.Fatalf("expected circuit OPEN after reaching threshold, got %s", c.circuitBreaker.state)
	}

	// Third request → blocked by open circuit
	_, err := c.Get(context.Background(), ts.URL(), 0, nil)
	if err == nil || !strings.Contains(err.Error(), "circuit breaker: circuit open") {
		t.Fatalf("expected circuit‑open error, got %v", err)
	}
}

func TestDynamicRateAdjustment(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		w.Header().Set("X-RateLimit-Remaining", "2")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now+1, 10))
		w.Header().Set("X-RateLimit-Limit", "5")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	c := New(
		WithRateLimit(10, 10), // generous initial limit
		WithDynamicRateAdjustment(),
	)

	_, err := c.Get(context.Background(), ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After adjustment the limiter’s limit should be ≈2 req/sec.
	expected := rate.Limit(2)
	if c.limiter.Limit() != expected {
		t.Fatalf("expected limiter limit %v, got %v", expected, c.limiter.Limit())
	}
}

func TestIdempotencyKeyIsAddedWhenConfigured(t *testing.T) {
	var receivedKey string
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		receivedKey = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	c := New(WithIdempotencyMethods(http.MethodPost))

	_, err := c.Post(context.Background(), ts.URL(), nil, 0, nil)
	if err != nil {
		t.Fatalf("post error: %v", err)
	}
	if receivedKey == "" {
		t.Fatalf("expected Idempotency-Key header to be set")
	}
}

func TestApplyTimeoutNoTimeout(t *testing.T) {
	c := New()
	ctx, cancel := c.applyTimeout(context.Background(), 0)
	defer cancel()
	if ctx != context.Background() {
		t.Fatalf("expected original context when timeout <= 0")
	}
}

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

func TestBufferOverflow(t *testing.T) {
	largeBody := make([]byte, 11*1024*1024) // >10 MiB default
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

// ---------------------------------------------------------------------
// NEW tests – cover PUT, DELETE and additional edge‑cases
// ---------------------------------------------------------------------

// TestPUTJSONDecoding verifies that Put correctly unmarshals a JSON payload.
func TestPUTJSONDecoding(t *testing.T) {
	type payload struct {
		Result int `json:"result"`
	}

	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("expected PUT, got %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(payload{Result: 42})
	})
	defer ts.Close()

	c := New()
	var out payload
	_, err := c.Put(context.Background(), ts.URL(), nil, 0, &out)
	if err != nil {
		t.Fatalf("PUT error: %v", err)
	}
	if out.Result != 42 {
		t.Fatalf("unexpected PUT decoded result: %d", out.Result)
	}
}

// TestDELETEJSONDecoding verifies that Delete correctly unmarshals a JSON payload.
func TestDELETEJSONDecoding(t *testing.T) {
	type payload struct {
		Deleted bool `json:"deleted"`
	}

	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(payload{Deleted: true})
	})
	defer ts.Close()

	c := New()
	var out payload
	_, err := c.Delete(context.Background(), ts.URL(), 0, &out)
	if err != nil {
		t.Fatalf("DELETE error: %v", err)
	}
	if !out.Deleted {
		t.Fatalf("unexpected DELETE decoded result: %+v", out)
	}
}

// TestPUTWithBodyAndTimeout ensures the body is sent and the per‑request timeout is honoured.
func TestPUTWithBodyAndTimeout(t *testing.T) {
	const bodyContent = `{"foo":"bar"}`
	var received []byte

	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		// simulate a tiny delay to hit the timeout if it were wrong
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.Put(ctx, ts.URL(), strings.NewReader(bodyContent), 0, nil)
	if err != nil {
		t.Fatalf("PUT with body error: %v", err)
	}
	if string(received) != bodyContent {
		t.Fatalf("PUT body mismatch: got %s, want %s", string(received), bodyContent)
	}
}

// TestDELETEWithoutBodyEnsuresNoUnexpectedRead panics.
func TestDELETEWithoutBodyEnsuresNoUnexpectedRead(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			// reading an empty body should be safe
			_, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer ts.Close()

	c := New()
	_, err := c.Delete(context.Background(), ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("DELETE error: %v", err)
	}
}

// TestPUTWithCustomHeaders validates that default headers are merged correctly
// and that callers can add additional headers via a raw http.Request.
func TestPUTWithCustomHeaders(t *testing.T) {
	const customKey = "X-Custom-Header"
	const customVal = "custom-value"

	// Server just verifies the incoming headers.
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(customKey); got != customVal {
			t.Fatalf("expected %s=%s, got %s", customKey, customVal, got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token123" {
			t.Fatalf("expected Authorization header to be set, got %s", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	// Initialise the client with a default Authorization header.
	c := New(
		WithDefaultHeader("Authorization", "Bearer token123"),
	)

	// Build a raw PUT request and attach the extra custom header.
	req, err := http.NewRequest(http.MethodPut, ts.URL(), nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(customKey, customVal)

	// Execute the request via the client’s low‑level Do method.
	_, err = c.Do(req)
	if err != nil {
		t.Fatalf("Do with custom header error: %v", err)
	}
}
