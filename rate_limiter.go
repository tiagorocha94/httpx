package httpx

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// RateLimitConfig configures a client-scoped token-bucket rate limit.
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained token refill rate. It must be positive
	// and finite.
	RequestsPerSecond float64
	// Burst is the maximum number of immediately admitted transport attempts.
	// The bucket starts full and Burst must be positive.
	Burst int
}

type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newRateLimiter(config *RateLimitConfig) *rateLimiter {
	if config == nil {
		return nil
	}
	now := time.Now()
	return &rateLimiter{
		rate:   config.RequestsPerSecond,
		burst:  float64(config.Burst),
		tokens: float64(config.Burst),
		last:   now,
		now:    time.Now,
	}
}

func (limiter *rateLimiter) allow(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	now := limiter.now()
	if elapsed := now.Sub(limiter.last); elapsed > 0 {
		limiter.tokens += elapsed.Seconds() * limiter.rate
		if limiter.tokens > limiter.burst {
			limiter.tokens = limiter.burst
		}
		limiter.last = now
	}
	if limiter.tokens < 1 {
		return ErrRateLimitExceeded
	}
	limiter.tokens--
	return nil
}

func (config RateLimitConfig) validate() error {
	if config.RequestsPerSecond <= 0 || math.IsNaN(config.RequestsPerSecond) ||
		math.IsInf(config.RequestsPerSecond, 0) {
		return errors.New("requests per second must be positive and finite")
	}
	if config.Burst < 1 {
		return errors.New("burst must be positive")
	}
	return nil
}
