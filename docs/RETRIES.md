# Retries

Retries are disabled unless `WithRetry` is configured. The policy re-executes
the original request through `http.Client.Do`; redirects within each execution
are handled by `net/http`.

## Configuration

```go
client, err := httpx.New(httpx.WithRetry(httpx.RetryConfig{
	MaxAttempts:    3,
	InitialBackoff: 100 * time.Millisecond,
	MaxBackoff:     2 * time.Second,
	Jitter:         0.2,
}))
```

`MaxAttempts` includes the initial execution and must be at least 2.
`InitialBackoff` defaults to 100ms, `MaxBackoff` to 2s, and `Jitter` to zero
(disabled). Backoff doubles up to `MaxBackoff`; jitter randomizes the computed
delay symmetrically by the configured fraction. A valid `Retry-After` header
on a retryable response takes precedence when it specifies a longer delay.
Delta-seconds and HTTP-date forms are supported. Retry-After delays are not
clamped to `MaxBackoff`, but caller cancellation and operation deadlines still
interrupt the wait.

By default HTTPX retries transport errors implementing `net.Error` and
responses with status 408, 429, 500, 502, 503, or 504. Context cancellation and
deadline errors are never retried. `ShouldRetry` can replace this eligibility
policy; it must be concurrency-safe and must not mutate its request or response
arguments. Replay-safety checks apply regardless of the custom policy.

## Replay safety

Only idempotent methods are eligible for automatic replay: GET, HEAD, OPTIONS,
PUT, DELETE, and TRACE. POST and other methods are never automatically retried,
even if they carry an idempotency key. Requests without a body can be replayed.
A request with a body must provide `GetBody`, which HTTPX uses to create a fresh
body for each retry. A non-recreatable body is sent once.

This conservative method check does not imply a transport error means the
server did not process the request. It limits repeats to methods whose
semantics are idempotent; applications must still account for server behavior.

## Responses, errors, and hooks

Before retrying a response, HTTPX closes its body. A close failure stops retries
and is returned to the caller. The final response is returned unchanged and
its body remains caller-owned. In particular, when a retryable HTTP status
remains after `MaxAttempts`, the final response is returned with a nil Go error,
consistent with `net/http`; HTTP status codes are not converted to transport
errors.

When a retryable network error remains after `MaxAttempts`, `Client.Do` returns
a `RetryExhaustedError`. It matches `ErrRetryExhausted` with `errors.Is` and
unwraps the final network error. Cancellation or deadline expiry during
backoff returns the corresponding context error instead.

Middleware runs once around all retry executions. Operation hooks run once for
the logical operation. Attempt hooks observe every underlying
`RoundTripper.RoundTrip`, including redirects inside each retry execution.
Thus the number of attempt-hook events can exceed `MaxAttempts` when redirects
occur.
