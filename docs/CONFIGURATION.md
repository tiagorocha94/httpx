# Client configuration

`httpx.New` builds a concurrency-safe client with the standard
`http.Client.Do` request and response behavior:

```go
client, err := httpx.New(
	httpx.WithTimeout(10*time.Second),
	httpx.WithTransport(&http.Transport{
		MaxIdleConnsPerHost: 20,
	}),
)
if err != nil {
	return err
}
```

The client implements `Do(*http.Request) (*http.Response, error)`, so it can be
used wherever that standard-library interface is accepted. Non-2xx status
codes are returned as responses, not converted to errors. The caller must
close the final response body.

## Options and unified configuration

Use individual options for common settings:

- `WithTimeout` sets the maximum duration for a logical operation, including
  middleware, redirects, and response-body reading. Zero disables the
  client-level timeout; negative durations are invalid. Middleware and custom
  transports must honor the request context for the timeout to interrupt
  their work.
- `WithTransport` accepts an `*http.Transport`. HTTPX clones it during client
  construction, so later changes to the supplied transport do not reconfigure
  the client. Configure connection pooling, TLS, proxy behavior, dialing, and
  other standard transport behavior on this value.
- `WithRoundTripper` injects a custom `http.RoundTripper` as-is. The custom
  implementation must be safe for concurrent use. It cannot be combined with
  `WithTransport`.
- `WithConfig` supplies a unified `Config`. Later options override the
  corresponding fields.
- `WithRetry` opts in to bounded retries. Retries are disabled by default;
  see [Retries](RETRIES.md) for defaults and replay-safety rules.
- `WithCircuitBreaker` opts in to a circuit breaker. It observes one complete
  logical operation, including configured retries; see
  [Circuit breaker](CIRCUIT_BREAKER.md) for defaults and state behavior.

Invalid combinations, policy settings, and negative timeouts return an error
matching `httpx.ErrInvalidConfig`.

## Defaults and ownership

With no transport option, HTTPX clones `http.DefaultTransport` when it is an
`*http.Transport`. If the standard default has been replaced with another
`RoundTripper`, that implementation is used as provided. Retries and other
execution policies are not enabled unless explicitly configured.

Each client owns its cloned transport configuration. Call
`Client.CloseIdleConnections` when the client is no longer needed or when
idle pooled connections should be closed. The client does not close response
bodies for the caller.

The zero-value timeout is zero, matching `http.Client`: there is no
client-level timeout. Standard transport defaults are inherited by cloning
`http.DefaultTransport`; there is no additional custom pool or TLS default.

## Timeouts and request context

The request's context remains authoritative for cancellation and deadlines.
The configured client timeout is an additional upper bound across middleware,
redirects, and response reading. The earlier of the request context deadline
and the client timeout applies. The derived operation context remains active
until the response body is closed or reaches EOF.

Use `http.NewRequestWithContext` (or `request.WithContext`) to associate
cancellation or a request-specific deadline with an operation.
An injected custom `RoundTripper` is responsible for respecting the request
context, as required by the `net/http` contract. If it returns a response body
that can block while reading, that body must also honor the request context.
See [Execution](EXECUTION.md) for the middleware contract.
