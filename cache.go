package httpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// Cache is the storage interface Client consults before sending a GET
// request and populates after a successful one. This package ships only
// the interface — plug in an in-memory map, Redis, a CDN-backed store,
// whatever fits the host application. A nil Cache (the default) disables
// caching entirely.
type Cache interface {
	// Get returns the cached value for key and true on a hit, or nil and
	// false on a miss or error. Implementations should treat their own
	// failures as a miss rather than surfacing an error to the caller —
	// a cache is an optimization, not a dependency the request should
	// fail on.
	Get(ctx context.Context, key string) ([]byte, bool)

	// Set stores value under key with the given TTL. A TTL of zero means
	// "no expiration" and is left to the implementation to interpret.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)

	// Delete removes key, if present.
	Delete(ctx context.Context, key string)
}

// DefaultCacheKey derives a cache key from a request's method, resolved
// URL and body. Two requests that are identical in all three hash to the
// same key regardless of header order or Go map iteration order.
func DefaultCacheKey(req *Request) string {
	h := sha256.New()
	h.Write([]byte(req.Method))
	h.Write([]byte{0})
	h.Write([]byte(req.url))
	h.Write([]byte{0})
	h.Write(req.bodyBytes)
	return hex.EncodeToString(h.Sum(nil))
}

// cachedPayload is the serialized form a Cache implementation actually
// stores — just enough to reconstruct a Response on a hit.
type cachedPayload struct {
	StatusCode int    `json:"status_code"`
	Status     string `json:"status"`
	Body       []byte `json:"body"`
}

// responseCache wraps CacheConfig with the lookup/store behavior
// Client.Do needs, keeping key derivation and the on-the-wire
// serialization format out of Client itself.
type responseCache struct {
	cfg CacheConfig
}

func newResponseCache(cfg CacheConfig) *responseCache {
	return &responseCache{cfg: cfg}
}

// appliesTo reports whether req is eligible for caching at all. GET is
// always eligible (when a Store is configured); any other method needs
// req.Cacheable set explicitly — this package has no way to tell a
// safe-to-cache read from a mutation by HTTP method alone (e.g. a
// JSON-RPC read sent as POST).
func (c *responseCache) appliesTo(req *Request) bool {
	if !c.cfg.Enabled || c.cfg.Store == nil {
		return false
	}
	return req.Method == http.MethodGet || req.Cacheable
}

// key derives req's cache key via CacheConfig.KeyFunc, falling back to
// DefaultCacheKey.
func (c *responseCache) key(req *Request) string {
	keyFunc := c.cfg.KeyFunc
	if keyFunc == nil {
		keyFunc = DefaultCacheKey
	}
	return keyFunc(req)
}

// lookup returns a Response reconstructed from a cache hit under key, or
// ok=false on a miss or a corrupt cached entry.
func (c *responseCache) lookup(ctx context.Context, key string, req *Request) (*Response, bool) {
	cached, ok := c.cfg.Store.Get(ctx, key)
	if !ok {
		return nil, false
	}
	var payload cachedPayload
	if err := json.Unmarshal(cached, &payload); err != nil {
		return nil, false
	}
	return &Response{
		StatusCode: payload.StatusCode,
		Status:     payload.Status,
		Headers:    http.Header{},
		Body:       payload.Body,
		Request:    req,
	}, true
}

// store saves resp under key for later lookup. Marshaling failures are
// swallowed — a cache write failing shouldn't fail the request that
// already succeeded.
func (c *responseCache) store(ctx context.Context, key string, resp *Response) {
	payload, err := json.Marshal(cachedPayload{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       resp.Body,
	})
	if err != nil {
		return
	}
	c.cfg.Store.Set(ctx, key, payload, c.cfg.TTL)
}
