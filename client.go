package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"
)

var ErrInvalidConfig = errors.New("httpx: invalid configuration")

// ErrInvalidResponse indicates that middleware returned an invalid response.
var ErrInvalidResponse = errors.New("httpx: middleware returned an invalid response")

// ErrRetryExhausted indicates that a retryable transport error persisted
// through the configured maximum number of attempts.
var ErrRetryExhausted = errors.New("httpx: retry attempts exhausted")

// ErrCircuitBreakerOpen indicates that a request was rejected while the
// circuit breaker was open or its half-open capacity was full.
var ErrCircuitBreakerOpen = errors.New("httpx: circuit breaker is open")

// ErrRateLimitExceeded indicates that a request was rejected because no
// token was available in the configured rate limiter.
var ErrRateLimitExceeded = errors.New("httpx: rate limit exceeded")

// Config contains immutable settings used to construct a Client.
//
// Transport is cloned during construction. RoundTripper is used as provided
// and must satisfy the concurrency-safety requirements of http.RoundTripper.
// Set at most one of Transport and RoundTripper.
type Config struct {
	Timeout      time.Duration
	Transport    *http.Transport
	RoundTripper http.RoundTripper
	Middleware   []Middleware
	// RequestIDHeader is the header used for operation IDs. It defaults to
	// "X-Request-ID".
	RequestIDHeader string
	// Hooks contains the callbacks used to observe operations and attempts.
	Hooks Hooks
	// Retry contains the retry policy. A nil value disables retries.
	Retry *RetryConfig
	// CircuitBreaker contains the breaker policy. A nil value disables it.
	CircuitBreaker *CircuitBreakerConfig
	// RateLimit contains the attempt-level rate limit. A nil value disables it.
	RateLimit *RateLimitConfig
	// Bulkhead contains the logical-operation concurrency limit. A nil value
	// disables it.
	Bulkhead *BulkheadConfig
	// Logging contains structured logging configuration. A nil value disables
	// logging.
	Logging *LoggingConfig
}

// Option configures a Client.
type Option func(*Config) error

// Client executes HTTP requests using a configured transport.
//
// A Client is safe for concurrent use. Its final response body belongs to the
// caller and must be closed by the caller.
type Client struct {
	client          *http.Client
	doer            HTTPDoer
	timeout         time.Duration
	requestIDHeader string
	hooks           Hooks
	logging         *LoggingConfig
}

// New constructs a Client using the supplied options.
//
// The default transport is cloned from http.DefaultTransport when it is an
// *http.Transport, so the Client owns an independently configured transport.
// Retry, circuit-breaker, rate-limit, bulkhead, and logging features are
// disabled by default.
func New(options ...Option) (*Client, error) {
	config := Config{}
	for i, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: option %d is nil", ErrInvalidConfig, i)
		}
		if err := option(&config); err != nil {
			return nil, fmt.Errorf("%w: option %d: %v", ErrInvalidConfig, i, err)
		}
	}
	config.Retry = normalizeRetryConfig(config.Retry)
	config.CircuitBreaker = normalizeCircuitBreakerConfig(config.CircuitBreaker)
	config.RateLimit = cloneRateLimitConfig(config.RateLimit)
	config.Bulkhead = cloneBulkheadConfig(config.Bulkhead)
	config.Logging = cloneLoggingConfig(config.Logging)

	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.RequestIDHeader == "" {
		config.RequestIDHeader = defaultRequestIDHeader
	} else {
		config.RequestIDHeader = http.CanonicalHeaderKey(config.RequestIDHeader)
	}

	roundTripper, err := config.roundTripper()
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{
		Transport: &operationRoundTripper{
			next:    roundTripper,
			limiter: newRateLimiter(config.RateLimit),
		},
	}
	var doer HTTPDoer = httpClient
	if config.Retry != nil {
		doer = &retryDoer{
			next:   doer,
			config: *config.Retry,
		}
	}
	if config.CircuitBreaker != nil {
		doer = &circuitBreakerDoer{
			next:    doer,
			breaker: newCircuitBreaker(*config.CircuitBreaker),
		}
	}
	if config.Bulkhead != nil {
		doer = &bulkheadDoer{
			next:     doer,
			bulkhead: newBulkhead(*config.Bulkhead),
		}
	}
	doer, err = config.operationDoer(doer)
	if err != nil {
		return nil, err
	}

	return &Client{
		client:          httpClient,
		doer:            doer,
		timeout:         config.Timeout,
		requestIDHeader: config.RequestIDHeader,
		hooks:           config.Hooks,
		logging:         config.Logging,
	}, nil
}

