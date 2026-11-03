# Execution and middleware

## Logical operations and attempts

Each call to `Client.Do` is one logical operation. HTTPX clones the caller's
request and attaches an operation-scoped context while preserving the original
request context, cancellation, deadline, and values.

The configured middleware chain wraps the complete logical operation. A
redirect initiated by `net/http` remains within the same operation and does
not invoke the logical middleware chain again. Each underlying
`RoundTripper.RoundTrip` call is an attempt; redirects therefore produce
multiple attempts. Operation and attempt state is private and is not exposed
through the public API.

Retries are disabled by default. When enabled with `WithRetry`, retry policy
wraps `http.Client.Do` inside the configured middleware chain. Middleware runs
once per public `Client.Do`; each retry re-executes the original request,
including any redirects. `RetryConfig.MaxAttempts` counts these complete
executions, including the initial one. Attempt hooks still run for every
individual transport exchange, so redirects and retries can produce more
attempt-hook events than `MaxAttempts`. See [Retries](RETRIES.md) for
eligibility, request replay, and response cleanup details.

## Middleware composition

Implement middleware using `httpx.Middleware` and add it with
`httpx.WithMiddleware`:

```go
package example

import (
	"net/http"

	"github.com/tiagorocha94/httpx"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (fn doerFunc) Do(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func newClient() (*httpx.Client, error) {
	addHeader := httpx.Middleware(func(next httpx.HTTPDoer) httpx.HTTPDoer {
		return doerFunc(func(request *http.Request) (*http.Response, error) {
			request.Header.Set("X-Client", "example")
			return next.Do(request)
		})
	})

	return httpx.New(httpx.WithMiddleware(addHeader))
}
```

Middleware is applied in the order passed to `New`. The first middleware is
the outermost wrapper: before-next execution runs first-in-first-out, and
after-next execution runs last-in-first-out.

Middleware must be safe for concurrent use. It may return a response or error
without calling `next` to short-circuit an operation. It must not consume
request or response bodies or otherwise silently change their semantics.
Middleware errors are returned to the caller. A nil middleware or a middleware
that constructs a nil `HTTPDoer` makes client construction fail with
`ErrInvalidConfig`. Returning a nil response without an error fails with
`ErrInvalidResponse`.

The request value passed to middleware is a clone. Its headers and URL can be
modified without changing the caller's request value. The body is a streaming
reader shared with the caller's request; middleware must not read or replace
it.

## Context, deadlines, and timeouts

The request context flows through middleware, redirects, and transport
attempts. Client timeout is applied as an operation-scoped context deadline;
the earlier of it and any caller-supplied deadline wins. It covers
synchronous middleware work that observes the context, as well as redirects
and response-body reads.

Middleware that performs waits or other potentially blocking work must observe
`request.Context().Done()`. HTTPX cannot forcibly stop middleware that blocks
without respecting its context. Custom `RoundTripper` implementations and
response bodies must likewise respect the request context for cancellation and
timeouts to interrupt I/O.

For a successful response with a body, the operation context remains active
until the caller closes the body or reads it to EOF. The caller owns and must
close the final response body. If the operation deadline expires before
middleware returns a response, HTTPX closes that response body and returns the
context error.

## Policy ordering

The logical middleware chain wraps the circuit breaker, which wraps retry
execution. The breaker admits or rejects once per logical operation and
classifies only the final result after retries are complete. A retryable
failure followed by success is counted as a successful operation. Operation
hooks wrap the middleware chain, while attempt hooks observe each transport
exchange inside retries and redirects. Future attempt-level controls such as
rate limiting should apply to each transport attempt; their rejections must
not be counted as downstream failures by the breaker.
