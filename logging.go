package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// LoggingConfig configures optional structured logs for operations and
// transport attempts.
type LoggingConfig struct {
	// Logger receives the structured log records. It must be safe for
	// concurrent use, as required by log/slog.
	Logger *slog.Logger
	// Dependency identifies the remote dependency and must not contain secrets.
	Dependency string
	// Level is the level used for operation and attempt records. Zero selects
	// slog.LevelInfo.
	Level slog.Level
}

func (config LoggingConfig) validate() error {
	if config.Logger == nil {
		return errors.New("logger must not be nil")
	}
	if strings.TrimSpace(config.Dependency) == "" {
		return errors.New("dependency must not be empty")
	}
	return nil
}

func logOperation(config *LoggingConfig, result OperationResult, attempts int) {
	if config == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("dependency", config.Dependency),
		slog.String("request_id", result.RequestID),
		slog.String("method", result.Method),
		slog.String("url_origin", safeURLOrigin(result.URL)),
		slog.Int("status_code", result.StatusCode),
		slog.Duration("duration", result.Duration),
		slog.Int("attempt_count", attempts),
		slog.String("error_class", classifyLogError(result.Err)),
	}
	config.Logger.LogAttrs(context.Background(), config.Level, "http operation completed", attrs...)
}

func logAttempt(config *LoggingConfig, event AttemptEvent, responseStatus int, err error, duration time.Duration) {
	if config == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("dependency", config.Dependency),
		slog.String("request_id", event.RequestID),
		slog.String("method", event.Method),
		slog.String("url_origin", safeURLOrigin(event.URL)),
		slog.Int("attempt", event.Attempt),
		slog.Int("status_code", responseStatus),
		slog.Duration("duration", duration),
		slog.String("error_class", classifyLogError(err)),
	}
	config.Logger.LogAttrs(context.Background(), config.Level, "http attempt completed", attrs...)
}

func safeURLOrigin(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func classifyLogError(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, ErrCircuitBreakerOpen):
		return "circuit_breaker_rejection"
	case errors.Is(err, ErrRateLimitExceeded):
		return "rate_limit_rejection"
	case errors.Is(err, ErrRetryExhausted):
		return "retry_exhausted"
	case isPolicyRejection(err):
		return "policy_rejection"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return "network_error"
	}
	return fmt.Sprintf("%T", err)
}