// Do sends the request and returns the response using net/http client
// semantics. It does not interpret HTTP status codes as errors.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if request == nil {
		return c.client.Do(nil)
	}

	requestID, err := requestIDFor(request, c.requestIDHeader)
	if err != nil {
		return nil, err
	}
	operation := &operationState{
		startedAt:       time.Now(),
		requestID:       requestID,
		requestIDHeader: c.requestIDHeader,
		hooks:           c.hooks,
		logger:          c.logging,
		event:           operationEvent(request, requestID),
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(request.Context(), c.timeout)
	} else {
		ctx, cancel = context.WithCancel(request.Context())
	}
	ctx = context.WithValue(ctx, operationContextKey{}, operation)
	clonedRequest := request.Clone(ctx)
	if clonedRequest.Header == nil {
		clonedRequest.Header = make(http.Header)
	}
	if requestID != "" {
		clonedRequest.Header.Set(operation.requestIDHeader, requestID)
	}

	if operation.hooks.BeforeOperation != nil {
		operation.hooks.BeforeOperation(operation.event)
	}
	response, err := c.doer.Do(clonedRequest)
	if err != nil {
		cancel()
		operation.finish(response, err)
		return response, err
	}
	if ctx.Err() != nil {
		var closeErr error
		if response != nil && response.Body != nil {
			closeErr = response.Body.Close()
		}
		cancel()
		if closeErr != nil {
			err = errors.Join(ctx.Err(), closeErr)
			operation.finish(response, err)
			return nil, err
		}
		err = ctx.Err()
		operation.finish(response, err)
		return nil, err
	}
	if response == nil {
		cancel()
		operation.finish(nil, ErrInvalidResponse)
		return nil, ErrInvalidResponse
	}
	if response.Body == nil {
		cancel()
		operation.finish(response, ErrInvalidResponse)
		return nil, ErrInvalidResponse
	}
	if response.Body == http.NoBody {
		cancel()
		operation.finish(response, nil)
		return response, nil
	}
	response.Body = &operationBody{
		body:   response.Body,
		ctx:    ctx,
		cancel: cancel,
	}
	operation.finish(response, nil)
	return response, nil
}

// CloseIdleConnections closes idle connections held by the underlying
// transport, when it supports idle connection cleanup.
func (c *Client) CloseIdleConnections() {
	c.client.CloseIdleConnections()
}

// WithConfig replaces the current configuration with config. Options supplied
// after WithConfig can override individual settings.
func WithConfig(config Config) Option {
	return func(current *Config) error {
		*current = config
		current.Middleware = append([]Middleware(nil), config.Middleware...)
		current.Retry = cloneRetryConfig(config.Retry)
		current.CircuitBreaker = cloneCircuitBreakerConfig(config.CircuitBreaker)
		current.RateLimit = cloneRateLimitConfig(config.RateLimit)
		current.Bulkhead = cloneBulkheadConfig(config.Bulkhead)
		current.Logging = cloneLoggingConfig(config.Logging)
		return nil
	}
}

// WithRequestIDHeader sets the header used to carry a logical operation's
// request ID. The default is "X-Request-ID". An existing non-empty value on
// the request is preserved; otherwise HTTPX generates an ID.
func WithRequestIDHeader(name string) Option {
	return func(config *Config) error {
		if !validHeaderName(name) {
			return fmt.Errorf("request ID header name %q is invalid", name)
		}
		config.RequestIDHeader = name
		return nil
	}
}

// WithRetry enables retries using config. MaxAttempts includes the initial
// execution. Requests are retried only when their method is idempotent and
// any body can be recreated.
func WithRetry(config RetryConfig) Option {
	return func(current *Config) error {
		current.Retry = normalizeRetryConfig(&config)
		return nil
	}
}

// WithCircuitBreaker enables a circuit breaker. It observes one complete
// logical operation, including all retries configured for that operation.
func WithCircuitBreaker(config CircuitBreakerConfig) Option {
	return func(current *Config) error {
		current.CircuitBreaker = normalizeCircuitBreakerConfig(&config)
		return nil
	}
}

// WithRateLimit enables a client-scoped token-bucket rate limit for individual
// transport attempts. Requests that exceed the available burst are rejected
// immediately with ErrRateLimitExceeded.
func WithRateLimit(config RateLimitConfig) Option {
	return func(current *Config) error {
		current.RateLimit = cloneRateLimitConfig(&config)
		return nil
	}
}

