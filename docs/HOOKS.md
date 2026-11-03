# Request IDs and lifecycle hooks

## Request IDs

For each non-nil request, HTTPX assigns a request ID and sends it in the
`X-Request-ID` header by default. A nil request follows the standard
`net/http` error path without request ID or lifecycle callbacks. Configure a
different header with `WithRequestIDHeader`:

```go
client, err := httpx.New(
	httpx.WithRequestIDHeader("X-Correlation-ID"),
)
```

An existing non-empty value in the configured header is preserved. Otherwise,
HTTPX generates a UUID v4. The ID is added to HTTPX's request clone, so the
caller-owned request and its headers are not modified. The same ID is sent on
each transport attempt, including redirects. Unrelated headers are preserved.
Middleware can read the ID, but HTTPX restores it at the transport boundary if
middleware changes it, keeping it consistent across attempts.

The configured header name must be a valid HTTP field name. Failure to generate
an ID is returned from `Client.Do`; HTTPX does not continue without one.

## Lifecycle callbacks

Use `WithHooks` to observe a logical operation and its individual transport
attempts:

```go
client, err := httpx.New(httpx.WithHooks(httpx.Hooks{
	BeforeOperation: func(event httpx.OperationEvent) {
		log.Printf("starting %s request_id=%s", event.Method, event.RequestID)
	},
	AfterOperation: func(result httpx.OperationResult) {
		log.Printf(
			"finished request_id=%s status=%d duration=%s err=%v",
			result.RequestID,
			result.StatusCode,
			result.Duration,
			result.Err,
		)
	},
	BeforeAttempt: func(event httpx.AttemptEvent) {
		log.Printf("attempt %d request_id=%s", event.Attempt, event.RequestID)
	},
	AfterAttempt: func(result httpx.AttemptResult) {
		log.Printf("attempt %d status=%d err=%v", result.Attempt, result.StatusCode, result.Err)
	},
}))
```

For non-nil requests, callbacks run synchronously. A logical operation's hooks
run once around the middleware chain; attempt hooks run around each underlying
`RoundTripper` exchange, including redirect exchanges. A middleware
short-circuit still triggers operation hooks but does not trigger attempt
hooks. The order is:

1. `BeforeOperation`
2. `BeforeAttempt` and `AfterAttempt` for each transport exchange
3. `AfterOperation`

`AfterOperation` runs once the middleware and HTTP execution have resolved,
before `Client.Do` returns the response. Its duration excludes response-body
reads; the operation context itself remains active for the response-body
lifetime as described in [Execution](EXECUTION.md). The result includes a
status code when a response is available (otherwise zero) and the error
returned by execution. An operation completed by middleware without a
transport attempt has no attempt events.

Callbacks receive value snapshots, not mutable requests, responses, or internal
execution state. They are observational: they cannot change execution or
return an error to stop it. Callbacks may be invoked concurrently for different
operations, so callback implementations must be concurrency-safe, non-blocking,
and must not panic. Callback code should handle `Err` before using result
fields. The `URL` field can contain user information or sensitive query values;
redact it before recording.

Retries are not implemented. When available, each retry will be represented as
an additional attempt within the same logical operation.
