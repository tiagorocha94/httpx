package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoggingRecordsSafeOperationAndAttemptFields(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	body := &loggingTestBody{reader: strings.NewReader("response body secret")}
	requestBody := &loggingTestBody{reader: strings.NewReader("request body secret")}
	client, err := New(
		WithLogging(LoggingConfig{
			Logger:     logger,
			Dependency: "payments",
			Level:      slog.LevelWarn,
		}),
		WithRoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.Body.Close(); err != nil {
				t.Errorf("close request body: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		"https://user:password@example.com/private/path?token=query-secret#fragment-secret",
		requestBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	records := decodeLoggingRecords(t, output.Bytes())
	if len(records) != 2 {
		t.Fatalf("log records = %d, want attempt and operation", len(records))
	}
	attempt := records[0]
	assertLogField(t, attempt, "level", "WARN")
	if attempt["msg"] != "http attempt completed" {
		t.Fatalf("first record message = %v, want attempt", attempt["msg"])
	}
	assertLogField(t, attempt, "dependency", "payments")
	assertLogField(t, attempt, "method", http.MethodPost)
	assertLogField(t, attempt, "url_origin", "https://example.com")
	assertLogField(t, attempt, "attempt", float64(1))
	assertLogField(t, attempt, "status_code", float64(http.StatusOK))
	assertLogField(t, attempt, "error_class", "none")

	operation := records[1]
	if operation["msg"] != "http operation completed" {
		t.Fatalf("second record message = %v, want operation", operation["msg"])
	}
	assertLogField(t, operation, "dependency", "payments")
	assertLogField(t, operation, "method", http.MethodPost)
	assertLogField(t, operation, "url_origin", "https://example.com")
	assertLogField(t, operation, "attempt_count", float64(1))
	assertLogField(t, operation, "status_code", float64(http.StatusOK))

	for _, secret := range []string{
		"user:password",
		"/private/path",
		"query-secret",
		"fragment-secret",
		"response body secret",
	} {
		if bytes.Contains(output.Bytes(), []byte(secret)) {
			t.Errorf("log output contains sensitive value %q", secret)
		}
	}
	if got := body.reads.Load(); got != 0 {
		t.Fatalf("response body reads during logging = %d, want 0", got)
	}
	if got := requestBody.reads.Load(); got != 0 {
		t.Fatalf("request body reads during logging = %d, want 0", got)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoggingRecordsRetriesAndTransportFailures(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	var calls atomic.Int64
	client, err := New(
		WithLogging(LoggingConfig{Logger: logger, Dependency: "catalog"}),
		WithRetry(RetryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return testResponse(http.StatusServiceUnavailable, "temporary"), nil
			}
			return nil, errors.New("transport secret detail")
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.com/items?secret=value", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("Do() error = %v, want ErrRetryExhausted", err)
	}

	records := decodeLoggingRecords(t, output.Bytes())
	if len(records) != 3 {
		t.Fatalf("log records = %d, want two attempts and one operation", len(records))
	}
	assertLogField(t, records[0], "attempt", float64(1))
	assertLogField(t, records[0], "status_code", float64(http.StatusServiceUnavailable))
	assertLogField(t, records[0], "error_class", "none")
	assertLogField(t, records[1], "attempt", float64(2))
	assertLogField(t, records[1], "error_class", "*errors.errorString")
	assertLogField(t, records[2], "attempt_count", float64(2))
	assertLogField(t, records[2], "error_class", "retry_exhausted")
	if bytes.Contains(output.Bytes(), []byte("transport secret detail")) ||
		bytes.Contains(output.Bytes(), []byte("secret=value")) {
		t.Fatal("log output contains raw transport error or sensitive URL query")
	}
}

func TestLoggingClassifiesRateLimitPolicyRejections(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	client, err := New(
		WithLogging(LoggingConfig{Logger: logger, Dependency: "inventory"}),
		WithRateLimit(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}),
		WithRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return testResponse(http.StatusOK, "ok"), nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, doErr := client.Do(request)
		if i == 0 {
			if doErr != nil {
				t.Fatal(doErr)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(doErr, ErrRateLimitExceeded) {
			t.Fatalf("rate-limited Do() error = %v, want ErrRateLimitExceeded", doErr)
		}
	}
	records := decodeLoggingRecords(t, output.Bytes())
	if len(records) != 3 {
		t.Fatalf("log records = %d, want first attempt, first operation, second operation", len(records))
	}
	assertLogField(t, records[2], "error_class", "rate_limit_rejection")
	assertLogField(t, records[2], "attempt_count", float64(0))
}

func TestNewRejectsInvalidLoggingConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []LoggingConfig{
		{Dependency: "service"},
		{Logger: logger},
		{Logger: logger, Dependency: " \t "},
	}
	for _, config := range tests {
		if _, err := New(WithLogging(config)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("New(%+v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
}

type loggingTestBody struct {
	reader io.Reader
	reads  atomic.Int64
}

func (body *loggingTestBody) Read(buffer []byte) (int, error) {
	body.reads.Add(1)
	return body.reader.Read(buffer)
}

func (*loggingTestBody) Close() error {
	return nil
}

func decodeLoggingRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func assertLogField(t *testing.T, record map[string]any, key string, want any) {
	t.Helper()
	if got := record[key]; got != want {
		t.Errorf("log field %q = %v, want %v", key, got, want)
	}
}
