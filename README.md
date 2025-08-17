# gohttpcl

A robust and configurable HTTP client package for Go, designed for reliable API interactions with features like retries, exponential backoff, jitter, rate limiting, circuit breaker, logging with golog, body buffering, dynamic rate limit adjustment, metrics, context-aware logging, per-request timeouts, idempotency keys, response validation, and optional JSON response unmarshalling.

## Features

- **Retries with Exponential Backoff and Jitter**: Automatically retry failed requests with configurable backoff and jitter to prevent server overload.
- **Rate Limiting**: Enforce API rate limits (e.g., CoinSpot's 1000 requests per minute) using a token bucket algorithm.
- **Circuit Breaker**: Prevent overwhelming a failing service by temporarily halting requests after repeated failures.
- **Logging**: Context-aware logging with request IDs using [golog](https://github.com/evdnx/golog) for structured logging to stdout, files, or Google Cloud Logging.
- **Body Buffering**: Buffer request bodies for safe retries, with configurable size limits.
- **Dynamic Rate Adjustment**: Adjust rate limits based on API response headers (e.g., `X-RateLimit-Remaining`).
- **Metrics**: Collect request, retry, failure, and latency metrics via a customizable interface.
- **Per-Request Timeouts**: Set timeouts per request, overriding global client settings.
- **Idempotency Keys**: Automatically generate `Idempotency-Key` headers for specified methods (e.g., POST, PUT).
- **Response Validation**: Validate response status codes and content types before processing.
- **JSON Unmarshalling**: Optionally unmarshal JSON response bodies into user-provided structs.

## Installation

```bash
go get github.com/evdnx/gohttpcl
go get github.com/evdnx/golog
```

## Usage

Below is an example of using `gohttpcl` with `golog` to interact with an API like CoinSpot, which has a rate limit of 1000 requests per minute.

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
		golog.WithFileProvider("/var/log/myapp.log", 10, 3, 7, true), // 10MB, 3 backups, 7 days, compress
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
		if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			return fmt.Errorf("unexpected content type: %s", contentType)
		}
		return nil
	}

	// Create client
	client := gohttpcl.New(
		gohttpcl.WithRateLimit(1000.0/60.0, 10), // CoinSpot: 1000 req/min
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
	body := strings.NewReader(`{"key": "value"}`)
	var responseData map[string]interface{}
	resp, err := client.Post(ctx, "https://api.coinspot.com.au/api/v2/endpoint", body, 5*time.Second, &responseData)
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

- `WithMaxRetries(n int)`: Set maximum retry attempts.
- `WithMinBackoff(d time.Duration)`: Set minimum backoff duration.
- `WithMaxBackoff(d time.Duration)`: Set maximum backoff duration.
- `WithBackoffFactor(f float64)`: Set exponential backoff factor.
- `WithJitter(b bool)`: Enable/disable jitter in backoff.
- `WithRetryable(f func(*http.Response, error) bool)`: Custom retry logic.
- `WithTimeout(d time.Duration)`: Set global client timeout.
- `WithTransport(tr http.RoundTripper)`: Set custom transport.
- `WithDefaultHeader(key, value string)`: Add a default header.
- `WithDefaultHeaders(headers map[string]string)`: Add multiple default headers.
- `WithRateLimit(limit float64, burst int)`: Set rate limit (requests per second) and burst size.
- `WithGologLogger(logger *golog.Logger)`: Set golog logger for structured logging.
- `WithCircuitBreaker(failureThreshold int, resetTimeout time.Duration)`: Enable circuit breaker.
- `WithDynamicRateAdjustment()`: Enable dynamic rate limit adjustment based on headers.
- `WithRateWindow(d time.Duration)`: Set fallback rate window for dynamic adjustment.
- `WithMetrics(metrics MetricsCollector)`: Set metrics collector.
- `WithMaxBufferSize(size int64)`: Set maximum request body buffer size (0 = no limit).
- `WithRateLimitHeaderPrefix(prefix string)`: Set custom prefix for rate limit headers.
- `WithIdempotencyMethods(methods ...string)`: Enable idempotency keys for specified methods.
- `WithResponseValidation(validate func(*http.Response) error)`: Set response validation function.

## Key Features in Detail

### Retries
Retries are handled with exponential backoff and optional jitter. The `Retry-After` header is respected for precise retry timing, particularly useful for 429 or 503 responses.

### Rate Limiting
The client uses a token bucket algorithm to enforce rate limits, configurable via `WithRateLimit`. For CoinSpot's 1000 requests per minute, use `WithRateLimit(1000.0/60.0, 10)`.

### Circuit Breaker
Prevents overwhelming a failing service by halting requests after a configurable number of failures, with a reset timeout for recovery.

### Logging
Uses `golog` for structured, context-aware logging with request IDs. Supports multiple providers (stdout, file with rotation, Google Cloud Logging) and log levels (Debug, Info, Warn, Error, Fatal).

### Body Buffering
Request bodies are buffered for retries, with a configurable size limit to prevent memory issues. Set to 0 for no limit (use with caution).

### Dynamic Rate Adjustment
Adjusts the rate limiter based on headers like `X-RateLimit-Remaining` and `X-RateLimit-Reset`. Customizable header prefixes support non-standard APIs.

### Metrics
A `MetricsCollector` interface allows tracking requests, retries, failures, and latency, compatible with systems like Prometheus.

### Per-Request Timeouts
Each HTTP method (`Get`, `Post`, `Put`, `Delete`) accepts a timeout parameter, overriding the global timeout for fine-grained control.

### Idempotency Keys
Automatically generates `Idempotency-Key` headers for specified methods, using the request ID to ensure safe retries for non-idempotent operations.

### Response Validation
Custom validation functions can check status codes, content types, or other response properties before processing.

### JSON Unmarshalling
Optionally unmarshal JSON responses into a provided `interface{}` (e.g., struct or map), simplifying response handling.

## Dependencies

- `github.com/evdnx/golog`: For structured logging with multiple providers.
- `github.com/google/uuid`: For generating request IDs and idempotency keys.
- `golang.org/x/time/rate`: For rate limiting.

## License

MIT License. See [LICENSE](LICENSE) for details.

## Contributing

Contributions are welcome! Please submit pull requests or open issues on the [GitHub repository](https://github.com/evdnx/gohttpcl).
