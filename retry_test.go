package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetriesAreDisabledByDefault(t *testing.T) {
	var calls atomic.Int64
	client, err := New(WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testResponse(http.StatusServiceUnavailable, "unavailable"), nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	request := retryTestRequest(t, http.MethodGet, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1 with retries disabled", got)
	}
}

func TestRetryStatusClosesIntermediateResponseAndPreservesFinal(t *testing.T) {
	var calls atomic.Int64
	intermediate := &trackingBody{reader: strings.NewReader("intermediate")}
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       intermediate,
				}, nil
			}
			return testResponse(http.StatusOK, "final"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
	if !intermediate.closed {
		t.Fatal("intermediate response body was not closed before retry")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "final" {
		t.Fatalf("final response body = %q, want final", body)
	}
	_ = response.Body.Close()
}

func TestRetryMaxAttemptsIncludesInitialExecution(t *testing.T) {
	const maxAttempts = 3
	var calls atomic.Int64
	var closed atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: maxAttempts, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			body := &countingBody{closed: &closed}
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatalf("Do() error = %v, want final HTTP response without transport error", err)
	}
	if got := calls.Load(); got != maxAttempts {
		t.Fatalf("transport calls = %d, want %d", got, maxAttempts)
	}
	if got := closed.Load(); got != maxAttempts-1 {
		t.Fatalf("intermediate bodies closed = %d, want %d", got, maxAttempts-1)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("final status = %d, want 503", response.StatusCode)
	}
	_ = response.Body.Close()
	if got := closed.Load(); got != maxAttempts {
		t.Fatalf("all bodies closed = %d, want %d", got, maxAttempts)
	}
}

func TestRetryDoesNotRetryPermanentStatusOrUnsafeMethod(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
	}{
		{name: "permanent status", method: http.MethodGet, status: http.StatusBadRequest},
		{name: "non-idempotent method", method: http.MethodPost, status: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			client, err := New(
				WithRetry(RetryConfig{MaxAttempts: 4, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
				WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return testResponse(test.status, "not retried"), nil
				})),
			)
			if err != nil {
				t.Fatal(err)
			}

			response, err := client.Do(retryTestRequest(t, test.method, nil))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if got := calls.Load(); got != 1 {
				t.Fatalf("transport calls = %d, want 1", got)
			}
		})
	}
}

