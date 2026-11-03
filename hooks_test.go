package httpx

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDoGeneratesAndPropagatesRequestID(t *testing.T) {
	var transported *http.Request
	client, err := New(WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		transported = request
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
		}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Unrelated", "preserved")
	if _, err := client.Do(request); err != nil {
		t.Fatal(err)
	}

	requestID := transported.Header.Get(defaultRequestIDHeader)
	if len(requestID) != 36 || requestID[8] != '-' || requestID[13] != '-' ||
		requestID[18] != '-' || requestID[23] != '-' || requestID[14] != '4' {
		t.Fatalf("generated request ID = %q, want UUID v4", requestID)
	}
	if transported.Header.Get("X-Unrelated") != "preserved" {
		t.Fatalf("unrelated header = %q, want preserved", transported.Header.Get("X-Unrelated"))
	}
	if request.Header.Get(defaultRequestIDHeader) != "" {
		t.Fatal("Client.Do mutated the caller's request headers")
	}
}

func TestDoPreservesSuppliedRequestIDWithConfiguredHeader(t *testing.T) {
	var transported *http.Request
	client, err := New(
		WithRequestIDHeader("X-Correlation-ID"),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			transported = request
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Correlation-ID", "caller-value")
	request.Header.Set("X-Other", "also-preserved")
	if _, err := client.Do(request); err != nil {
		t.Fatal(err)
	}

	if got := transported.Header.Get("X-Correlation-ID"); got != "caller-value" {
		t.Fatalf("correlation ID = %q, want caller-value", got)
	}
	if got := transported.Header.Get(defaultRequestIDHeader); got != "" {
		t.Fatalf("default request ID header = %q, want empty", got)
	}
	if got := transported.Header.Get("X-Other"); got != "also-preserved" {
		t.Fatalf("unrelated header = %q, want also-preserved", got)
	}
}

func TestRequestIDIsRestoredAfterMiddlewareMutation(t *testing.T) {
	var operationID string
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if got := request.Header.Get(defaultRequestIDHeader); got != operationID {
				t.Errorf("transport request ID = %q, want %q", got, operationID)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})),
		WithMiddleware(func(next HTTPDoer) HTTPDoer {
			return doerFunc(func(request *http.Request) (*http.Response, error) {
				operationID = request.Header.Get(defaultRequestIDHeader)
				if operationID == "" {
					t.Error("request ID was not available to middleware")
				}
				request.Header.Set(defaultRequestIDHeader, "middleware-value")
				return next.Do(request)
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
	if _, err := client.Do(request); err != nil {
		t.Fatal(err)
	}
}

func TestHooksFollowOperationAndAttemptOrderAcrossRedirects(t *testing.T) {
	var calls []string
	var requestIDs []string
	var attemptURLs []string
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			requestIDs = append(requestIDs, request.Header.Get(defaultRequestIDHeader))
			attemptURLs = append(attemptURLs, request.URL.String())
			if len(attemptURLs) == 1 {
				response := &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"/final"}},
					Body:       http.NoBody,
					Request:    request,
				}
				return response, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})),
		WithHooks(Hooks{
			BeforeOperation: func(event OperationEvent) {
				calls = append(calls, "before-operation")
				if event.RequestID == "" || event.Method != http.MethodGet {
					t.Errorf("before operation event = %+v", event)
				}
			},
			BeforeAttempt: func(event AttemptEvent) {
				calls = append(calls, "before-attempt")
				if event.Attempt != len(attemptURLs)+1 {
					t.Errorf("attempt number = %d, want %d", event.Attempt, len(attemptURLs)+1)
				}
			},
			AfterAttempt: func(result AttemptResult) {
				calls = append(calls, "after-attempt")
				if result.Attempt != len(attemptURLs) {
					t.Errorf("completed attempt = %d, want %d", result.Attempt, len(attemptURLs))
				}
			},
			AfterOperation: func(result OperationResult) {
				calls = append(calls, "after-operation")
				if result.StatusCode != http.StatusOK || result.Err != nil {
					t.Errorf("operation result = %+v, want successful 200", result)
				}
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"before-operation",
		"before-attempt", "after-attempt",
		"before-attempt", "after-attempt",
		"after-operation",
	}
	if !equalStrings(calls, wantCalls) {
		t.Fatalf("hook order = %v, want %v", calls, wantCalls)
	}
	if len(requestIDs) != 2 || requestIDs[0] == "" || requestIDs[0] != requestIDs[1] {
		t.Fatalf("request IDs across attempts = %v, want one shared ID", requestIDs)
	}
	if !strings.HasSuffix(attemptURLs[0], "/start") || !strings.HasSuffix(attemptURLs[1], "/final") {
		t.Fatalf("attempt URLs = %v, want redirect and final URLs", attemptURLs)
	}
}

func TestHooksReportFailureAndShortCircuit(t *testing.T) {
	wantErr := errors.New("middleware failed")
	var calls []string
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("transport called after middleware short-circuit")
			return nil, nil
		})),
		WithMiddleware(func(HTTPDoer) HTTPDoer {
			return doerFunc(func(*http.Request) (*http.Response, error) {
				return nil, wantErr
			})
		}),
		WithHooks(Hooks{
			BeforeOperation: func(OperationEvent) {
				calls = append(calls, "before")
			},
			BeforeAttempt: func(AttemptEvent) {
				calls = append(calls, "before-attempt")
			},
			AfterAttempt: func(AttemptResult) {
				calls = append(calls, "after-attempt")
			},
			AfterOperation: func(result OperationResult) {
				calls = append(calls, "after")
				if !errors.Is(result.Err, wantErr) {
					t.Errorf("operation error = %v, want middleware error", result.Err)
				}
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); !errors.Is(err, wantErr) {
		t.Fatalf("Do() error = %v, want middleware error", err)
	}
	if !equalStrings(calls, []string{"before", "after"}) {
		t.Fatalf("hooks on short-circuit = %v, want operation hooks only", calls)
	}
}

func TestHooksReportCanceledAttempt(t *testing.T) {
	var attemptErr error
	var operationErr error
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return nil, request.Context().Err()
		})),
		WithHooks(Hooks{
			AfterAttempt: func(result AttemptResult) {
				attemptErr = result.Err
			},
			AfterOperation: func(result OperationResult) {
				operationErr = result.Err
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() error = %v, want context.Canceled", err)
	}
	if !errors.Is(attemptErr, context.Canceled) {
		t.Fatalf("attempt hook error = %v, want context.Canceled", attemptErr)
	}
	if !errors.Is(operationErr, context.Canceled) {
		t.Fatalf("operation hook error = %v, want context.Canceled", operationErr)
	}
}

func TestHooksAreSafeAcrossConcurrentOperations(t *testing.T) {
	const requests = 32
	var beforeCalls atomic.Int64
	var afterCalls atomic.Int64
	var requestIDs sync.Map
	client, err := New(
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			requestID := request.Header.Get(defaultRequestIDHeader)
			if _, loaded := requestIDs.LoadOrStore(requestID, struct{}{}); loaded {
				t.Errorf("request ID %q was reused", requestID)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})),
		WithHooks(Hooks{
			BeforeOperation: func(OperationEvent) {
				beforeCalls.Add(1)
			},
			AfterOperation: func(OperationResult) {
				afterCalls.Add(1)
			},
		}),
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
			request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if err != nil {
				errs <- err
				return
			}
			_, err = client.Do(request)
			if err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := beforeCalls.Load(); got != requests {
		t.Errorf("before operation calls = %d, want %d", got, requests)
	}
	if got := afterCalls.Load(); got != requests {
		t.Errorf("after operation calls = %d, want %d", got, requests)
	}
}

func TestNewRejectsInvalidRequestIDHeader(t *testing.T) {
	for _, name := range []string{"", "Bad Header", "X-Bad\nHeader"} {
		t.Run(name, func(t *testing.T) {
			_, err := New(WithRequestIDHeader(name))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
