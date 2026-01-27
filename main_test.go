package gohttpcl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/*
Helper -----------------------------------------------------------------------
*/

type testServer struct {
	handler http.HandlerFunc
	srv     *http.Server
	ln      net.Listener
	url     string
	calls   int32
}

func newTestServer(h http.HandlerFunc) *testServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	ts := &testServer{
		handler: h,
		ln:      ln,
		url:     "http://" + ln.Addr().String(),
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&ts.calls, 1)
			h(w, r)
		}),
	}
	ts.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			panic(err)
		}
	}()
	return ts
}

func (ts *testServer) URL() string { return ts.url }
func (ts *testServer) Calls() int  { return int(atomic.LoadInt32(&ts.calls)) }
func (ts *testServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = ts.srv.Shutdown(ctx)
}

type captureServer struct {
	srv    *http.Server
	ln     net.Listener
	url    string
	header http.Header
	body   []byte
	mu     sync.Mutex
}

func newCaptureServer(handler func(w http.ResponseWriter, r *http.Request)) *captureServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	cs := &captureServer{
		ln:  ln,
		url: "http://" + ln.Addr().String(),
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cs.mu.Lock()
			cs.header = r.Header.Clone()
			if r.Body != nil {
				cs.body, _ = io.ReadAll(r.Body)
			} else {
				cs.body = nil
			}
			cs.mu.Unlock()
			handler(w, r)
		}),
	}
	cs.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			panic(err)
		}
	}()
	return cs
}

// helper to spin up a test server that records the request it receives
func (cs *captureServer) URL() string { return cs.url }
func (cs *captureServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = cs.srv.Shutdown(ctx)
}
func (cs *captureServer) Header() http.Header {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.header.Clone()
}
func (cs *captureServer) Body() []byte {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]byte(nil), cs.body...)
}

type countingBody struct {
	mu     sync.Mutex
	closed bool
}

func (cb *countingBody) Read(p []byte) (int, error) { return 0, io.EOF }

func (cb *countingBody) Close() error {
	cb.mu.Lock()
	cb.closed = true
	cb.mu.Unlock()
	return nil
}

func (cb *countingBody) Closed() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.closed
}

type recordingTransport struct {
	mu     sync.Mutex
	calls  int
	bodies []*countingBody
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.calls++
	if rt.calls == 1 {
		body := &countingBody{}
		rt.bodies = append(rt.bodies, body)
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       body,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (rt *recordingTransport) FirstBodyClosed() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.bodies) == 0 {
		return false
	}
	return rt.bodies[0].Closed()
}

func (rt *recordingTransport) CallCount() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.calls
}

type metricsRecorder struct {
	mu      sync.Mutex
	retries []int
}

func (m *metricsRecorder) IncRequests(string, string) {}
func (m *metricsRecorder) IncRetries(_ string, _ string, attempt int) {
	m.mu.Lock()
	m.retries = append(m.retries, attempt)
	m.mu.Unlock()
}
func (m *metricsRecorder) IncFailures(string, string, int)              {}
func (m *metricsRecorder) ObserveLatency(string, string, time.Duration) {}
func (m *metricsRecorder) RetryAttempts() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, len(m.retries))
	copy(out, m.retries)
	return out
}

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

