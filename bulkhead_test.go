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

func TestBulkheadLimitsConcurrentLogicalOperations(t *testing.T) {
	const (
		maxConcurrent = 2
		requests      = 6
	)
	bulkhead := newBulkhead(BulkheadConfig{MaxConcurrent: maxConcurrent})
	transportStarted := make(chan struct{}, requests)
	releaseTransport := make(chan struct{})
	releaseBodies := make(chan struct{})
	responses := make(chan *http.Response, requests)
	var current atomic.Int64
	var maximum atomic.Int64
	doer := &bulkheadDoer{
		bulkhead: bulkhead,
		next: doerFunc(func(*http.Request) (*http.Response, error) {
			inFlight := current.Add(1)
			for observed := maximum.Load(); inFlight > observed; observed = maximum.Load() {
				if maximum.CompareAndSwap(observed, inFlight) {
					break
				}
			}
			transportStarted <- struct{}{}
			<-releaseTransport
			current.Add(-1)
			return testResponse(http.StatusOK, "response"), nil
		}),
	}

	var wait sync.WaitGroup
	for i := 0; i < requests; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if err != nil {
				t.Error(err)
				return
			}
			response, err := doer.Do(request)
			if err != nil {
				t.Errorf("Do() error = %v", err)
				return
			}
			responses <- response
			<-releaseBodies
			if err := response.Body.Close(); err != nil {
				t.Errorf("close response body: %v", err)
			}
		}()
	}

	for i := 0; i < maxConcurrent; i++ {
		<-transportStarted
	}
	if got := maximum.Load(); got > maxConcurrent {
		t.Fatalf("maximum concurrent operations = %d, want at most %d", got, maxConcurrent)
	}
	select {
	case <-transportStarted:
		t.Fatal("bulkhead admitted more operations than its capacity")
	default:
	}

	close(releaseTransport)
	for i := 0; i < maxConcurrent; i++ {
		<-responses
	}
	select {
	case <-transportStarted:
		t.Fatal("bulkhead admitted another operation before response bodies were closed")
	default:
	}

	close(releaseBodies)
	wait.Wait()
	if got := maximum.Load(); got > maxConcurrent {
		t.Fatalf("maximum concurrent operations = %d, want at most %d", got, maxConcurrent)
	}
}

func TestBulkheadContextCancellationWhileWaiting(t *testing.T) {
	bulkhead := newBulkhead(BulkheadConfig{MaxConcurrent: 1})
	doer := &bulkheadDoer{
		bulkhead: bulkhead,
		next: doerFunc(func(*http.Request) (*http.Response, error) {
			return testResponse(http.StatusOK, "response"), nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := doer.Do(request)
		result <- err
	}()
	<-started
	select {
	case err := <-result:
		t.Fatalf("waiting Do() returned before a permit was available: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting Do() error = %v, want context.Canceled", err)
	}

	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = doer.Do(request)
	if err != nil {
		t.Fatalf("Do() after releasing permit: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBulkheadReleasesPermitOnAllCompletionPaths(t *testing.T) {
	testErr := errors.New("transport failed")
	tests := []struct {
		name      string
		next      HTTPDoer
		consume   func(*http.Response) error
		wantPanic bool
	}{
		{
			name: "close successful response",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return testResponse(http.StatusOK, "response"), nil
			}),
			consume: func(response *http.Response) error {
				return response.Body.Close()
			},
		},
		{
			name: "read response to EOF",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return testResponse(http.StatusOK, "response"), nil
			}),
			consume: func(response *http.Response) error {
				_, err := io.ReadAll(response.Body)
				return err
			},
		},
		{
			name: "response read error",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				response := testResponse(http.StatusOK, "")
				response.Body = bulkheadReadErrorBody{err: testErr}
				return response, nil
			}),
			consume: func(response *http.Response) error {
				_, err := response.Body.Read(make([]byte, 1))
				if !errors.Is(err, testErr) {
					return errors.New("response body read did not return the configured error")
				}
				return nil
			},
		},
		{
			name: "transport error",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return nil, testErr
			}),
		},
		{
			name: "policy rejection",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return nil, newPolicyRejection(ErrCircuitBreakerOpen)
			}),
		},
		{
			name: "empty response",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return nil, nil
			}),
		},
		{
			name: "response without body",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK}, nil
			}),
		},
		{
			name: "response without body stream",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			}),
		},
		{
			name: "panic",
			next: doerFunc(func(*http.Request) (*http.Response, error) {
				panic("transport panic")
			}),
			wantPanic: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bulkhead := newBulkhead(BulkheadConfig{MaxConcurrent: 1})
			doer := &bulkheadDoer{next: test.next, bulkhead: bulkhead}
			request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if err != nil {
				t.Fatal(err)
			}

			if test.wantPanic {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("Do() did not panic")
						}
					}()
					_, _ = doer.Do(request)
				}()
			} else {
				response, doErr := doer.Do(request)
				if test.name == "transport error" && !errors.Is(doErr, testErr) {
					t.Fatalf("Do() error = %v, want transport error", doErr)
				}
				if test.name == "policy rejection" && !isPolicyRejection(doErr) {
					t.Fatalf("Do() error = %v, want policy rejection", doErr)
				}
				if test.consume != nil {
					if err := test.consume(response); err != nil {
						t.Fatalf("consume response: %v", err)
					}
				}

			}

			if got := len(bulkhead.slots); got != 0 {
				t.Fatalf("held permits = %d, want 0", got)
			}
		})
	}
}

type bulkheadReadErrorBody struct {
	err error
}

func (body bulkheadReadErrorBody) Read([]byte) (int, error) {
	return 0, body.err
}

func (bulkheadReadErrorBody) Close() error {
	return nil
}

func TestBulkheadReleasesPermitWhenResponseBodyReadPanics(t *testing.T) {
	bulkhead := newBulkhead(BulkheadConfig{MaxConcurrent: 1})
	doer := &bulkheadDoer{
		bulkhead: bulkhead,
		next: doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       bulkheadReadPanicBody{},
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("response body Read() did not panic")
			}
		}()
		_, _ = response.Body.Read(make([]byte, 1))
	}()
	if got := len(bulkhead.slots); got != 0 {
		t.Fatalf("held permits after body panic = %d, want 0", got)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

type bulkheadReadPanicBody struct{}

func (bulkheadReadPanicBody) Read([]byte) (int, error) {
	panic("response body panic")
}

func (bulkheadReadPanicBody) Close() error {
	return nil
}

func TestBulkheadConfigAndClientScope(t *testing.T) {
	config := Config{
		Bulkhead: &BulkheadConfig{MaxConcurrent: 1},
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}
	first, err := New(WithConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(WithBulkhead(BulkheadConfig{MaxConcurrent: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if first.doer.(*bulkheadDoer).bulkhead == second.doer.(*bulkheadDoer).bulkhead {
		t.Fatal("clients share bulkhead state")
	}
	if _, err := New(WithBulkhead(BulkheadConfig{})); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() with zero capacity error = %v, want ErrInvalidConfig", err)
	}
}