// WithBulkhead enables a client-scoped concurrency limit for logical
// operations. Acquisition waits until capacity is available or the request
// context is canceled.
func WithBulkhead(config BulkheadConfig) Option {
	return func(current *Config) error {
		current.Bulkhead = cloneBulkheadConfig(&config)
		return nil
	}
}

// WithLogging enables structured operation and attempt logs using log/slog.
// The dependency name must be a non-empty, non-sensitive identifier.
func WithLogging(config LoggingConfig) Option {
	return func(current *Config) error {
		current.Logging = cloneLoggingConfig(&config)
		return nil
	}
}

// WithTimeout sets the maximum duration for a logical operation, including
// redirects, middleware, and reading the response body. Middleware must honor
// the request context for the timeout to interrupt its work. A zero timeout
// disables this client-level limit; negative values are invalid.
func WithTimeout(timeout time.Duration) Option {
	return func(config *Config) error {
		config.Timeout = timeout
		return nil
	}
}

// WithTransport sets the HTTP transport configuration. The transport is
// cloned when New constructs the Client. It cannot be combined with
// WithRoundTripper.
func WithTransport(transport *http.Transport) Option {
	return func(config *Config) error {
		if transport == nil {
			return errors.New("transport must not be nil")
		}
		config.Transport = transport
		return nil
	}
}

// WithRoundTripper sets a custom http.RoundTripper. The RoundTripper is used
// as provided and cannot be combined with WithTransport.
func WithRoundTripper(roundTripper http.RoundTripper) Option {
	return func(config *Config) error {
		if isNil(roundTripper) {
			return errors.New("round tripper must not be nil")
		}
		config.RoundTripper = roundTripper
		return nil
	}
}

func (config Config) validate() error {
	if config.Timeout < 0 {
		return fmt.Errorf("%w: timeout must not be negative", ErrInvalidConfig)
	}
	if config.RoundTripper != nil && isNil(config.RoundTripper) {
		return fmt.Errorf("%w: round tripper must not be nil", ErrInvalidConfig)
	}
	if config.Transport != nil && config.RoundTripper != nil {
		return fmt.Errorf("%w: transport and round tripper are mutually exclusive", ErrInvalidConfig)
	}
	if config.RequestIDHeader != "" && !validHeaderName(config.RequestIDHeader) {
		return fmt.Errorf("%w: request ID header name %q is invalid", ErrInvalidConfig, config.RequestIDHeader)
	}
	if config.Retry != nil {
		if err := config.Retry.validate(); err != nil {
			return fmt.Errorf("%w: retry: %v", ErrInvalidConfig, err)
		}
	}
	if config.CircuitBreaker != nil {
		if err := config.CircuitBreaker.validate(); err != nil {
			return fmt.Errorf("%w: circuit breaker: %v", ErrInvalidConfig, err)
		}
	}
	if config.RateLimit != nil {
		if err := config.RateLimit.validate(); err != nil {
			return fmt.Errorf("%w: rate limit: %v", ErrInvalidConfig, err)
		}
	}
	if config.Bulkhead != nil {
		if err := config.Bulkhead.validate(); err != nil {
			return fmt.Errorf("%w: bulkhead: %v", ErrInvalidConfig, err)
		}
	}
	if config.Logging != nil {
		if err := config.Logging.validate(); err != nil {
			return fmt.Errorf("%w: logging: %v", ErrInvalidConfig, err)
		}
	}
	for i, middleware := range config.Middleware {
		if middleware == nil {
			return fmt.Errorf("%w: middleware %d is nil", ErrInvalidConfig, i)
		}
	}
	return nil
}

func cloneRetryConfig(config *RetryConfig) *RetryConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func cloneCircuitBreakerConfig(config *CircuitBreakerConfig) *CircuitBreakerConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func cloneRateLimitConfig(config *RateLimitConfig) *RateLimitConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func cloneBulkheadConfig(config *BulkheadConfig) *BulkheadConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func cloneLoggingConfig(config *LoggingConfig) *LoggingConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (config Config) roundTripper() (http.RoundTripper, error) {
	if config.RoundTripper != nil {
		return config.RoundTripper, nil
	}
	if config.Transport != nil {
		return config.Transport.Clone(), nil
	}

	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		return defaultTransport.Clone(), nil
	}
	if http.DefaultTransport == nil {
		return nil, fmt.Errorf("%w: http.DefaultTransport is nil", ErrInvalidConfig)
	}
	return http.DefaultTransport, nil
}
