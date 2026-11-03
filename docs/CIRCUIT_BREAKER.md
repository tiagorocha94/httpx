# Circuit breaker

The circuit breaker is disabled unless `WithCircuitBreaker` is configured. It
protects one client by tracking the final outcome of each logical operation.

## Configuration and classification

```go
client, err := httpx.New(httpx.WithCircuitBreaker(httpx.CircuitBreakerConfig{
	FailureThreshold:    5,
	RecoveryTimeout:     30 * time.Second,
	HalfOpenMaxRequests: 1,
}))
```

Defaults are five consecutive failures, a 30-second recovery timeout, and one
concurrent half-open probe. Explicit nonzero values must be positive; zero
selects the default. Each successful classification resets the
consecutive-failure count.

By default, errors other than context cancellation/deadline and HTTP responses
with status 500 or greater count as failures. Other HTTP statuses are
classified as success. Configure `ShouldTrip` to replace this policy. It
receives read-only request and response snapshots and the final error; it must
be safe for concurrent use and must not mutate its arguments or read/close the
response body.

Policy rejections are not downstream failures. The breaker rejects requests
with an error matching `ErrCircuitBreakerOpen` while open or when its
half-open capacity is full. Cancellation and deadline errors do not increase
the failure count or resolve a half-open probe. Successful classifications
reset the closed-state failure count.

## States and transitions

- **Closed:** requests are admitted. Consecutive classified failures open the
  breaker when they reach `FailureThreshold`; a success resets the count.
- **Open:** requests fail immediately with `ErrCircuitBreakerOpen`. After
  `RecoveryTimeout`, requests may enter half-open.
- **Half-open:** at most `HalfOpenMaxRequests` logical operations may probe
  concurrently. A classified failure reopens the breaker for another recovery
  interval. A success closes it and resets the failure count.

A probe admitted after recovery is allowed to finish. If concurrent probes are
configured, the first completed probe decides the transition; results from
other probes admitted under that previous state are ignored after the
transition. A neutral result such as cancellation releases its half-open slot
but leaves the breaker half-open for a later probe.

## Retry and operation semantics

Policy ordering is logical middleware, circuit breaker, retry, then HTTP
transport. The breaker admits once around the complete retry-enabled
`http.Client.Do` execution. It counts one final operation outcome, not each
attempt: an intermediate retry failure followed by a successful final
response does not count as a breaker failure. Retry exhaustion and other
final transport errors are classified as failures by default. Classification
happens when response headers or the final execution error are available;
errors encountered later while the caller reads the response body do not
change breaker state.

The half-open limit therefore counts concurrent logical operations, including
all retries and redirects within each. Circuit-breaker rejections do not run
retries or transport attempts. Operation and attempt hook behavior is
unchanged; see [Retries](RETRIES.md) and [Execution](EXECUTION.md).