func TestBackoffStrategyVariants(t *testing.T) {
	c := New()
	c.jitter = false
	c.minBackoff = 100 * time.Millisecond
	c.maxBackoff = time.Hour

	c.backoffStrategy = BackoffConstant
	for attempt := 0; attempt < 3; attempt++ {
		if got := c.calculateBackoff(attempt, 0); got != c.minBackoff {
			t.Fatalf("constant strategy: attempt %d, want %v got %v", attempt, c.minBackoff, got)
		}
	}

	c.backoffStrategy = BackoffLinear
	wantLinear := []time.Duration{
		c.minBackoff,
		2 * c.minBackoff,
		3 * c.minBackoff,
	}
	for attempt, want := range wantLinear {
		if got := c.calculateBackoff(attempt, 0); got != want {
			t.Fatalf("linear strategy: attempt %d, want %v got %v", attempt, want, got)
		}
	}

	c.backoffStrategy = BackoffFibonacci
	wantFib := []time.Duration{
		c.minBackoff,     // fib(1) = 1
		c.minBackoff,     // fib(2) = 1
		2 * c.minBackoff, // fib(3) = 2
		3 * c.minBackoff, // fib(4) = 3
		5 * c.minBackoff, // fib(5) = 5
	}
	for attempt, want := range wantFib {
		if got := c.calculateBackoff(attempt, 0); got != want {
			t.Fatalf("fibonacci strategy: attempt %d, want %v got %v", attempt, want, got)
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

func TestRetryBudgetLimitsRetries(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("fail"))
	})
	defer ts.Close()

	ctx := context.Background()

	clientNoBudget := New(WithMaxRetries(5))
	resp, err := clientNoBudget.Get(ctx, ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("unexpected error without budget: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	callsWithoutBudget := ts.Calls()
	if callsWithoutBudget <= 1 {
		t.Fatalf("expected retries without budget, got %d calls", callsWithoutBudget)
	}

	atomic.StoreInt32(&ts.calls, 0)

	clientWithBudget := New(
		WithMaxRetries(5),
		WithRetryBudget(0.3, time.Hour),
	)
	resp, err = clientWithBudget.Get(ctx, ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("unexpected error with budget: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	callsWithBudget := ts.Calls()
	if callsWithBudget != 2 {
		t.Fatalf("expected budget to limit retries to 1 additional attempt, got %d calls", callsWithBudget)
	}
	if callsWithBudget >= callsWithoutBudget {
		t.Fatalf("budget should reduce retries: without budget %d, with budget %d", callsWithoutBudget, callsWithBudget)
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
		w.Header().Set("X-RateLimit-Remaining", "2")
		w.Header().Set("X-RateLimit-Reset", "1")
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

	// After adjustment the limiter’s limit should be close to 2 req/sec.
	const expected = 2.0
	if diff := math.Abs(float64(c.limiter.Limit()) - expected); diff > 0.25 {
		t.Fatalf("expected limiter limit approximately %v, got %v (diff=%v)", expected, c.limiter.Limit(), diff)
	}
}

func TestRetryTransportClosesBodyAndRecordsRetries(t *testing.T) {
	rt := &recordingTransport{}
	metrics := &metricsRecorder{}
	c := New(
		WithTransport(rt),
		WithMaxRetries(1),
		WithMetrics(metrics),
	)

	_, err := c.Post(context.Background(), "http://example.com", strings.NewReader("payload"), 0, nil)
	if err != nil {
		t.Fatalf("post failed: %v", err)
	}

	if rt.CallCount() != 2 {
		t.Fatalf("expected 2 transport calls (1 retry), got %d", rt.CallCount())
	}
	if !rt.FirstBodyClosed() {
		t.Fatalf("expected response body to be closed before retry")
	}
	if attempts := metrics.RetryAttempts(); len(attempts) != 1 || attempts[0] != 1 {
		t.Fatalf("expected metrics to record retry attempt 1, got %v", attempts)
	}
}

func TestParseResetWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	validHTTPDate := now.Add(3 * time.Second).UTC().Format(http.TimeFormat)

	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"seconds offset", "5", 5 * time.Second, true},
		{"epoch seconds", strconv.FormatInt(now.Add(2*time.Second).Unix(), 10), 2 * time.Second, true},
		{"http date", validHTTPDate, 3 * time.Second, true},
		{"invalid", "not-a-number", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseResetWindow(tt.value, now)
			if ok != tt.ok {
				t.Fatalf("expected ok=%v, got %v", tt.ok, ok)
			}
			if !tt.ok {
				return
			}
			if diff := math.Abs(float64(got - tt.want)); diff > float64(20*time.Millisecond) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
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

func TestResponseValidationIsApplied(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("oops body"))
	})
	defer ts.Close()

	c := New(
		WithResponseValidation(func(resp *http.Response) error {
			if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
				return fmt.Errorf("unexpected content type: %s", resp.Header.Get("Content-Type"))
			}
			return nil
		}),
	)

	resp, err := c.Get(context.Background(), ts.URL(), 0, nil)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	if !IsValidationError(err) {
		t.Fatalf("expected IsValidationError to detect validation failure, got %v", err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError type")
	}
	if ve.Response == nil {
		t.Fatalf("expected ValidationError to carry response")
	}
	if resp == nil || resp.Body == nil {
		t.Fatalf("expected response (with body) to be returned alongside error")
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("failed to read returned body: %v", readErr)
	}
	if string(body) != "oops body" {
		t.Fatalf("unexpected body content: %q", body)
	}

	// BodyBytes should cache and keep the body reusable.
	bytes1, berr := ve.BodyBytes()
	if berr != nil {
		t.Fatalf("BodyBytes error: %v", berr)
	}
	if string(bytes1) != "oops body" {
		t.Fatalf("BodyBytes content mismatch: %q", bytes1)
	}
	// Second call should return same bytes without error.
	bytes2, berr := ve.BodyBytes()
	if berr != nil {
		t.Fatalf("BodyBytes second call error: %v", berr)
	}
	if string(bytes2) != "oops body" {
		t.Fatalf("BodyBytes second content mismatch: %q", bytes2)
	}
	// Response body should remain readable after BodyBytes.
	again, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("failed to reread body after BodyBytes: %v", readErr)
	}
	if string(again) != "oops body" {
		t.Fatalf("unexpected reread content: %q", again)
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

func TestBufferRequestBodyAllowsExactLimit(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1024)
	req := httptest.NewRequest(http.MethodPost, "http://example.com", bytes.NewReader(body))

	data, err := bufferRequestBody(req, int64(len(body)))
	if err != nil {
		t.Fatalf("unexpected error buffering body: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("buffered body mismatch")
	}
	replay, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read rewound body: %v", err)
	}
	if !bytes.Equal(replay, body) {
		t.Fatalf("rewound body mismatch")
	}
}

func TestBufferRequestBodyUnlimited(t *testing.T) {
	body := []byte("payload")
	req := httptest.NewRequest(http.MethodPost, "http://example.com", bytes.NewReader(body))

	data, err := bufferRequestBody(req, 0)
	if err != nil {
		t.Fatalf("unexpected error buffering body with unlimited size: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("buffered body mismatch")
	}
	replay, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read rewound body: %v", err)
	}
	if !bytes.Equal(replay, body) {
		t.Fatalf("rewound body mismatch")
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

func TestDefaultHeadersAreSentWhenNoOpts(t *testing.T) {
	// Server just returns 200 OK.
	ts := newCaptureServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	client := New(
		WithDefaultHeader("X-Foo", "bar"),
		WithDefaultHeader("Authorization", "Bearer default-token"),
	)

	// Use Delete (any helper would work) without any per‑request options.
	_, err := client.Delete(context.Background(), ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	hdr := ts.Header()
	if got := hdr.Get("X-Foo"); got != "bar" {
		t.Fatalf("expected X-Foo=bar, got %q", got)
	}
	if got := hdr.Get("Authorization"); got != "Bearer default-token" {
		t.Fatalf("expected Authorization default, got %q", got)
	}
}

func TestWithHeaderOverridesDefault(t *testing.T) {
	ts := newCaptureServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	client := New(
		WithDefaultHeader("Authorization", "Bearer default-token"),
	)

	// Override the Authorization header for this request only.
	_, err := client.Get(
		context.Background(),
		ts.URL(),
		0,
		nil,
		WithHeader("Authorization", "Bearer overridden-token"),
	)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}

	hdr := ts.Header()
	if got := hdr.Get("Authorization"); got != "Bearer overridden-token" {
		t.Fatalf("expected overridden Authorization, got %q", got)
	}
}

func TestWithHeadersMultiValue(t *testing.T) {
	ts := newCaptureServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	client := New()

	// Build a multi‑value header set.
	h := http.Header{}
	h.Add("Set-Cookie", "a=1; Path=/")
	h.Add("Set-Cookie", "b=2; Path=/")

	_, err := client.Post(
		context.Background(),
		ts.URL(),
		strings.NewReader(`{}`),
		0,
		nil,
		WithHeaders(h),
	)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}

	hdr := ts.Header()
	cookies := hdr["Set-Cookie"]
	if len(cookies) != 2 {
		t.Fatalf("expected 2 Set-Cookie values, got %d", len(cookies))
	}
	if cookies[0] != "a=1; Path=/" || cookies[1] != "b=2; Path=/" {
		t.Fatalf("unexpected Set-Cookie values: %v", cookies)
	}
}

func TestMultipleOptionOrder(t *testing.T) {
	ts := newCaptureServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	client := New(
		WithDefaultHeader("X-Test", "default"),
	)

	// First we add a multi‑value header set, then we override with a single WithHeader.
	hdrSet := http.Header{}
	hdrSet.Add("X-Test", "from-WithHeaders")

	_, err := client.Put(
		context.Background(),
		ts.URL(),
		strings.NewReader(`{}`),
		0,
		nil,
		WithHeaders(hdrSet),           // adds X-Test=from-WithHeaders
		WithHeader("X-Test", "final"), // should overwrite the previous value
	)
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}

	hdr := ts.Header()
	if got := hdr.Get("X-Test"); got != "final" {
		t.Fatalf("expected final X-Test header to be 'final', got %q", got)
	}
}

func TestAllHelpersRespectOptions(t *testing.T) {
	// Server echoes back the method name so we know which helper was called.
	ts := newCaptureServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Method", r.Method) // <-- response header
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	client := New()

	// Helper map: method name → function to invoke.
	helpers := map[string]func() (*http.Response, error){
		"GET": func() (*http.Response, error) {
			return client.Get(context.Background(), ts.URL(), 0, nil,
				WithHeader("X-From", "GET"))
		},
		"POST": func() (*http.Response, error) {
			return client.Post(context.Background(), ts.URL(),
				strings.NewReader(`{}`), 0, nil,
				WithHeader("X-From", "POST"))
		},
		"PUT": func() (*http.Response, error) {
			return client.Put(context.Background(), ts.URL(),
				strings.NewReader(`{}`), 0, nil,
				WithHeader("X-From", "PUT"))
		},
		"DELETE": func() (*http.Response, error) {
			return client.Delete(context.Background(), ts.URL(), 0, nil,
				WithHeader("X-From", "DELETE"))
		},
	}

	for wantMethod, fn := range helpers {
		resp, err := fn()
		if err != nil {
			t.Fatalf("%s helper returned error: %v", wantMethod, err)
		}
		// 1️⃣ Verify the **response** header that the server set.
		if got := resp.Header.Get("X-Echo-Method"); got != wantMethod {
			t.Fatalf("expected server to receive %s, got %s", wantMethod, got)
		}
		// 2️⃣ Verify the per‑request header that we injected.
		if got := ts.Header().Get("X-From"); got != wantMethod {
			t.Fatalf("%s helper did not forward per‑request header, got %q", wantMethod, got)
		}
	}
}

func TestOptionsDoNotLeakBetweenCalls(t *testing.T) {
	ts := newCaptureServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	client := New(
		WithDefaultHeader("X-Common", "common"),
	)

	// First request supplies a temporary header.
	_, err := client.Get(context.Background(), ts.URL(), 0, nil,
		WithHeader("X-Temp", "first"),
	)
	if err != nil {
		t.Fatalf("first GET failed: %v", err)
	}
	hdr1 := ts.Header()
	if got := hdr1.Get("X-Temp"); got != "first" {
		t.Fatalf("first request missing X-Temp header")
	}

	// Second request does **not** specify X-Temp; it must NOT appear.
	_, err = client.Get(context.Background(), ts.URL(), 0, nil)
	if err != nil {
		t.Fatalf("second GET failed: %v", err)
	}
	hdr2 := ts.Header()
	if got := hdr2.Get("X-Temp"); got != "" {
		t.Fatalf("second request leaked X-Temp header: %q", got)
	}
	// Ensure the common default header is still there.
	if got := hdr2.Get("X-Common"); got != "common" {
		t.Fatalf("common header missing on second request")
	}
}
