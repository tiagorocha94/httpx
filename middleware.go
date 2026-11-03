package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPDoer sends an HTTP request and returns its response.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Middleware wraps a logical HTTP operation.
//
// Middleware is applied in the order supplied to New: the first middleware is
// the outermost wrapper. Middleware must be safe for concurrent use, preserve
// request and response body semantics, and call next at most once. It may
// short-circuit by returning a response or error without calling next.
type Middleware func(next HTTPDoer) HTTPDoer

type operationContextKey struct{}

type operationState struct {
	startedAt time.Time
	attempts  atomic.Uint64
}

type operationBody struct {
	body   io.ReadCloser
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (body *operationBody) Read(buffer []byte) (int, error) {
	if err := body.ctx.Err(); err != nil {
		body.stop()
		return 0, err
	}
	n, err := body.body.Read(buffer)
	if err == io.EOF {
		body.stop()
	}
	if contextErr := body.ctx.Err(); contextErr != nil {
		if err == nil {
			err = contextErr
		}
		body.stop()
	}
	return n, err
}

func (body *operationBody) Close() error {
	err := body.body.Close()
	body.stop()
	return err
}

func (body *operationBody) stop() {
	body.once.Do(body.cancel)
}

func operationFromContext(ctx context.Context) *operationState {
	operation, _ := ctx.Value(operationContextKey{}).(*operationState)
	return operation
}

type operationRoundTripper struct {
	next http.RoundTripper
}

func (transport *operationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if operation := operationFromContext(request.Context()); operation != nil {
		operation.attempts.Add(1)
	}
	return transport.next.RoundTrip(request)
}

func (transport *operationRoundTripper) CloseIdleConnections() {
	if closer, ok := transport.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// WithMiddleware appends middleware to the logical-operation chain. The first
// middleware supplied is the outermost wrapper. Nil middleware is invalid.
func WithMiddleware(middleware ...Middleware) Option {
	copied := append([]Middleware(nil), middleware...)
	return func(config *Config) error {
		for i, item := range copied {
			if item == nil {
				return fmt.Errorf("middleware %d must not be nil", i)
			}
		}
		config.Middleware = append(config.Middleware, copied...)
		return nil
	}
}

func (config Config) operationDoer(next HTTPDoer) (HTTPDoer, error) {
	for i := len(config.Middleware) - 1; i >= 0; i-- {
		next = config.Middleware[i](next)
		if isNil(next) {
			return nil, fmt.Errorf("%w: middleware %d returned a nil HTTPDoer", ErrInvalidConfig, i)
		}
	}
	return next, nil
}
