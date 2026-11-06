# Bulkhead

The bulkhead is disabled unless `WithBulkhead` is configured. Each `Client`
owns an independent concurrency limit shared safely across its concurrent
requests.

## Configuration and scope

```go
client, err := httpx.New(httpx.WithBulkhead(httpx.BulkheadConfig{
	MaxConcurrent: 20,
}))
```

`MaxConcurrent` must be positive. The limit applies to logical operations:
each `Client.Do` acquires at most one permit before entering the circuit
breaker, retry policy, and HTTP client. Redirects and retries share that
permit rather than acquiring additional capacity.

## Acquisition and release

Acquisition waits until a permit is available or the request context is
canceled. Cancellation while waiting returns the request's context error and
does not consume capacity.

For a response with a streaming body, the permit remains held until the body
is read to EOF, returns a read error, or is closed. `http.NoBody` releases the
permit as soon as `Client.Do` returns. Errors and panics before a response body
is handed to the caller release the permit before returning or propagating the
panic. Response bodies remain caller-owned and must be closed, consistent with
standard `net/http` usage; failing to consume or close a body also retains its
permit.

Policy ordering is logical middleware, bulkhead, circuit breaker, retry, then
the HTTP client and attempt-level rate limiter. A request rejected by the
circuit breaker releases its bulkhead permit immediately. See
[Execution](EXECUTION.md), [Retries](RETRIES.md), [Circuit breaker](CIRCUIT_BREAKER.md),
and [Rate limiting](RATE_LIMITING.md).