func TestRetryPolicyCanSelectStatuses(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
			ShouldRetry: func(_ *http.Request, response *http.Response, _ error) bool {
				return response != nil && response.StatusCode == http.StatusTeapot
			},
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return testResponse(http.StatusTeapot, "retry"), nil
			}
			return testResponse(http.StatusOK, "done"), nil
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", response.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestRetryRecreatesIdempotentRequestBody(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			_ = request.Body.Close()
			if string(body) != "payload" {
				t.Errorf("attempt body = %q, want payload", body)
			}
			if calls.Add(1) == 1 {
				return testResponse(http.StatusServiceUnavailable, "retry"), nil
			}
			return testResponse(http.StatusOK, "done"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	request := retryTestRequest(t, http.MethodPut, strings.NewReader("payload"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestRetryDoesNotReplayUnrecreatableBody(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return testResponse(http.StatusServiceUnavailable, "unavailable"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	request := retryTestRequest(t, http.MethodPut, strings.NewReader("payload"))
	request.GetBody = nil
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1 for unrecreatable body", got)
	}
}

func TestRetryExhaustionWrapsUnderlyingTransportError(t *testing.T) {
	wantErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("unavailable")}
	var calls atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, wantErr
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(retryTestRequest(t, http.MethodGet, nil))
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("Do() error = %v, want ErrRetryExhausted", err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Do() error = %v, want underlying network error", err)
	}
	var exhausted *RetryExhaustedError
	if !errors.As(err, &exhausted) || exhausted.Attempts != 2 {
		t.Fatalf("retry exhaustion details = %+v, want 2 attempts", exhausted)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2", got)
	}
}

func TestRetryBackoffIsCanceledAndClosesIntermediateResponse(t *testing.T) {
	var calls atomic.Int64
	body := &trackingBody{reader: strings.NewReader("retry")}
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 3, InitialBackoff: time.Second, MaxBackoff: time.Second}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	request := retryTestRequest(t, http.MethodGet, nil).WithContext(ctx)
	_, err = client.Do(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want context.DeadlineExceeded", err)
	}
	if !body.closed {
		t.Fatal("intermediate response body was not closed when backoff was canceled")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want no retry after cancellation", got)
	}
}

func TestRetryAfterDelayRespectsContextDeadline(t *testing.T) {
	var calls atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			response := testResponse(http.StatusServiceUnavailable, "retry")
			response.Header.Set("Retry-After", "1")
			return response, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	request := retryTestRequest(t, http.MethodGet, nil).WithContext(ctx)
	_, err = client.Do(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want no retry before Retry-After expires", got)
	}
}

func TestRetryRunsMiddlewareOnceAndAttemptHooksPerExchange(t *testing.T) {
	var middlewareCalls atomic.Int64
	var beforeOperations atomic.Int64
	var afterOperations atomic.Int64
	var beforeAttempts atomic.Int64
	var afterAttempts atomic.Int64
	var transportCalls atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if transportCalls.Add(1) == 1 {
				return testResponse(http.StatusServiceUnavailable, "retry"), nil
			}
			return testResponse(http.StatusOK, "done"), nil
		})),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				middlewareCalls.Add(1)
				return next.Do(request)
			})
		}),
		WithHooks(Hooks{
			BeforeOperation: func(OperationEvent) { beforeOperations.Add(1) },
			AfterOperation:  func(OperationResult) { afterOperations.Add(1) },
			BeforeAttempt:   func(AttemptEvent) { beforeAttempts.Add(1) },
			AfterAttempt:    func(AttemptResult) { afterAttempts.Add(1) },
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(retryTestRequest(t, http.MethodGet, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := middlewareCalls.Load(); got != 1 {
		t.Errorf("middleware calls = %d, want 1", got)
	}
	if got := beforeOperations.Load(); got != 1 {
		t.Errorf("before-operation hooks = %d, want 1", got)
	}
	if got := afterOperations.Load(); got != 1 {
		t.Errorf("after-operation hooks = %d, want 1", got)
	}
	if got := beforeAttempts.Load(); got != 2 {
		t.Errorf("before-attempt hooks = %d, want 2", got)
	}
	if got := afterAttempts.Load(); got != 2 {
		t.Errorf("after-attempt hooks = %d, want 2", got)
	}
}

func TestRetryAttemptsIncludeRedirectExecutionAndHooksCountExchanges(t *testing.T) {
	var unstableCalls atomic.Int64
	var transportCalls atomic.Int64
	var hookAttempts atomic.Int64
	client, err := New(
		WithRetry(RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			if request.URL.Path == "/start" {
				response := testResponse(http.StatusFound, "")
				response.Header.Set("Location", "/unstable")
				response.Request = request
				return response, nil
			}
			if unstableCalls.Add(1) == 1 {
				return testResponse(http.StatusServiceUnavailable, "retry"), nil
			}
			return testResponse(http.StatusOK, "done"), nil
		})),
		WithHooks(Hooks{
			BeforeAttempt: func(event AttemptEvent) {
				if event.Attempt != int(hookAttempts.Add(1)) {
					t.Errorf("attempt hook number = %d, want sequential count", event.Attempt)
				}
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request := retryTestRequest(t, http.MethodGet, nil)
	request.URL.Path = "/start"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", response.StatusCode)
	}
	if got := transportCalls.Load(); got != 4 {
		t.Fatalf("transport exchanges = %d, want 4 across two redirect executions", got)
	}
	if got := hookAttempts.Load(); got != 4 {
		t.Fatalf("attempt hooks = %d, want 4 transport exchanges", got)
	}
}

func TestRetryAfterAndBackoffCalculations(t *testing.T) {
	config := RetryConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     250 * time.Millisecond,
		Jitter:         0,
	}
	for retryNumber, want := range []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		250 * time.Millisecond,
		250 * time.Millisecond,
	} {
		if got := backoffDelay(config, retryNumber+1); got != want {
			t.Errorf("backoffDelay(retry %d) = %s, want %s", retryNumber+1, got, want)
		}
	}

	now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	if got, ok := parseRetryAfter("3", now); !ok || got != 3*time.Second {
		t.Errorf("delta Retry-After = %s, %v; want 3s, true", got, ok)
	}
	date := now.Add(5 * time.Second).Format(http.TimeFormat)
	if got, ok := parseRetryAfter(date, now); !ok || got != 5*time.Second {
		t.Errorf("date Retry-After = %s, %v; want 5s, true", got, ok)
	}
	if got, ok := parseRetryAfter("invalid", now); ok || got != 0 {
		t.Errorf("invalid Retry-After = %s, %v; want 0, false", got, ok)
	}
	response := &http.Response{Header: http.Header{"Retry-After": []string{"3"}}}
	if got, want := retryDelay(config, 1, response, now), 3*time.Second; got != want {
		t.Errorf("retryDelay with Retry-After = %s, want %s", got, want)
	}
}

func TestRetryJitterBounds(t *testing.T) {
	const base = time.Second
	for _, sample := range []float64{0, 0.25, 0.5, 0.75, 1} {
		got := jitteredDelay(base, 0.2, sample)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Errorf("jittered delay for sample %v = %s, outside [800ms, 1.2s]", sample, got)
		}
	}
	if got := jitteredDelay(base, 0.2, 0.5); got != base {
		t.Errorf("midpoint jitter = %s, want %s", got, base)
	}
	if got := jitteredDelay(time.Duration(1<<63-1), 1, 1); got != time.Duration(1<<63-1) {
		t.Errorf("jittered maximum duration = %s, want MaxInt64", got)
	}
}

func TestNewRejectsInvalidRetryConfig(t *testing.T) {
	tests := []RetryConfig{
		{MaxAttempts: 1},
		{MaxAttempts: 2, InitialBackoff: -time.Second},
		{MaxAttempts: 2, InitialBackoff: time.Second, MaxBackoff: time.Millisecond},
		{MaxAttempts: 2, Jitter: -0.1},
		{MaxAttempts: 2, Jitter: 1.1},
	}
	for i, config := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if _, err := New(WithRetry(config)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func retryTestRequest(t *testing.T, method string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, "https://example.com/resource", body)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

type countingBody struct {
	closed *atomic.Int64
}

func (*countingBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (body *countingBody) Close() error {
	body.closed.Add(1)
	return nil
}
