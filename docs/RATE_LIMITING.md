# Rate limiting

Rate limiting is disabled unless `WithRateLimit` is configured. Each `Client`
owns an independent token bucket, shared safely across that client's
concurrent requests.

## Configuration and behavior

```go
client, err := httpx.New(httpx.WithRateLimit(httpx.RateLimitConfig{
	RequestsPerSecond: 10,
	Burst:             5,
}))
```

`RequestsPerSecond` must be positive and finite. `Burst` must be positive. The
bucket starts full, so it admits up to `Burst` attempts immediately and
replenishes at `RequestsPerSecond` tokens per second, up to the burst
capacity. Admission is non-blocking: if there is no token available, the
request fails immediately with an error matching `httpx.ErrRateLimitExceeded`.
Caller cancellation is returned as the context error and does not consume a
token.

## Attempt and policy semantics

The limiter runs immediately before each underlying transport exchange. Each
retry execution and each redirect is a separate attempt and consumes a token.
An attempt rejected for lack of capacity does not reach the transport and does
not invoke attempt hooks. Rate-limit rejections are not retried, even when a
custom retry policy would otherwise retry arbitrary errors.

The circuit breaker wraps retry execution and classifies only its final
outcome. Rate-limit rejections are treated as policy rejections rather than
downstream failures, so they do not trip the breaker. See
[Execution](EXECUTION.md), [Retries](RETRIES.md), and
[Circuit breaker](CIRCUIT_BREAKER.md) for related policy ordering.
