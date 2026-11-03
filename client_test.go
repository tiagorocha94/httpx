package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestNewUsesStandardDefaults(t *testing.T) {
	client, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if client.timeout != 0 {
		t.Fatalf("default timeout = %s, want 0", client.timeout)
	}
	transport, ok := client.roundTripper().(*http.Transport)
	if !ok {
		t.Fatalf("default transport type = %T, want *http.Transport", client.roundTripper())
	}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && transport == defaultTransport {
		t.Fatal("default transport was not cloned")
	}
}

func TestNewUsesCustomRoundTripper(t *testing.T) {
	wantResponse := &http.Response{
		StatusCode: http.StatusTeapot,
		Body:       io.NopCloser(strings.NewReader("tea")),
		Header:     make(http.Header),
	}
	called := false
	client, err := New(WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.Method != http.MethodGet {
			t.Errorf("method = %q, want %q", request.Method, http.MethodGet)
		}
		return wantResponse, nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !called {
		t.Fatal("RoundTripper was not called")
	}
	if response != wantResponse {
		t.Fatal("Do() did not return the RoundTripper response")
	}
	if response.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusTeapot)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "tea" {
		t.Fatalf("body = %q, want %q", body, "tea")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewClonesConfiguredTransport(t *testing.T) {
	transport := &http.Transport{MaxIdleConns: 12}
	client, err := New(WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}

	got, ok := client.roundTripper().(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.roundTripper())
	}
	if got == transport {
		t.Fatal("client retained caller-owned transport")
	}
	if got.MaxIdleConns != 12 {
		t.Fatalf("MaxIdleConns = %d, want 12", got.MaxIdleConns)
	}
	transport.MaxIdleConns = 99
	if got.MaxIdleConns != 12 {
		t.Fatalf("client transport changed after input mutation: %d", got.MaxIdleConns)
	}
}

func TestNewUsesUnifiedConfigAndLaterOptionsOverride(t *testing.T) {
	config := Config{Timeout: time.Second}
	client, err := New(WithConfig(config), WithTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if client.timeout != 2*time.Second {
		t.Fatalf("timeout = %s, want 2s", client.timeout)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		options []Option
	}{
		{
			name:    "negative timeout",
			options: []Option{WithTimeout(-time.Second)},
		},
		{
			name: "both transports",
			options: []Option{
				WithTransport(&http.Transport{}),
				WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, nil
				})),
			},
		},
		{
			name: "nil option",
			options: []Option{
				nil,
			},
		},
		{
			name:    "nil round tripper",
			options: []Option{WithRoundTripper(nil)},
		},
		{
			name:    "typed nil round tripper",
			options: []Option{WithRoundTripper(roundTripperFunc(nil))},
		},
		{
			name:    "nil transport",
			options: []Option{WithTransport(nil)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.options...)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestDoRespectsRequestCancellation(t *testing.T) {
	called := false
	client, err := New(WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		return nil, request.Context().Err()
	})))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() error = %v, want context.Canceled", err)
	}
	if !called {
		t.Fatal("RoundTripper was not called")
	}
}

func TestDoAppliesClientTimeout(t *testing.T) {
	client, err := New(
		WithTimeout(20*time.Millisecond),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})),
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

func TestCloseIdleConnectionsDelegatesToTransport(t *testing.T) {
	transport := &closeTrackingTransport{}
	client, err := New(WithRoundTripper(transport))
	if err != nil {
		t.Fatal(err)
	}

	client.CloseIdleConnections()
	if !transport.closed {
		t.Fatal("CloseIdleConnections was not called on transport")
	}
}

type closeTrackingTransport struct {
	closed bool
}

func (*closeTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected transport call")
}

func (transport *closeTrackingTransport) CloseIdleConnections() {
	transport.closed = true
}

func (client *Client) roundTripper() http.RoundTripper {
	transport, ok := client.client.Transport.(*operationRoundTripper)
	if !ok {
		return client.client.Transport
	}
	return transport.next
}
