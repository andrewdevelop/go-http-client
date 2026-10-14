package httpclient

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// NewIdempotencyKey generates a random key suitable for
// Request.IdempotencyKey, for callers that don't already have a
// business identifier (an order ID, a transaction nonce, ...) to use
// instead.
func NewIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing indicates a broken system entropy
		// source — nothing sensible to return instead.
		panic("httpclient: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// idempotentMethods lists the HTTP methods that are idempotent by
// spec — safe to retry without any extra assertion from the caller.
var idempotentMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPut:     true,
	http.MethodDelete:  true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// isSafeToRetry reports whether req may be retried under cfg. Methods
// that are idempotent by HTTP semantics always are; a POST/PATCH (or
// any other non-idempotent method) needs cfg.RetryNonIdempotent, or the
// request to assert it itself via Idempotent/IdempotencyKey.
func isSafeToRetry(req *Request, cfg RetryConfig) bool {
	if cfg.RetryNonIdempotent {
		return true
	}
	if idempotentMethods[req.Method] {
		return true
	}
	return req.Idempotent || req.IdempotencyKey != ""
}
