package httpx

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreakerIsDisabledByDefault(t *testing.T) {
	var calls atomic.Int64
	client, err := New(WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testResponse(http.StatusServiceUnavailable, "unavailable"), nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("transport calls = %d, want 3 with breaker disabled", got)
	}
}

func TestCircuitBreakerOpensAtConsecutiveFailureThreshold(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 2}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return testResponse(http.StatusInternalServerError, "failure"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
		if err != nil {
			t.Fatalf("Do() call %d error = %v", i+1, err)
		}
		_ = response.Body.Close()
	}
	_, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("Do() error = %v, want ErrCircuitBreakerOpen", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestCircuitBreakerCountsConsecutiveFailuresOnly(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 2,
			ShouldTrip: func(_ *http.Request, response *http.Response, _ error) bool {
				return response != nil && response.StatusCode >= http.StatusInternalServerError
			},
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			switch calls.Add(1) {
			case 1, 3, 4:
				return testResponse(http.StatusServiceUnavailable, "failure"), nil
			default:
				return testResponse(http.StatusNotFound, "not found"), nil
			}
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []int{http.StatusServiceUnavailable, http.StatusNotFound, http.StatusServiceUnavailable, http.StatusServiceUnavailable} {
		response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
		if err != nil {
			t.Fatalf("Do() error before threshold = %v", err)
		}
		if response.StatusCode != status {
			t.Errorf("status = %d, want %d", response.StatusCode, status)
		}
		_ = response.Body.Close()
	}
	_, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("Do() error = %v, want breaker open after two consecutive failures", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("transport calls = %d, want 4", got)
	}
}

func TestCircuitBreakerDefaultClassificationTreatsClientErrorsAsSuccess(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return testResponse(http.StatusNotFound, "not found"), nil
			}
			return testResponse(http.StatusOK, "ok"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []int{http.StatusNotFound, http.StatusOK} {
		response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
		if err != nil {
			t.Fatalf("Do() error = %v, want status response", err)
		}
		if response.StatusCode != status {
			t.Errorf("status = %d, want %d", response.StatusCode, status)
		}
		_ = response.Body.Close()
	}
}

func TestCircuitBreakerPolicyReceivesRequestAndResponseSnapshots(t *testing.T) {
	var transportedHeader string
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 2,
			ShouldTrip: func(request *http.Request, response *http.Response, _ error) bool {
				request.Header.Set("X-Policy", "mutated")
				response.Header.Set("X-Policy", "mutated")
				return false
			},
		}),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			transportedHeader = request.Header.Get("X-Policy")
			response := testResponse(http.StatusOK, "ok")
			response.Header.Set("X-Policy", "original")
			return response, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	request := retryTestRequest(t, http.MethodGet, nil)
	request.Header.Set("X-Policy", "caller")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := request.Header.Get("X-Policy"); got != "caller" {
		t.Errorf("caller request header = %q, want caller", got)
	}
	if transportedHeader != "caller" {
		t.Errorf("transport request header = %q, want caller", transportedHeader)
	}
	if got := response.Header.Get("X-Policy"); got != "original" {
		t.Errorf("caller response header = %q, want original", got)
	}
}

func TestCircuitBreakerRecoveryAllowsHalfOpenProbe(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryTimeout:  20 * time.Millisecond,
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return testResponse(http.StatusServiceUnavailable, "failure"), nil
			}
			return testResponse(http.StatusOK, "recovered"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("Do() error = %v, want open breaker", err)
	}

	time.Sleep(30 * time.Millisecond)
	response, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("half-open probe error = %v", err)
	}
	_ = response.Body.Close()
	response, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("request after successful probe error = %v", err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 3 {
		t.Fatalf("transport calls = %d, want 3", got)
	}
}

