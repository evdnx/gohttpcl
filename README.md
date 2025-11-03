# gohttpcl

A robust and configurable HTTP client package for Go, designed for reliable API interactions with features like retries, exponential backoff, jitter, rate limiting, circuit breaker, logging with golog, body buffering, dynamic rate‑limit adjustment, metrics, context‑aware logging, per‑request timeouts, idempotency keys, response validation, and optional JSON response unmarshalling.

## Features

- **Retries with Exponential Backoff and Jitter** – Automatically retry failed requests with configurable backoff and jitter to prevent server overload.
- **Backoff Strategies** – Choose from exponential, linear, constant, or Fibonacci delay policies per retry.
- **Rate Limiting** – Enforce API rate limits (e.g., CoinSpot's 1000 requests per minute) using a token bucket algorithm.
- **Circuit Breaker** – Prevent overwhelming a failing service by temporarily halting requests after repeated failures.
- **Logging** – Context‑aware logging with request IDs using [golog](https://github.com/evdnx/golog) for structured logging to stdout, files, or Google Cloud Logging.
- **Body Buffering** – Buffer request bodies for safe retries, with configurable size limits.
- **Dynamic Rate Adjustment** – Adjust rate limits based on API response headers (e.g., `X-RateLimit-Remaining`).
- **Retry Budgets** – Enforce retry-to-request ratios to avoid overwhelming downstream services during outages.
- **Metrics** – Collect request, retry, failure, and latency metrics via a customizable interface.
- **Per‑Request Timeouts** – Set timeouts per request, overriding global client settings.
- **Idempotency Keys** – Automatically generate `Idempotency-Key` headers for specified methods (e.g., `POST`, `PUT`).
- **Response Validation** – Validate response status codes and content types before processing.
- **JSON Unmarshalling** – Optionally unmarshal JSON responses into a user‑provided struct or map.
- **Full HTTP Verb Support** – `GET`, `POST`, `PUT`, `DELETE` helpers (all honour the same feature set).
- **Header Management** – Merge default headers with per‑request headers, support multi‑value headers, and allow explicit overrides.

## Installation

```bash
go get github.com/evdnx/gohttpcl
go get github.com/evdnx/golog
```

## Quick Start

Below is an example of using `gohttpcl` with `golog` to interact with an API like CoinSpot, which has a rate limit of 1000 requests per minute.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/evdnx/golog"
	"github.com/evdnx/gohttpcl"
)

// Simple metrics collector for demonstration
type simpleMetrics struct{}

func (m *simpleMetrics) IncRequests(method, url string) {
	log.Printf("Metric: Request sent: %s %s", method, url)
}
func (m *simpleMetrics) IncRetries(method, url string, attempt int) {
	log.Printf("Metric: Retry attempt %d: %s %s", attempt, method, url)
}
func (m *simpleMetrics) IncFailures(method, url string, statusCode int) {
	log.Printf("Metric: Failure (status %d): %s %s", statusCode, method, url)
}
func (m *simpleMetrics) ObserveLatency(method, url string, duration time.Duration) {
	log.Printf("Metric: Latency %v: %s %s", duration, method, url)
}

func main() {
	// Create a golog logger
	logger, err := golog.NewLogger(
		golog.WithStdOutProvider("console"),
		golog.WithFileProvider("/var/log/myapp.log", 10, 3, 7, true), // 10 MiB, 3 backups, 7 days, compress
		golog.WithLevel(golog.DebugLevel),
	)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Sync()

	// Response validation function
	validateResponse := func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status: %s", resp.Status)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			return fmt.Errorf("unexpected content type: %s", ct)
		}
		return nil
	}

	// Initialise the client
	client := gohttpcl.New(
		gohttpcl.WithRateLimit(1000.0/60.0, 10), // CoinSpot: 1000 req/min
		gohttpcl.WithDynamicRateAdjustment(),
		gohttpcl.WithMaxRetries(5),
		gohttpcl.WithCircuitBreaker(3, 30*time.Second),
		gohttpcl.WithGologLogger(logger),
		gohttpcl.WithMetrics(&simpleMetrics{}),
		gohttpcl.WithMaxBufferSize(5*1024*1024),
		gohttpcl.WithIdempotencyMethods("POST", "PUT"),
		gohttpcl.WithResponseValidation(validateResponse),
		gohttpcl.WithDefaultHeader("Authorization", "Bearer your-api-key"),
	)

	// Example POST request with JSON unmarshalling
	ctx := context.Background()
	body := strings.NewReader(`{"key":"value"}`)
	var responseData map[string]interface{}
	resp, err := client.Post(ctx,
		"https://api.coinspot.com.au/api/v2/endpoint",
		body,
		5*time.Second,
		&responseData,
	)
	if err != nil {
		logger.Error("Request failed", golog.Error(err))
		return
	}
	defer resp.Body.Close()

	fmt.Printf("Response status: %s\n", resp.Status)
	fmt.Printf("Unmarshalled response: %v\n", responseData)
}
```

## Configuration Options

The `gohttpcl` package uses functional options for flexible configuration. Available options include:

- `WithMaxRetries(n int)` – Set maximum retry attempts.
- `WithMinBackoff(d time.Duration)` – Minimum backoff duration.
- `WithMaxBackoff(d time.Duration)` – Maximum backoff duration.
- `WithBackoffFactor(f float64)` – Exponential backoff factor.
- `WithBackoffStrategy(strategy gohttpcl.BackoffStrategy)` – Select linear, constant, Fibonacci, or exponential delays.
- `WithJitter(b bool)` – Enable/disable jitter.
- `WithRetryable(fn func(*http.Response, error) bool)` – Custom retry predicate.
- `WithTimeout(d time.Duration)` – Global client timeout.
- `WithTransport(tr http.RoundTripper)` – Custom transport.
- `WithDefaultHeader(key, value string)` – Add a default header.
- `WithDefaultHeaders(headers map[string]string)` – Add multiple default headers.
- `WithRateLimit(limit float64, burst int)` – Set requests‑per‑second and burst size.
- `WithGologLogger(logger *golog.Logger)` – Structured logger.
- `WithCircuitBreaker(failureThreshold int, resetTimeout time.Duration)` – Enable circuit breaker.
- `WithDynamicRateAdjustment()` – Enable dynamic rate‑limit adjustment.
- `WithRateWindow(d time.Duration)` – Fallback window for dynamic adjustment.
- `WithMetrics(m MetricsCollector)` – Set metrics collector.
- `WithRetryBudget(maxRatio float64, window time.Duration)` – Limit total retries over a rolling window.
- `WithMaxBufferSize(size int64)` – Max request‑body buffer size (`0` = unlimited).
- `WithRateLimitHeaderPrefix(prefix string)` – Custom header prefix for rate‑limit info.
- `WithIdempotencyMethods(methods ...string)` – Enable idempotency keys for given methods.
- `WithResponseValidation(fn func(*http.Response) error)` – Set response validator.

## Key Features in Detail

### Retries
Handled by an exponential backoff algorithm. The `Retry-After` header is respected for precise timing, especially on `429` or `503` responses.

### Rate Limiting
Implemented with a token bucket (`golang.org/x/time/rate`). Configure via `WithRateLimit`. Example for CoinSpot: `WithRateLimit(1000.0/60.0, 10)`.

### Circuit Breaker
Stops sending requests after `failureThreshold` consecutive failures. After `resetTimeout` the breaker attempts to close again.

### Logging
Integrates with **golog** for structured logs containing request IDs, timestamps, and severity levels. Supports multiple providers (stdout, file rotation, GCP).

### Body Buffering
Buffers request bodies up to `maxBufferSize`. Guarantees identical payloads on retries.

### Dynamic Rate Adjustment
Reads headers such as `X-RateLimit-Remaining` and `X-RateLimit-Reset` to recalculate the limiter’s rate on‑the‑fly.

### Metrics
Implement the `MetricsCollector` interface to hook into Prometheus, OpenTelemetry, or any custom system.

### Per‑Request Timeouts
Each helper (`Get`, `Post`, `Put`, `Delete`) accepts a `timeout` argument. Internally `context.WithTimeout` is applied.

### Idempotency Keys
For methods listed via `WithIdempotencyMethods`, a UUID‑based `Idempotency-Key` header is added automatically.

### Response Validation
User‑provided function runs after receiving a response but before body processing, allowing checks on status, content type, etc.

### JSON Unmarshalling
If a non‑nil `out` parameter is supplied, the response body is read, unmarshalled into `out`, and the body is closed.

## Dependencies

- `github.com/evdnx/golog` – Structured logging with multiple output providers.
- `github.com/google/uuid` – Generation of request IDs and idempotency keys.
- `golang.org/x/time/rate` – Token‑bucket rate limiting implementation.

## Testing

The library ships with a comprehensive test suite covering:

| Feature                              | Tested Method(s) |
|--------------------------------------|------------------|
| Default retry predicate              | `defaultRetryable` |
| Exponential back‑off (deterministic) | `calculateBackoff` |
| Automatic retries on transient errors| `Get` |
| Circuit‑breaker state transitions    | `Get` (via CB) |
| Dynamic rate‑limit adjustment        | `Get` |
| Idempotency‑Key injection            | `Post` |
| Timeout handling (no‑timeout & with‑timeout) | `applyTimeout` |
| JSON decoding                        | `Get` / `Post` |
| Request‑body buffering overflow protection | `Post` |
| **PUT** request handling & JSON unmarshalling | `Put` |
| **DELETE** request handling & JSON unmarshalling | `Delete` |
| Header propagation & default‑header merging | all helpers |
| Rate‑limiting token bucket behavior   | `Get`, `Put`, `Delete` |

Run the full suite with:

```bash
go test ./...
```

## License

MIT‑0 License. See the `LICENSE` file for full terms.
