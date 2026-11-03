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
}

// Option configures a Client.
type Option func(*Config) error

// Client executes HTTP requests using a configured transport.
//
// A Client is safe for concurrent use. Its final response body belongs to the
// caller and must be closed by the caller.
type Client struct {
	client  *http.Client
	doer    HTTPDoer
	timeout time.Duration
}

// New constructs a Client using the supplied options.
//
// The default transport is cloned from http.DefaultTransport when it is an
// *http.Transport, so the Client owns an independently configured transport.
// Retries and other execution policies are not enabled by default.
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

	if err := config.validate(); err != nil {
		return nil, err
	}

	roundTripper, err := config.roundTripper()
	if err != nil {
		return nil, err
	}

	attemptTransport := &operationRoundTripper{next: roundTripper}
	httpClient := &http.Client{
		Transport: attemptTransport,
	}
	doer, err := config.operationDoer(httpClient)
	if err != nil {
		return nil, err
	}

	return &Client{
		client:  httpClient,
		doer:    doer,
		timeout: config.Timeout,
	}, nil
}

// Do sends the request and returns the response using net/http client
// semantics. It does not interpret HTTP status codes as errors.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if request == nil {
		return c.client.Do(nil)
	}

	operation := &operationState{startedAt: time.Now()}
	var ctx context.Context
	var cancel context.CancelFunc
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(request.Context(), c.timeout)
	} else {
		ctx, cancel = context.WithCancel(request.Context())
	}
	ctx = context.WithValue(ctx, operationContextKey{}, operation)
	clonedRequest := request.Clone(ctx)

	response, err := c.doer.Do(clonedRequest)
	if err != nil {
		cancel()
		return response, err
	}
	if ctx.Err() != nil {
		var closeErr error
		if response != nil && response.Body != nil {
			closeErr = response.Body.Close()
		}
		cancel()
		if closeErr != nil {
			return nil, errors.Join(ctx.Err(), closeErr)
		}
		return nil, ctx.Err()
	}
	if response == nil {
		cancel()
		return nil, ErrInvalidResponse
	}
	if response.Body == nil {
		cancel()
		return nil, ErrInvalidResponse
	}
	if response.Body == http.NoBody {
		cancel()
		return response, nil
	}
	response.Body = &operationBody{
		body:   response.Body,
		ctx:    ctx,
		cancel: cancel,
	}
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
	for i, middleware := range config.Middleware {
		if middleware == nil {
			return fmt.Errorf("%w: middleware %d is nil", ErrInvalidConfig, i)
		}
	}
	return nil
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