func TestCircuitBreakerFailedHalfOpenProbeReopens(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryTimeout:  20 * time.Millisecond,
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return testResponse(http.StatusServiceUnavailable, "failure"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	time.Sleep(30 * time.Millisecond)
	response, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("Do() error = %v, want reopened breaker", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestCircuitBreakerLimitsConcurrentHalfOpenRequests(t *testing.T) {
	var calls atomic.Int64
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold:    1,
			RecoveryTimeout:     20 * time.Millisecond,
			HalfOpenMaxRequests: 1,
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			switch calls.Add(1) {
			case 1:
				return testResponse(http.StatusServiceUnavailable, "failure"), nil
			case 2:
				close(probeStarted)
				<-releaseProbe
				return testResponse(http.StatusOK, "probe success"), nil
			default:
				return testResponse(http.StatusOK, "ok"), nil
			}
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	time.Sleep(30 * time.Millisecond)

	probeResult := make(chan error, 1)
	go func() {
		response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
		if response != nil {
			_ = response.Body.Close()
		}
		probeResult <- err
	}()
	<-probeStarted
	_, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("concurrent half-open Do() error = %v, want ErrCircuitBreakerOpen", err)
	}
	close(releaseProbe)
	if err := <-probeResult; err != nil {
		t.Fatalf("half-open probe error = %v", err)
	}
}

func TestCircuitBreakerCountsFinalRetryOutcomeOnce(t *testing.T) {
	var calls atomic.Int64
	var classifications atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 1,
			ShouldTrip: func(_ *http.Request, response *http.Response, _ error) bool {
				classifications.Add(1)
				return response != nil && response.StatusCode >= http.StatusInternalServerError
			},
		}),
		WithRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return testResponse(http.StatusServiceUnavailable, "first failure"), nil
			}
			return testResponse(http.StatusOK, "success"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := classifications.Load(); got != 1 {
		t.Fatalf("failure classifications = %d, want one per logical operation", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport attempts = %d, want 2", got)
	}

	response, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("breaker tripped on intermediate retry failure: %v", err)
	}
	_ = response.Body.Close()
}

func TestCircuitBreakerDoesNotCountPolicyRejection(t *testing.T) {
	var middlewareCalls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return testResponse(http.StatusOK, "ok"), nil
		})),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				if middlewareCalls.Add(1) == 1 {
					return nil, ErrCircuitBreakerOpen
				}
				return next.Do(request)
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("policy rejection error = %v, want ErrCircuitBreakerOpen", err)
	}
	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("request after policy rejection error = %v, want success", err)
	}
	_ = response.Body.Close()
}

func TestCircuitBreakerIgnoresDownstreamPolicyRejection(t *testing.T) {
	var calls atomic.Int64
	var classifications atomic.Int64
	breaker := newCircuitBreaker(*normalizeCircuitBreakerConfig(&CircuitBreakerConfig{
		FailureThreshold: 1,
		ShouldTrip: func(*http.Request, *http.Response, error) bool {
			classifications.Add(1)
			return true
		},
	}))
	doer := &circuitBreakerDoer{
		breaker: breaker,
		next: doerFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return nil, newPolicyRejection(errors.New("rate limit rejected"))
			}
			return testResponse(http.StatusOK, "ok"), nil
		}),
	}

	request := retryTestRequest(t, http.MethodGet, nil)
	if _, err := doer.Do(request); err == nil {
		t.Fatal("first operation error = nil, want policy rejection")
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatalf("operation after policy rejection error = %v", err)
	}
	_ = response.Body.Close()
	if got := classifications.Load(); got != 1 {
		t.Fatalf("classified operations = %d, want 1; policy rejection must not count", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("next calls = %d, want 2", got)
	}
}

func TestCircuitBreakerConcurrentUse(t *testing.T) {
	const requests = 64
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold:    requests,
			RecoveryTimeout:     time.Millisecond,
			HalfOpenMaxRequests: 4,
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return testResponse(http.StatusOK, "ok"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errs := make(chan error, requests)
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
			if err != nil {
				errs <- err
				return
			}
			if closeErr := response.Body.Close(); closeErr != nil {
				errs <- closeErr
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := calls.Load(); got != requests {
		t.Errorf("transport calls = %d, want %d", got, requests)
	}
}

func TestNewRejectsInvalidCircuitBreakerConfig(t *testing.T) {
	tests := []CircuitBreakerConfig{
		{FailureThreshold: -1},
		{FailureThreshold: 1, RecoveryTimeout: -time.Second},
		{FailureThreshold: 1, HalfOpenMaxRequests: -1},
	}
	names := []string{"failure threshold", "recovery timeout", "half-open limit"}
	for i, config := range tests {
		t.Run(names[i], func(t *testing.T) {
			if _, err := New(WithCircuitBreaker(config)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestCircuitBreakerDoesNotTripOnContextCancellation(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return nil, context.Canceled
			}
			return testResponse(http.StatusOK, "ok"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() error = %v, want context.Canceled", err)
	}
	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("request after cancellation = %v, want success", err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestCircuitBreakerCanceledHalfOpenProbeRemainsHalfOpen(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryTimeout:  20 * time.Millisecond,
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			switch calls.Add(1) {
			case 1:
				return testResponse(http.StatusServiceUnavailable, "failure"), nil
			case 2:
				return nil, context.Canceled
			default:
				return testResponse(http.StatusOK, "recovered"), nil
			}
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	time.Sleep(30 * time.Millisecond)
	if _, err := client.Do(retryTestRequest(t, http.MethodGet, nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error = %v, want context.Canceled", err)
	}
	response, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("probe after canceled half-open request = %v, want success", err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 3 {
		t.Fatalf("transport calls = %d, want 3", got)
	}
}
