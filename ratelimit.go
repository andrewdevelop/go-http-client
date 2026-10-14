package httpclient

import (
	"context"

	"golang.org/x/time/rate"
)

// Limiter throttles outgoing requests. Client calls Acquire once per
// physical attempt (so retries each pay their own cost) before the
// circuit breaker and the actual HTTP round trip.
//
// The built-in implementation wraps golang.org/x/time/rate; plug in your
// own via RateLimitConfig.Store for e.g. a distributed limiter shared
// across multiple Client instances/processes (Redis-backed token
// bucket, etc.).
type Limiter interface {
	// Acquire blocks until a token is available, or returns an error —
	// ctx cancellation, or any sentinel of the implementation's choosing
	// (the built-in Limiter returns ErrRateLimited in non-blocking mode).
	Acquire(ctx context.Context) error
}

// rateLimiter wraps golang.org/x/time/rate behind an Enabled flag so
// callers can configure "no limiting" without special-casing a nil
// limiter. Safe for concurrent use. Implements Limiter.
type rateLimiter struct {
	limiter *rate.Limiter
	enabled bool
	wait    bool
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	rl := &rateLimiter{enabled: cfg.Enabled, wait: cfg.Wait}
	if cfg.Enabled {
		rl.limiter = rate.NewLimiter(rate.Limit(cfg.RPS), cfg.Burst)
	}
	return rl
}

// Acquire blocks until a token is available (if the limiter is
// configured to wait) or returns ErrRateLimited immediately when no
// token is available.
func (rl *rateLimiter) Acquire(ctx context.Context) error {
	if !rl.enabled {
		return nil
	}
	if rl.wait {
		return rl.limiter.Wait(ctx)
	}
	if !rl.limiter.Allow() {
		return ErrRateLimited
	}
	return nil
}

var _ Limiter = (*rateLimiter)(nil)
