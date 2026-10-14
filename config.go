package httpclient

import (
	"net/http"
	"time"
)

// Config configures a Client. All sub-configs default to disabled/no-op
// when left at their zero value, so a bare Config{} yields a plain HTTP
// client with sane timeouts and nothing else switched on.
type Config struct {
	// BaseURL, if set, is prepended to any request URL that doesn't
	// already look absolute (see Request.resolveURL).
	BaseURL string

	// Timeout bounds a single HTTP round trip (one physical attempt),
	// not the whole retry loop. Defaults to DefaultTimeout.
	Timeout time.Duration

	// HTTPClient, if set, is used as the underlying transport instead of
	// a client constructed from Timeout. Supplying one of your own means
	// you own Timeout/Transport configuration for it — Transport below
	// is ignored in that case.
	HTTPClient *http.Client

	// Transport, if set, is used as the RoundTripper for the client this
	// package constructs (ignored when HTTPClient is set — that client
	// already owns its own Transport). Plug in an instrumenting,
	// tracing, or mocking RoundTripper here without touching the
	// retry/breaker/cache logic layered on top in Client.Do.
	Transport http.RoundTripper

	// CheckRedirect, if set, is used as the underlying http.Client's
	// CheckRedirect (ignored when HTTPClient is set — that client
	// already owns its own policy). Left nil, net/http's default
	// applies: follow up to 10 redirects, forwarding most request
	// headers — including Authorization when the redirect stays on the
	// same host, but dropping it on a cross-host redirect. Set this to
	// enforce a stricter policy (e.g. refuse all redirects) when
	// sending credentials to a third-party API you don't fully trust.
	CheckRedirect func(req *http.Request, via []*http.Request) error

	// Jar, if set, is used as the underlying http.Client's CookieJar
	// (ignored when HTTPClient is set — that client already owns its
	// own jar). Left nil, no cookies are sent or stored, which is fine
	// for the common case of header/token-authenticated APIs (RPC, LLM,
	// most REST). Set this only if the remote API actually relies on
	// cookie-based sessions — e.g. a jar built with net/http/cookiejar.New.
	Jar http.CookieJar

	// Headers are merged into every outgoing request, with per-request
	// headers taking precedence on conflicts.
	Headers http.Header

	// IdempotencyHeaderName is the header Request.IdempotencyKey (when
	// set) is sent under. Defaults to "Idempotency-Key" (the Stripe
	// convention) when left empty.
	IdempotencyHeaderName string

	// MaxResponseBytes caps how much of a response body Client reads
	// off the wire. 0 (the default) means no limit. Exceeding it fails
	// the request with ErrResponseTooLarge, which is never retried —
	// protects against a misbehaving or malicious server trying to
	// exhaust memory via an unbounded response.
	MaxResponseBytes int64

	// MaxConcurrency bounds how many requests this Client sends at
	// once — a bulkhead, distinct from RateLimit (which paces the rate
	// of requests, not how many are in flight simultaneously). 0 (the
	// default) means no limit. A request beyond the limit blocks until
	// a slot frees up or its context is done; it does not fail fast.
	MaxConcurrency int

	Retry          RetryConfig
	RateLimit      RateLimitConfig
	CircuitBreaker CircuitBreakerConfig

	// Cache is consulted before a request is sent and populated after a
	// successful response. Nil (the default) disables caching entirely —
	// this package only ships the Cache interface, not an implementation.
	Cache CacheConfig
}

// DefaultTimeout is used when Config.Timeout is left at zero.
const DefaultTimeout = 30 * time.Second

// DefaultIdempotencyHeaderName is used when Config.IdempotencyHeaderName
// is left empty.
const DefaultIdempotencyHeaderName = "Idempotency-Key"

