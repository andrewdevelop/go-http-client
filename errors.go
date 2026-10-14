package httpclient

import "errors"

// ErrRetriesExhausted is returned once a request's configured
// RetryConfig.MaxAttempts have all been spent on a retryable failure
// without success. Check errors.Is(err, ErrRetriesExhausted) to
// distinguish "we tried and every attempt failed" from a non-retryable
// error, which is returned unwrapped.
var ErrRetriesExhausted = errors.New("httpclient: retry attempts exhausted")

// ErrCircuitOpen is returned when the circuit breaker is open and a
// request is rejected without being sent.
var ErrCircuitOpen = errors.New("httpclient: circuit breaker is open")

// ErrRateLimited is returned when RateLimitConfig.Wait is false and the
// limiter has no tokens available for an immediate request.
var ErrRateLimited = errors.New("httpclient: rate limit exceeded")

// ErrResponseTooLarge is returned when a response body exceeds
// Config.MaxResponseBytes. It is never retried — a response that's too
// large once will be too large on every attempt.
var ErrResponseTooLarge = errors.New("httpclient: response body exceeds MaxResponseBytes")
