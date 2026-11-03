package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (fn doerFunc) Do(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestMiddlewareCompositionOrder(t *testing.T) {
	var calls []string
	wrap := func(name string) Middleware {
		return func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				calls = append(calls, name+" before")
				response, err := next.Do(request)
				calls = append(calls, name+" after")
				return response, err
			})
		}
	}

	client, err := New(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls = append(calls, "transport")
			return testResponse(http.StatusOK, "ok"), nil
		})),
		WithMiddleware(wrap("first"), wrap("second")),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"first before",
		"second before",
		"transport",
		"second after",
		"first after",
	}
	if !equalStrings(calls, want) {
		t.Fatalf("middleware order = %v, want %v", calls, want)
	}
}

func TestMiddlewareReceivesCloneAndCanModifyIt(t *testing.T) {
	var transported *http.Request
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			transported = request
			return testResponse(http.StatusOK, "response"), nil
		})),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				request.Method = http.MethodHead
				request.URL.Path = "/middleware"
				request.Header.Set("X-Middleware", "applied")
				return next.Do(request)
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com/original", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Original", "preserved")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if transported == request {
		t.Fatal("middleware chain received the caller-owned request")
	}
	if transported.Method != http.MethodHead || transported.URL.Path != "/middleware" {
		t.Fatalf("transport request = %s %s, want HEAD /middleware", transported.Method, transported.URL.Path)
	}
	if transported.Header.Get("X-Middleware") != "applied" {
		t.Fatal("middleware header did not reach transport")
	}
	if request.Method != http.MethodGet || request.URL.Path != "/original" {
		t.Fatalf("caller request was mutated: %s %s", request.Method, request.URL.Path)
	}
	if request.Header.Get("X-Middleware") != "" || request.Header.Get("X-Original") != "preserved" {
		t.Fatalf("caller request headers were mutated: %v", request.Header)
	}
}

func TestMiddlewareRunsOnceForRedirectingLogicalOperation(t *testing.T) {
	var middlewareCalls int
	var completedAttempts uint64
	var operation *operationState
	var transportCalls int

	client, err := New(
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			transportCalls++
			if transportCalls == 1 {
				response := testResponse(http.StatusFound, "")
				response.Header.Set("Location", "/final")
				response.Request = request
				return response, nil
			}
			return testResponse(http.StatusOK, "done"), nil
		})),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				middlewareCalls++
				operation = operationFromContext(request.Context())
				if operation == nil {
					t.Fatal("logical operation missing from request context")
				}
				response, err := next.Do(request)
				completedAttempts = operation.attempts.Load()
				return response, err
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if middlewareCalls != 1 {
		t.Fatalf("middleware calls = %d, want 1 logical operation", middlewareCalls)
	}
	if transportCalls != 2 || completedAttempts != 2 {
		t.Fatalf("transport attempts = %d, tracked attempts = %d, want 2", transportCalls, completedAttempts)
	}
	if operation.startedAt.IsZero() {
		t.Fatal("logical operation start time was not recorded")
	}
}

func TestMiddlewareErrorIsReturned(t *testing.T) {
	wantErr := errors.New("middleware failed")
	client, err := New(WithMiddleware(func(HTTPDoer) HTTPDoer {
		return doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, wantErr
		})
	}))
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Do() error = %v, want middleware error", err)
	}
}

func TestMiddlewareCanShortCircuit(t *testing.T) {
	transportCalled := false
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			transportCalled = true
			return nil, errors.New("unexpected transport call")
		})),
		WithMiddleware(func(HTTPDoer) HTTPDoer {
			return doerFunc(func(*http.Request) (*http.Response, error) {
				return testResponse(http.StatusAccepted, "cached"), nil
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if transportCalled {
		t.Fatal("transport was called after middleware short-circuited")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "cached" {
		t.Fatalf("response body = %q, want cached", body)
	}
}

func TestNewRejectsNilMiddleware(t *testing.T) {
	_, err := New(WithMiddleware(nil))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
	}
}

func TestNewRejectsMiddlewareReturningNilDoer(t *testing.T) {
	_, err := New(WithMiddleware(func(HTTPDoer) HTTPDoer {
		return nil
	}))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
	}
}

func TestDoRejectsNilResponseFromMiddleware(t *testing.T) {
	client, err := New(WithMiddleware(func(HTTPDoer) HTTPDoer {
		return doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, nil
		})
	}))
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Do() error = %v, want ErrInvalidResponse", err)
	}
}

func TestDoReturnsDeadlineWhenMiddlewareTimesOutWithoutResponse(t *testing.T) {
	client, err := New(
		WithTimeout(20*time.Millisecond),
		WithMiddleware(func(HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, nil
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestDoTimeoutCoversMiddleware(t *testing.T) {
	body := &trackingBody{reader: strings.NewReader("late")}
	client, err := New(
		WithTimeout(20*time.Millisecond),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       body,
					Header:     make(http.Header),
				}, nil
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want context.DeadlineExceeded", err)
	}
	if !body.closed {
		t.Fatal("late response body was not closed")
	}
}

func TestMiddlewareAndTransportAreConcurrentAcrossOperations(t *testing.T) {
	var middlewareCalls atomic.Int64
	var transportCalls atomic.Int64
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       http.NoBody,
				Header:     make(http.Header),
			}, nil
		})),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				middlewareCalls.Add(1)
				if operationFromContext(request.Context()) == nil {
					return nil, errors.New("operation context missing")
				}
				return next.Do(request)
			})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	const requests = 32
	var wait sync.WaitGroup
	errorsFound := make(chan error, requests)
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if err != nil {
				errorsFound <- err
				return
			}
			response, err := client.Do(request)
			if err != nil {
				errorsFound <- err
				return
			}
			if response.Body != http.NoBody {
				if err := response.Body.Close(); err != nil {
					errorsFound <- err
				}
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if got := middlewareCalls.Load(); got != requests {
		t.Errorf("middleware calls = %d, want %d", got, requests)
	}
	if got := transportCalls.Load(); got != requests {
		t.Errorf("transport calls = %d, want %d", got, requests)
	}
}

func TestDoTimeoutRemainsActiveWhileReadingBody(t *testing.T) {
	client, err := New(
		WithTimeout(100*time.Millisecond),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       blockingBody{ctx: request.Context()},
				Header:     make(http.Header),
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	_, err = io.ReadAll(response.Body)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadAll() error = %v, want context.DeadlineExceeded", err)
	}
}

func testResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        make(http.Header),
		ContentLength: int64(len(body)),
	}
}

type blockingBody struct {
	ctx context.Context
}

func (body blockingBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (blockingBody) Close() error {
	return nil
}

type trackingBody struct {
	reader io.Reader
	closed bool
}

func (body *trackingBody) Read(buffer []byte) (int, error) {
	return body.reader.Read(buffer)
}

func (body *trackingBody) Close() error {
	body.closed = true
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