// RetryConfig controls whether and how failed requests are retried with
// exponential backoff.
type RetryConfig struct {
	Enabled bool

	// MaxAttempts is the total number of attempts (including the first),
	// so MaxAttempts: 3 means "the original try plus up to 2 retries".
	// Values < 1 are treated as 1.
	MaxAttempts int

	// BaseDelay is the backoff delay before the first retry; it doubles
	// on each subsequent attempt (capped at MaxDelay).
	BaseDelay time.Duration
	MaxDelay  time.Duration
	Jitter    bool

	// RetryableStatusCodes lists HTTP status codes that should trigger a
	// retry. Defaults to DefaultRetryableStatusCodes when nil. Ignored
	// if Policy is set.
	RetryableStatusCodes []int

	// Policy, if set, is used as the RetryPolicy instead of the built-in
	// status-code-based one — RetryableStatusCodes/BaseDelay/MaxDelay/
	// Jitter above are then ignored; it is this RetryPolicy's job to
	// interpret its own notion of retryable/backoff. MaxAttempts and
	// Enabled still apply — Policy only replaces the per-attempt
	// retry/backoff decision, not the attempt-counting loop around it.
	Policy RetryPolicy

	// RetryNonIdempotent, when true, disables the idempotency gate:
	// Client.Do retries any method on a retryable failure, including
	// POST/PATCH, regardless of Request.Idempotent/IdempotencyKey. Left
	// false (the default), a POST/PATCH request is only retried if it
	// sets Idempotent or IdempotencyKey — retrying a non-idempotent
	// request blindly risks double-submitting a mutation (e.g.
	// re-sending a blockchain transaction) when the original request
	// actually reached the server but its response was lost. Methods
	// that are idempotent by HTTP semantics (GET, HEAD, PUT, DELETE,
	// OPTIONS, TRACE) are always retried regardless of this setting.
	RetryNonIdempotent bool
}

// DefaultRetryableStatusCodes is used when RetryConfig.RetryableStatusCodes
// is nil: 429 (rate limited) and the common transient 5xx responses.
var DefaultRetryableStatusCodes = []int{
	http.StatusTooManyRequests,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

// RateLimitConfig throttles outgoing requests client-side, independent of
// whatever rate limiting the remote API enforces.
type RateLimitConfig struct {
	Enabled bool

	// RPS is the sustained request rate; Burst is the maximum number of
	// requests allowed to fire back-to-back before RPS pacing kicks in.
	RPS   float64
	Burst int

	// Wait, when true, blocks until a token is available (bounded by the
	// request's context). When false, a request made with no tokens
	// available fails immediately with ErrRateLimited.
	Wait bool

	// Store, if set, is used as the Limiter instead of the built-in
	// golang.org/x/time/rate-backed one — Enabled/RPS/Burst/Wait above
	// are then ignored; it is this Limiter's job to interpret its own
	// configuration. Plug in a distributed limiter here when multiple
	// Client instances/processes need to share one rate budget.
	Store Limiter
}

// CircuitBreakerConfig trips a breaker after a run of failures, so a
// downstream outage fails fast instead of every caller retrying into it.
type CircuitBreakerConfig struct {
	// Enabled defaults to false — a bare CircuitBreakerConfig{} (or the
	// zero value left in Config.CircuitBreaker) runs every request
	// straight through, with no breaker in the way. Worth turning on
	// for high-concurrency fan-out to one backend; for low-QPS/sequential
	// callers, retry+backoff alone is usually enough.
	Enabled bool

	// Threshold is the minimum number of failures (within Interval)
	// before the breaker is eligible to trip.
	Threshold int

	// Interval is the rolling window failure counts are measured over.
	Interval time.Duration

	// Timeout is how long the breaker stays open before allowing a
	// single trial request through (half-open).
	Timeout time.Duration

	// HalfOpenMaxCalls is the number of trial requests allowed through
	// while half-open before the breaker fully closes.
	HalfOpenMaxCalls int

	// Store, if set, is used as the Breaker instead of the built-in
	// gobreaker-backed one — Enabled/Threshold/Interval/Timeout/
	// HalfOpenMaxCalls above are then ignored; it is this Breaker's job
	// to interpret its own configuration. Plug in a different breaker
	// library, or one that shares trip state across multiple Client
	// instances, here.
	Store Breaker
}

// CacheConfig controls response caching. GET requests are cached
// automatically; any other method needs the individual Request's
// Cacheable flag set (see Client.Do and Request.Cacheable) — useful for
// e.g. JSON-RPC reads, which are sent as POST.
type CacheConfig struct {
	// Store is the cache backend. Nil disables caching regardless of
	// Enabled.
	Store Cache

	Enabled bool

	// TTL is passed to Cache.Set for each stored entry.
	TTL time.Duration

	// KeyFunc derives a cache key from a request. Defaults to
	// DefaultCacheKey when nil.
	KeyFunc func(*Request) string
}
