package httpclient

import (
	"errors"

	"github.com/sony/gobreaker"
)

// Breaker guards a request behind a circuit breaker. Client calls
// Execute for each physical attempt (inside the retry loop, outside the
// rate limiter) and checks IsAvailable where it needs to report breaker
// state without making a call.
//
// The built-in implementation wraps github.com/sony/gobreaker; plug in
// your own via CircuitBreakerConfig.Store for a different breaker
// library, or one that shares trip state across multiple Client
// instances.
type Breaker interface {
	// Execute runs fn, recording its result against the breaker's
	// failure count. Returns ErrCircuitOpen (or an implementation's own
	// equivalent) without calling fn if the breaker is currently open.
	Execute(fn func() error) error

	// IsAvailable reports whether the breaker would currently let a
	// request through.
	IsAvailable() bool
}

// circuitBreaker wraps sony/gobreaker with an Enabled flag so callers can
// configure "no breaker" without special-casing a nil breaker. Safe for
// concurrent use. Implements Breaker.
type circuitBreaker struct {
	cb      *gobreaker.CircuitBreaker
	enabled bool
}

func newCircuitBreaker(cfg CircuitBreakerConfig) *circuitBreaker {
	settings := gobreaker.Settings{
		Name:        "httpclient",
		MaxRequests: uint32(cfg.HalfOpenMaxCalls),
		Interval:    cfg.Interval,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests == 0 {
				return false
			}
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return int(counts.TotalFailures) >= cfg.Threshold && failureRatio >= 0.5
		},
		IsSuccessful: func(err error) bool {
			return !isBreakerFailure(err)
		},
	}

	return &circuitBreaker{
		cb:      gobreaker.NewCircuitBreaker(settings),
		enabled: cfg.Enabled,
	}
}

// isBreakerFailure reports whether err should count against the breaker.
// A *StatusError only counts for 429/5xx — an application-level 4xx (bad
// request, not found, ...) means the server is fine and shouldn't punish
// its health score. Any other error (DNS failure, connection refused,
// timeout, ...) is a transport-level problem and always counts.
func isBreakerFailure(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode == 429 || se.StatusCode >= 500
	}
	return true
}

func (cb *circuitBreaker) Execute(fn func() error) error {
	if !cb.enabled {
		return fn()
	}

	_, err := cb.cb.Execute(func() (interface{}, error) {
		return nil, fn()
	})
	if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
		return ErrCircuitOpen
	}
	return err
}

func (cb *circuitBreaker) State() gobreaker.State {
	return cb.cb.State()
}

func (cb *circuitBreaker) IsAvailable() bool {
	if !cb.enabled {
		return true
	}
	return cb.State() != gobreaker.StateOpen
}

var _ Breaker = (*circuitBreaker)(nil)
