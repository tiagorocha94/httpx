package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

const defaultRequestIDHeader = "X-Request-ID"

// Hooks contains optional observational callbacks around logical operations
// and transport attempts. Callbacks run synchronously, may run concurrently,
// and must be safe for concurrent use. They must not panic.
type Hooks struct {
	BeforeOperation func(OperationEvent)
	AfterOperation  func(OperationResult)
	BeforeAttempt   func(AttemptEvent)
	AfterAttempt    func(AttemptResult)
}

// WithHooks configures observational callbacks for logical operations and
// transport attempts.
func WithHooks(hooks Hooks) Option {
	return func(config *Config) error {
		config.Hooks = hooks
		return nil
	}
}

// OperationEvent identifies a logical operation without exposing mutable
// request or execution state.
type OperationEvent struct {
	RequestID string
	Method    string
	URL       string
}

// OperationResult describes the outcome of a logical operation. Duration
// measures request execution through response headers, not response-body reads.
type OperationResult struct {
	OperationEvent
	StatusCode int
	Duration   time.Duration
	Err        error
}

// AttemptEvent identifies one transport exchange within a logical operation.
type AttemptEvent struct {
	OperationEvent
	Attempt int
}

// AttemptResult describes the outcome of one transport exchange. Duration
// measures the RoundTrip call, not response-body reads.
type AttemptResult struct {
	AttemptEvent
	StatusCode int
	Duration   time.Duration
	Err        error
}

func requestIDFor(request *http.Request, header string) (string, error) {
	if requestID := request.Header.Get(header); requestID != "" {
		return requestID, nil
	}

	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("httpx: generate request ID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80

	var encoded [36]byte
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded[:]), nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		char := name[i]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '!' || char == '#' ||
			char == '$' || char == '%' || char == '&' || char == '\'' ||
			char == '*' || char == '+' || char == '-' || char == '.' ||
			char == '^' || char == '_' || char == '`' || char == '|' ||
			char == '~' {
			continue
		}
		return false
	}
	return true
}

func operationEvent(request *http.Request, requestID string) OperationEvent {
	url := ""
	if request.URL != nil {
		url = request.URL.String()
	}
	return OperationEvent{
		RequestID: requestID,
		Method:    request.Method,
		URL:       url,
	}
}

func (operation *operationState) finish(response *http.Response, err error) {
	if operation.hooks.AfterOperation == nil && operation.logger == nil {
		return
	}
	result := OperationResult{
		OperationEvent: operation.event,
		Duration:       time.Since(operation.startedAt),
		Err:            err,
	}
	if response != nil {
		result.StatusCode = response.StatusCode
	}
	if operation.hooks.AfterOperation != nil {
		operation.hooks.AfterOperation(result)
	}
	logOperation(operation.logger, result, int(operation.attempts.Load()))
}
