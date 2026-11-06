# Structured logging

Logging is disabled unless `WithLogging` is configured. HTTPX uses Go's
standard `log/slog` package and emits records for logical operations and
individual transport attempts.

## Configuration

```go
client, err := httpx.New(httpx.WithLogging(httpx.LoggingConfig{
	Logger:     slog.Default(),
	Dependency: "payments",
	Level:      slog.LevelInfo,
}))
```

`Logger` is required. `Dependency` must be a non-empty, non-sensitive
identifier. `Level` is applied to both operation and attempt records; its zero
value is `slog.LevelInfo`. Logger handlers must be safe for concurrent use as
required by `log/slog`.

## Records and fields

Each logical operation emits `http operation completed` with dependency,
request ID, method, URL origin, status code, total duration, attempt count,
and error classification. Each transport exchange emits an
`http attempt completed` record with the corresponding fields, plus the
attempt number and attempt duration.

`error_class` contains a stable category such as `none`,
`circuit_breaker_rejection`, `rate_limit_rejection`, `policy_rejection`,
`context_canceled`, `deadline_exceeded`, `retry_exhausted`, or
`network_error`. Other errors are represented by their Go error type. Raw
error messages are not logged because transport errors may include sensitive
URLs or credentials.

## Sensitive data and bodies

The `url_origin` field contains only scheme and host. Userinfo, path, query,
and fragment are omitted because any may contain credentials or identifiers.
The configured dependency name and request ID are logged as provided; do not
put secrets in either value.

HTTPX never logs request or response bodies. It does not read, buffer, clone,
or otherwise change body handling for logging. The caller retains the standard
`net/http` responsibility for closing the final response body. Lifecycle hooks
are separate from logging: hook URL fields contain the original URL and should
be redacted by applications before recording; see [Hooks](HOOKS.md).
