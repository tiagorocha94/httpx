package httpx

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiterAllowsBurstAndRefills(t *testing.T) {
	now := time.Now()
	limiter := newRateLimiter(&RateLimitConfig{
		RequestsPerSecond: 2,
		Burst:             2,
	})
	limiter.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if err := limiter.allow(context.Background()); err != nil {
			t.Fatalf("allow initial burst token %d: %v", i+1, err)
		}
	}
	if err := limiter.allow(context.Background()); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("allow beyond burst error = %v, want ErrRateLimitExceeded", err)
	}

	now = now.Add(500 * time.Millisecond)
	if err := limiter.allow(context.Background()); err != nil {
		t.Fatalf("allow after one token refilled: %v", err)
	}
}

func TestRateLimiterConcurrentBurstCapacity(t *testing.T) {
	const burst = 4
	limiter := newRateLimiter(&RateLimitConfig{
		RequestsPerSecond: 1,
		Burst:             burst,
	})
	now := time.Now()
	limiter.now = func() time.Time { return now }

	const requests = 64
	start := make(chan struct{})
	var admitted atomic.Int64
	var wait sync.WaitGroup
	for i := 0; i < requests; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if err := limiter.allow(context.Background()); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrRateLimitExceeded) {
				t.Errorf("allow() error = %v, want ErrRateLimitExceeded", err)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := admitted.Load(); got != burst {
		t.Fatalf("admitted requests = %d, want burst capacity %d", got, burst)
	}
}

func TestRateLimiterCanceledRequestDoesNotConsumeToken(t *testing.T) {
	limiter := newRateLimiter(&RateLimitConfig{
		RequestsPerSecond: 1,
		Burst:             1,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.allow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("allow canceled context error = %v, want context.Canceled", err)
	}
	if err := limiter.allow(context.Background()); err != nil {
		t.Fatalf("allow token after canceled request: %v", err)
	}
}

func TestRateLimitRejectsImmediatelyAndDoesNotRunAttemptHooks(t *testing.T) {
	var calls atomic.Int64
	var beforeAttempts atomic.Int64
	client, err := New(
		WithRateLimit(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}),
		WithHooks(Hooks{
			BeforeAttempt: func(AttemptEvent) {
				beforeAttempts.Add(1)
			},
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return testResponse(http.StatusOK, "ok"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("first Do() error = %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("second Do() error = %v, want ErrRateLimitExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
	if got := beforeAttempts.Load(); got != 1 {
		t.Fatalf("BeforeAttempt calls = %d, want 1", got)
	}
}

func TestRateLimitAppliesToRedirectAttempts(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithRateLimit(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := testResponse(http.StatusFound, "")
			response.Header.Set("Location", "/redirected")
			response.Request = request
			return response, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("Do() error = %v, want ErrRateLimitExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

func TestRateLimitRejectionIsNotRetried(t *testing.T) {
	var transportCalls atomic.Int64
	var retryPolicyCalls atomic.Int64
	client, err := New(
		WithRateLimit(RateLimitConfig{RequestsPerSecond: 0.01, Burst: 1}),
		WithRetry(RetryConfig{
			MaxAttempts:    3,
			InitialBackoff: time.Nanosecond,
			MaxBackoff:     time.Nanosecond,
			ShouldRetry: func(*http.Request, *http.Response, error) bool {
				retryPolicyCalls.Add(1)
				return true
			},
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return testResponse(http.StatusServiceUnavailable, "retry"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("Do() error = %v, want ErrRateLimitExceeded", err)
	}
	if got := transportCalls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
	if got := retryPolicyCalls.Load(); got != 1 {
		t.Fatalf("retry policy calls = %d, want 1 before rate-limit rejection", got)
	}
}

func TestRateLimitRejectionDoesNotTripCircuitBreaker(t *testing.T) {
	now := time.Now()
	downstreamErr := errors.New("downstream failure")
	var transportCalls atomic.Int64
	client, err := New(
		WithRateLimit(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}),
		WithCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if transportCalls.Add(1) == 1 {
				return testResponse(http.StatusOK, "ok"), nil
			}
			return nil, downstreamErr
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*operationRoundTripper)
	transport.limiter.now = func() time.Time { return now }

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("first Do() error = %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("rate-limited Do() error = %v, want ErrRateLimitExceeded", err)
	}

	now = now.Add(time.Second)
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, downstreamErr) {
		t.Fatalf("downstream Do() error = %v, want downstream failure", err)
	}
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("following Do() error = %v, want ErrCircuitBreakerOpen", err)
	}
	if got := transportCalls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestNewRejectsInvalidRateLimitConfig(t *testing.T) {
	tests := []RateLimitConfig{
		{RequestsPerSecond: 0, Burst: 1},
		{RequestsPerSecond: -1, Burst: 1},
		{RequestsPerSecond: math.NaN(), Burst: 1},
		{RequestsPerSecond: math.Inf(1), Burst: 1},
		{RequestsPerSecond: 1, Burst: 0},
	}
	for _, config := range tests {
		if _, err := New(WithRateLimit(config)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("New(%+v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
}

func TestRateLimitConfigCanBeSetWithConfig(t *testing.T) {
	config := Config{
		RateLimit: &RateLimitConfig{
			RequestsPerSecond: 1,
			Burst:             1,
		},
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return testResponse(http.StatusOK, "ok"), nil
		}),
	}
	client, err := New(WithConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	if client.client.Transport.(*operationRoundTripper).limiter == nil {
		t.Fatal("WithConfig did not enable the rate limiter")
	}
}

func TestRateLimiterStateIsClientScoped(t *testing.T) {
	newLimitedClient := func() *Client {
		client, err := New(
			WithRateLimit(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}),
			WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return testResponse(http.StatusOK, "ok"), nil
			})),
		)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	first := newLimitedClient()
	second := newLimitedClient()
	for _, client := range []*Client{first, second} {
		response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
		if err != nil {
			t.Fatalf("first request for client: %v", err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("second request for first client error = %v, want ErrRateLimitExceeded", err)
	}
}
