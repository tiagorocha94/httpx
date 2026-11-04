package httpx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultInitialBackoff = 100 * time.Millisecond
	defaultMaxBackoff     = 2 * time.Second
)

// RetryPolicy reports whether a response or transport error is eligible for
// retry. It must not mutate request or response state and must be safe for
// concurrent use. HTTPX still enforces request replay safety.
type RetryPolicy func(*http.Request, *http.Response, error) bool

// RetryConfig configures opt-in retry behavior.
type RetryConfig struct {
	// MaxAttempts includes the initial HTTP execution. It must be at least 2.
	MaxAttempts int
	// InitialBackoff is the delay before the first retry. Zero uses 100ms.
	InitialBackoff time.Duration
	// MaxBackoff caps exponential backoff. Zero uses 2s.
	MaxBackoff time.Duration
	// Jitter is a symmetric randomization factor in [0, 1]. Zero disables it.
	Jitter float64
	// ShouldRetry overrides the default status and transport error policy.
	ShouldRetry RetryPolicy
}

// RetryExhaustedError reports that a retryable transport error remained after
// MaxAttempts executions.
type RetryExhaustedError struct {
	Attempts int
	Err      error
}

func (err *RetryExhaustedError) Error() string {
	return fmt.Sprintf("%v after %d attempts: %v", ErrRetryExhausted, err.Attempts, err.Err)
}

func (err *RetryExhaustedError) Unwrap() error {
	return err.Err
}

func (err *RetryExhaustedError) Is(target error) bool {
	return target == ErrRetryExhausted
}

type retryDoer struct {
	next   HTTPDoer
	config RetryConfig
}

func (doer *retryDoer) Do(request *http.Request) (*http.Response, error) {
	operation := operationFromContext(request.Context())
	if operation == nil {
		return doer.next.Do(request)
	}

	var response *http.Response
	var err error
	for attempt := 1; attempt <= doer.config.MaxAttempts; attempt++ {
		attemptRequest := request.Clone(request.Context())
		if attempt > 1 {
			if !requestReplayable(request) {
				return response, err
			}
			if request.Body != nil && request.Body != http.NoBody {
				attemptRequest.Body, err = request.GetBody()
				if err != nil {
					return nil, fmt.Errorf("httpx: recreate request body for retry: %w", err)
				}
				if attemptRequest.Body == nil {
					return nil, errors.New("httpx: recreate request body for retry: GetBody returned nil")
				}
			}
		}

		response, err = doer.next.Do(attemptRequest)
		if request.Context().Err() != nil {
			return response, request.Context().Err()
		}
		if !doer.retryable(request, response, err) || !requestReplayable(request) {
			return response, err
		}
		if attempt == doer.config.MaxAttempts {
			if err != nil {
				return response, &RetryExhaustedError{Attempts: attempt, Err: err}
			}
			return response, nil
		}

		delay := retryDelay(doer.config, attempt, response, time.Now())
		if response != nil {
			if response.Body != nil && response.Body != http.NoBody {
				if closeErr := response.Body.Close(); closeErr != nil {
					return nil, fmt.Errorf("httpx: close intermediate response before retry: %w", closeErr)
				}
			}
		}
		if err := waitForRetry(request.Context(), delay); err != nil {
			return nil, err
		}
	}
	return response, err
}

func (doer *retryDoer) retryable(request *http.Request, response *http.Response, err error) bool {
	if isPolicyRejection(err) {
		return false
	}
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return false
	}
	if doer.config.ShouldRetry != nil {
		return doer.config.ShouldRetry(request, response, err)
	}
	if err != nil {
		var networkError net.Error
		return errors.As(err, &networkError)
	}
	if response == nil {
		return false
	}
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func requestReplayable(request *http.Request) bool {
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut,
		http.MethodDelete, http.MethodTrace:
	default:
		return false
	}
	return request.Body == nil || request.Body == http.NoBody || request.GetBody != nil
}

func normalizeRetryConfig(config *RetryConfig) *RetryConfig {
	if config == nil {
		return nil
	}
	normalized := *config
	if normalized.InitialBackoff == 0 {
		normalized.InitialBackoff = defaultInitialBackoff
	}
	if normalized.MaxBackoff == 0 {
		normalized.MaxBackoff = defaultMaxBackoff
	}
	return &normalized
}

func (config RetryConfig) validate() error {
	if config.MaxAttempts < 2 {
		return errors.New("max attempts must be at least 2")
	}
	if config.InitialBackoff < 0 || config.MaxBackoff < 0 {
		return errors.New("backoff durations must not be negative")
	}
	if config.MaxBackoff < config.InitialBackoff {
		return errors.New("max backoff must not be less than initial backoff")
	}
	if math.IsNaN(config.Jitter) || config.Jitter < 0 || config.Jitter > 1 {
		return errors.New("jitter must be between 0 and 1")
	}
	return nil
}

func backoffDelay(config RetryConfig, retryNumber int) time.Duration {
	delay := config.InitialBackoff
	for i := 1; i < retryNumber && delay < config.MaxBackoff; i++ {
		if delay > config.MaxBackoff/2 {
			delay = config.MaxBackoff
		} else {
			delay *= 2
		}
	}
	if delay > config.MaxBackoff {
		delay = config.MaxBackoff
	}
	if delay == 0 || config.Jitter == 0 {
		return delay
	}
	return jitteredDelay(delay, config.Jitter, rand.Float64())
}

func retryDelay(config RetryConfig, retryNumber int, response *http.Response, now time.Time) time.Duration {
	delay := backoffDelay(config, retryNumber)
	if response == nil {
		return delay
	}
	if retryAfter, ok := parseRetryAfter(response.Header.Get("Retry-After"), now); ok && retryAfter > delay {
		return retryAfter
	}
	return delay
}

func jitteredDelay(delay time.Duration, jitter, sample float64) time.Duration {
	factor := 1 + (2*sample-1)*jitter
	randomized := float64(delay) * factor
	if randomized >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(randomized)
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		maxSeconds := uint64(math.MaxInt64 / int64(time.Second))
		if seconds > maxSeconds {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if delay < 0 {
		return 0, true
	}
	return delay, true
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
