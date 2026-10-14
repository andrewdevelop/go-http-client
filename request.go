package httpclient

import (
	"bytes"
	"net/http"
	"net/url"
)

// Request is a provider-agnostic HTTP request: a blockchain RPC call, an
// LLM chat completion, or any other API call with a raw body. This
// package does no body encoding of its own — build the payload with
// whatever library fits (encoding/json, protobuf, ...) and pass the
// resulting bytes to SetBody. Build a Request with NewRequest and the
// Set* helpers, then pass it to Client.Do (or use the
// Get/Post/Put/Patch/Delete shortcuts).
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Headers http.Header

	// Cacheable opts a non-GET request into response caching (GET is
	// always cacheable when a Cache is configured; this package can't
	// tell a safe-to-cache read from a mutation by HTTP method alone for
	// e.g. JSON-RPC, which sends everything as POST). Ignored for GET.
	Cacheable bool

	// Idempotent asserts that retrying this request is safe even though
	// its HTTP method isn't idempotent by spec (POST, PATCH) — e.g. a
	// JSON-RPC read sent as POST. Ignored for methods that are already
	// idempotent by HTTP semantics (GET, HEAD, PUT, DELETE, OPTIONS,
	// TRACE), which are always retried regardless of this flag. See
	// RetryConfig.RetryNonIdempotent for a client-wide override.
	Idempotent bool

	// IdempotencyKey, if set, is sent as the configured idempotency
	// header (see Config.IdempotencyHeaderName) on every attempt — the
	// same value on retries, so the server can deduplicate — and also
	// implies Idempotent. Generate one with NewIdempotencyKey, or pass a
	// business identifier you already have (e.g. an order ID).
	IdempotencyKey string

	// url and bodyBytes are filled in by Client.Do: url is Path resolved
	// against the client's BaseURL (plus Query), and bodyBytes is the
	// request body buffered once so it can be replayed across retries
	// and hashed for the cache key.
	url       string
	bodyBytes []byte
}

// NewRequest builds a Request for method and path. path may be a full
// URL or a path relative to the Client's BaseURL.
func NewRequest(method, path string) *Request {
	return &Request{
		Method:  method,
		Path:    path,
		Query:   url.Values{},
		Headers: http.Header{},
	}
}

// SetHeader sets a header, overwriting any existing value. Returns the
// receiver for chaining.
func (r *Request) SetHeader(key, value string) *Request {
	r.Headers.Set(key, value)
	return r
}

// SetQuery sets a query parameter, overwriting any existing value(s) for
// key. Returns the receiver for chaining.
func (r *Request) SetQuery(key, value string) *Request {
	r.Query.Set(key, value)
	return r
}

// SetBody sets a raw request body. Returns the receiver for chaining.
func (r *Request) SetBody(body []byte) *Request {
	r.bodyBytes = body
	return r
}

// SetCacheable opts a non-GET request into response caching; see
// Request.Cacheable. Returns the receiver for chaining.
func (r *Request) SetCacheable(v bool) *Request {
	r.Cacheable = v
	return r
}

// SetIdempotent asserts that retrying this request is safe; see
// Request.Idempotent. Returns the receiver for chaining.
func (r *Request) SetIdempotent(v bool) *Request {
	r.Idempotent = v
	return r
}

// SetIdempotencyKey sets the idempotency key sent on every attempt; see
// Request.IdempotencyKey. Returns the receiver for chaining.
func (r *Request) SetIdempotencyKey(key string) *Request {
	r.IdempotencyKey = key
	return r
}

// Body returns the buffered request body, or nil if none was set.
func (r *Request) Body() []byte {
	return r.bodyBytes
}

// URL returns the request's fully resolved URL. Empty until the request
// has been passed through Client.Do at least once.
func (r *Request) URL() string {
	return r.url
}

func (r *Request) bodyReader() *bytes.Reader {
	return bytes.NewReader(r.bodyBytes)
}
