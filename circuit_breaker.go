package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	defaultBreakerFailureThreshold = 5
	defaultBreakerRecoveryTimeout  = 30 * time.Second
	defaultHalfOpenMaxRequests     = 1
)

type circuitBreakerState uint8

const (
	breakerClosed circuitBreakerState = iota
	breakerOpen
	breakerHalfOpen
)

// CircuitBreakerPolicy reports whether a completed logical operation counts
// as a downstream failure. It must not mutate its arguments and must be safe
// for concurrent use.
type CircuitBreakerPolicy func(*http.Request, *http.Response, error) bool

// CircuitBreakerConfig configures an opt-in circuit breaker.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive failed logical operations
	// required to open the breaker. Zero uses 5.
	FailureThreshold int
	// RecoveryTimeout is how long the breaker remains open before allowing
	// half-open probes. Zero uses 30 seconds.
	RecoveryTimeout time.Duration
	// HalfOpenMaxRequests is the maximum number of concurrent logical probes.
	// Zero uses 1.
	HalfOpenMaxRequests int
	// ShouldTrip overrides the default failure classification.
	ShouldTrip CircuitBreakerPolicy
}

type circuitBreaker struct {
	mu                  sync.Mutex
	config              CircuitBreakerConfig
	state               circuitBreakerState
	consecutiveFailures int
	halfOpenInFlight    int
	openUntil           time.Time
	generation          uint64
	now                 func() time.Time
}

type circuitBreakerPermit struct {
	generation uint64
	halfOpen   bool
}

type circuitBreakerOutcome uint8

const (
	breakerNeutral circuitBreakerOutcome = iota
	breakerSuccess
	breakerFailure
)

type circuitBreakerDoer struct {
	next    HTTPDoer
	breaker *circuitBreaker
}

type policyRejectionMarker interface {
	policyRejection()
}

type policyRejectionError struct {
	err error
}

func (err *policyRejectionError) Error() string {
	return err.err.Error()
}

func (err *policyRejectionError) Unwrap() error {
	return err.err
}

func (*policyRejectionError) policyRejection() {}

func newPolicyRejection(err error) error {
	return &policyRejectionError{err: err}
}

func isPolicyRejection(err error) bool {
	var rejection policyRejectionMarker
	return errors.As(err, &rejection)
}

func (doer *circuitBreakerDoer) Do(request *http.Request) (*http.Response, error) {
	if request == nil {
		return doer.next.Do(nil)
	}
	permit, err := doer.breaker.acquire(request.Context())
	if err != nil {
		if errors.Is(err, ErrCircuitBreakerOpen) {
			return nil, newPolicyRejection(err)
		}
		return nil, err
	}

	response, executionErr := doer.next.Do(request)
	if request.Context().Err() != nil {
		executionErr = request.Context().Err()
	}
	outcome := classifyCircuitBreakerOutcome(request, response, executionErr, doer.breaker.config.ShouldTrip)
	doer.breaker.complete(permit, outcome)
	return response, executionErr
}

func newCircuitBreaker(config CircuitBreakerConfig) *circuitBreaker {
	return &circuitBreaker{
		config: config,
		state:  breakerClosed,
		now:    time.Now,
	}
}

func (breaker *circuitBreaker) acquire(ctx context.Context) (circuitBreakerPermit, error) {
	if err := ctx.Err(); err != nil {
		return circuitBreakerPermit{}, err
	}

	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	now := breaker.now()
	switch breaker.state {
	case breakerOpen:
		if now.Before(breaker.openUntil) {
			return circuitBreakerPermit{}, ErrCircuitBreakerOpen
		}
		breaker.state = breakerHalfOpen
		breaker.halfOpenInFlight = 0
		breaker.generation++
	case breakerHalfOpen:
		if breaker.halfOpenInFlight >= breaker.config.HalfOpenMaxRequests {
			return circuitBreakerPermit{}, ErrCircuitBreakerOpen
		}
	}

	permit := circuitBreakerPermit{
		generation: breaker.generation,
		halfOpen:   breaker.state == breakerHalfOpen,
	}
	if permit.halfOpen {
		breaker.halfOpenInFlight++
	}
	return permit, nil
}

func (breaker *circuitBreaker) complete(permit circuitBreakerPermit, outcome circuitBreakerOutcome) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	if permit.generation != breaker.generation {
		return
	}
	now := breaker.now()
	if permit.halfOpen {
		breaker.halfOpenInFlight--
		if outcome == breakerFailure {
			breaker.open(now)
			return
		}
		if outcome == breakerSuccess {
			breaker.close()
		}
		return
	}
	if breaker.state != breakerClosed {
		return
	}
	if outcome == breakerNeutral {
		return
	}
	if outcome == breakerSuccess {
		breaker.consecutiveFailures = 0
		return
	}
	breaker.consecutiveFailures++
	if breaker.consecutiveFailures >= breaker.config.FailureThreshold {
		breaker.open(now)
	}
}

func (breaker *circuitBreaker) open(now time.Time) {
	breaker.state = breakerOpen
	breaker.consecutiveFailures = 0
	breaker.halfOpenInFlight = 0
	breaker.openUntil = now.Add(breaker.config.RecoveryTimeout)
	breaker.generation++
}

func (breaker *circuitBreaker) close() {
	breaker.state = breakerClosed
	breaker.consecutiveFailures = 0
	breaker.halfOpenInFlight = 0
	breaker.openUntil = time.Time{}
	breaker.generation++
}

func classifyCircuitBreakerOutcome(request *http.Request, response *http.Response, err error, policy CircuitBreakerPolicy) circuitBreakerOutcome {
	if isPolicyRejection(err) || errors.Is(err, ErrCircuitBreakerOpen) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return breakerNeutral
	}
	if policy != nil {
		if policy(request.Clone(request.Context()), cloneResponseForPolicy(response), err) {
			return breakerFailure
		}
		return breakerSuccess
	}
	if err != nil {
		return breakerFailure
	}
	if response != nil && response.StatusCode >= http.StatusInternalServerError {
		return breakerFailure
	}
	return breakerSuccess
}

func cloneResponseForPolicy(response *http.Response) *http.Response {
	if response == nil {
		return nil
	}
	cloned := new(http.Response)
	*cloned = *response
	cloned.Header = response.Header.Clone()
	cloned.Trailer = response.Trailer.Clone()
	if response.Request != nil {
		cloned.Request = response.Request.Clone(response.Request.Context())
	}
	return cloned
}

func normalizeCircuitBreakerConfig(config *CircuitBreakerConfig) *CircuitBreakerConfig {
	if config == nil {
		return nil
	}
	normalized := *config
	if normalized.FailureThreshold == 0 {
		normalized.FailureThreshold = defaultBreakerFailureThreshold
	}
	if normalized.RecoveryTimeout == 0 {
		normalized.RecoveryTimeout = defaultBreakerRecoveryTimeout
	}
	if normalized.HalfOpenMaxRequests == 0 {
		normalized.HalfOpenMaxRequests = defaultHalfOpenMaxRequests
	}
	return &normalized
}

func (config CircuitBreakerConfig) validate() error {
	if config.FailureThreshold < 1 {
		return fmt.Errorf("failure threshold must be positive")
	}
	if config.RecoveryTimeout <= 0 {
		return fmt.Errorf("recovery timeout must be positive")
	}
	if config.HalfOpenMaxRequests < 1 {
		return fmt.Errorf("half-open max requests must be positive")
	}
	return nil
}
