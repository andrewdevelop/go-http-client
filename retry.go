package httpclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// RetryPolicy decides whether a failed attempt should be retried, and
// how long to wait before the next one. Client calls ShouldRetry with
// the error from the attempt that just failed, then — if it returned
// true — NextDelay with the same error and attempt number.
//
// The built-in implementation retries based on
// RetryConfig.RetryableStatusCodes with exponential backoff; plug in
// your own via RetryConfig.Policy to e.g. honor a Retry-After response
// header (common on LLM/RPC rate limits — available via
// errors.As(err, *StatusError) and StatusError.Response.Headers), or use
// a different backoff curve entirely.
type RetryPolicy interface {
	// ShouldRetry reports whether attempt (1-indexed, the attempt that
	// just produced err) should be retried.
	ShouldRetry(err error, attempt int) bool

	// NextDelay returns how long to wait before attempt+1.
	NextDelay(err error, attempt int) time.Duration
}

// defaultRetryPolicy is the RetryPolicy used whenever RetryConfig.Policy
// is left nil: retry status codes from cfg.RetryableStatusCodes (falling
// back to DefaultRetryableStatusCodes) and any non-StatusError (DNS
// failure, connection refused, i/o timeout, ...) except context
// cancellation/deadline and ErrResponseTooLarge — none of which retrying
// would ever fix — with exponential backoff per calculateBackoff.
// Implements RetryPolicy.
type defaultRetryPolicy struct {
	cfg   RetryConfig
	codes []int
}

func newDefaultRetryPolicy(cfg RetryConfig) *defaultRetryPolicy {
	codes := cfg.RetryableStatusCodes
	if codes == nil {
		codes = DefaultRetryableStatusCodes
	}
	return &defaultRetryPolicy{cfg: cfg, codes: codes}
}

func (p *defaultRetryPolicy) ShouldRetry(err error, attempt int) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrResponseTooLarge) {
		return false
	}

	var se *StatusError
	if errors.As(err, &se) {
		for _, c := range p.codes {
			if c == se.StatusCode {
				return true
			}
		}
		return false
	}

	return true
}

func (p *defaultRetryPolicy) NextDelay(err error, attempt int) time.Duration {
	return calculateBackoff(attempt, p.cfg)
}

var _ RetryPolicy = (*defaultRetryPolicy)(nil)

// withRetry runs fn, retrying on retryable failures with exponential
// backoff up to cfg.MaxAttempts, subject to limiter and cb. Non-retryable
// errors and a cancelled/expired ctx return immediately.
func withRetry(ctx context.Context, cfg RetryConfig, limiter Limiter, cb Breaker, fn func(context.Context) error) error {
	if !cfg.Enabled {
		if err := limiter.Acquire(ctx); err != nil {
			return err
		}
		return cb.Execute(func() error { return fn(ctx) })
	}

	maxAttempts := cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	policy := cfg.Policy
	if policy == nil {
		policy = newDefaultRetryPolicy(cfg)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := limiter.Acquire(ctx); err != nil {
			return err
		}

		err := cb.Execute(func() error {
			return fn(ctx)
		})
		if err == nil {
			return nil
		}
		lastErr = err

		if errors.Is(err, ErrCircuitOpen) || !policy.ShouldRetry(err, attempt) {
			return err
		}
		if attempt >= maxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(policy.NextDelay(err, attempt)):
		}
	}

	return fmt.Errorf("%w: %d attempts exceeded, last error: %w", ErrRetriesExhausted, maxAttempts, lastErr)
}

// minBackoffDelay is the floor calculateBackoff never returns below, even
// when BaseDelay is 0 — otherwise a misconfigured (but retry-enabled)
// RetryConfig turns into a hot loop hammering the server with no pause
// between attempts at all.
const minBackoffDelay = time.Millisecond

func calculateBackoff(attempt int, cfg RetryConfig) time.Duration {
	delay := float64(cfg.BaseDelay) * math.Pow(2, float64(attempt-1))
	if cfg.MaxDelay > 0 && delay > float64(cfg.MaxDelay) {
		delay = float64(cfg.MaxDelay)
	}
	if cfg.Jitter {
		delay += rand.Float64() * 0.5 * delay
	}
	if d := time.Duration(delay); d > minBackoffDelay {
		return d
	}
	return minBackoffDelay
}
