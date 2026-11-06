# HTTPX

HTTPX is a Go library for reliable, observable outbound HTTP communication,
built on the standard library's `net/http` package.

The goal is to provide a small, composable foundation for configuring HTTP
clients, middleware, resilience policies, and observability—while preserving
standard Go request and response behavior.

## Status

HTTPX is in early development. It currently provides a configurable HTTP
client with timeout, transport injection, logical-operation middleware,
request IDs, lifecycle hooks, opt-in retries, an opt-in circuit breaker, an
opt-in per-attempt rate limit, and an opt-in logical-operation bulkhead.
Integrated observability is planned but not yet implemented.

## Intended scope

The library is intended to provide:

- A configurable HTTP client compatible with Go's `net/http` conventions.
- Composable logical-operation middleware and request lifecycle hooks.
- Opt-in resilience policies such as retries, circuit breaking, rate limiting,
  and concurrency limits.
- Structured logging and optional metrics and tracing integrations.

Business logic, domain-specific API clients, serialization frameworks,
application-specific authentication, service discovery, and caching are out of
scope.

## Quickstart

```go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/tiagorocha94/httpx"
)

func main() {
	client, err := httpx.New(
		httpx.WithTimeout(10*time.Second),
		httpx.WithRequestIDHeader("X-Correlation-ID"),
		httpx.WithHooks(httpx.Hooks{
			AfterOperation: func(result httpx.OperationResult) {
				log.Printf(
					"HTTP %s request_id=%s status=%d duration=%s err=%v",
					result.Method,
					result.RequestID,
					result.StatusCode,
					result.Duration,
					result.Err,
				)
			},
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"https://example.com",
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		log.Fatal(err)
	}
	defer response.Body.Close()
}
```

The client follows standard `net/http` behavior: non-2xx statuses are
responses, not transport errors, and the caller owns the response body. See
[Configuration](docs/CONFIGURATION.md) for transport options and defaults.
See [Execution](docs/EXECUTION.md) for middleware composition, operation
boundaries, attempts, and context behavior. See [Hooks](docs/HOOKS.md) for
request IDs and operation/attempt lifecycle callbacks. See [Retries](docs/RETRIES.md)
for retry eligibility, replay safety, and backoff.

Retries are opt-in and limited to idempotent methods with replayable bodies:

```go
client, err := httpx.New(httpx.WithRetry(httpx.RetryConfig{
	MaxAttempts: 3,
}))
```

Non-idempotent methods such as POST are never automatically retried. See
[Retries](docs/RETRIES.md) for defaults, backoff configuration, and replay
limitations.

An optional circuit breaker rejects operations after repeated classified
failures:

```go
client, err := httpx.New(httpx.WithCircuitBreaker(httpx.CircuitBreakerConfig{
	FailureThreshold: 5,
	RecoveryTimeout:  30 * time.Second,
}))
```

See [Circuit breaker](docs/CIRCUIT_BREAKER.md) for state transitions,
classification, and retry accounting.

An optional per-client rate limiter controls the rate and burst of transport
attempts. Requests beyond the available capacity are rejected immediately:

```go
client, err := httpx.New(httpx.WithRateLimit(httpx.RateLimitConfig{
	RequestsPerSecond: 10,
	Burst:             5,
}))
```

See [Rate limiting](docs/RATE_LIMITING.md) for token-bucket behavior,
rejections, and how limits apply to retries and redirects.

An optional per-client bulkhead bounds concurrent logical operations. A permit
is held until the response body is read to completion or closed:

```go
client, err := httpx.New(httpx.WithBulkhead(httpx.BulkheadConfig{
	MaxConcurrent: 20,
}))
```

Waiting for capacity observes the request context. See
[Bulkhead](docs/BULKHEAD.md) for permit scope and lifecycle.

## Development

The project uses GNU Make to provide consistent local checks:

```sh
make check
```

See [Development](docs/DEVELOPMENT.md) for prerequisites and available targets,
and [CI](docs/CI.md) for the GitHub Actions and branch protection workflow.
