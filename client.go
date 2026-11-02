package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"
)

var ErrInvalidConfig = errors.New("httpx: invalid configuration")

// Config contains immutable settings used to construct a Client.
//
// Transport is cloned during construction. RoundTripper is used as provided
// and must satisfy the concurrency-safety requirements of http.RoundTripper.
// Set at most one of Transport and RoundTripper.
type Config struct {
	Timeout      time.Duration
	Transport    *http.Transport
	RoundTripper http.RoundTripper
}

// Option configures a Client.
type Option func(*Config) error

// Client executes HTTP requests using a configured transport.
//
// A Client is safe for concurrent use. Its final response body belongs to the
// caller and must be closed by the caller.
type Client struct {
	client *http.Client
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

	return &Client{
		client: &http.Client{
			Timeout:   config.Timeout,
			Transport: roundTripper,
		},
	}, nil
}

// Do sends the request and returns the response using net/http client
// semantics. It does not interpret HTTP status codes as errors.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request)
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
		return nil
	}
}

// WithTimeout sets the maximum duration for a complete request, including
// redirects and reading the response body. A zero timeout disables this
// client-level limit; negative values are invalid.
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
