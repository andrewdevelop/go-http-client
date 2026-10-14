# go-http-client

A provider-agnostic HTTP client for talking to third-party APIs safely and resiliently: retries with exponential backoff, a circuit breaker, client-side rate limiting, response caching (interface only), and axios-style interceptors.

## Features

- **Retries + backoff** — `RetryConfig`: exponential backoff with jitter, a configurable list of retryable HTTP status codes (429/500/502/503/504 by default), network errors (DNS, connection refused, timeout) are always retried except on context cancellation. Retry/backoff logic itself is a pluggable contract — see `RetryPolicy` below.
- **Circuit breaker** — a wrapper around `sony/gobreaker`: after a run of failures the breaker opens and requests fail instantly (`ErrCircuitOpen`) with no network call. Pluggable contract — see `Breaker` below.
- **Rate limiting** — a wrapper around `golang.org/x/time/rate`: throttles outgoing requests independently of whatever limits the remote API itself enforces, with "wait for a token" or "fail fast" modes (`ErrRateLimited`). Pluggable contract — see `Limiter` below.
- **Response caching (interface only)** — `Cache` describes `Get/Set/Delete`; the library ships no implementation (in-memory, Redis, a CDN-backed store — bring your own). `GET` is cached automatically; any other method needs the request to opt in explicitly via `Request.Cacheable` (needed for JSON-RPC read methods, which are sent as `POST`).
- **Idempotency** — retries never touch a method that isn't idempotent by HTTP semantics (`POST`/`PATCH`) unless the request explicitly marks itself `Request.Idempotent`/`IdempotencyKey`; the idempotency key is sent in a header on every attempt, including retries.
- **Resource limits** — `Config.MaxResponseBytes` guards against an unbounded response body (OOM protection); `Config.MaxConcurrency` is a bulkhead that caps how many requests are in flight at once, independent of request rate (`RateLimit`).
- **No request/response body parsing** — `Request.SetBody`/`Response.Body` deal in raw `[]byte`. The library knows nothing about JSON, protobuf, or anything else — marshal and unmarshal however fits (`encoding/json`, something faster, a big-int-safe decoder for blockchain RPC — whatever you need).
- **Interceptors (axios-style)** — `UseRequestInterceptor` / `UseResponseInterceptor` / `UseErrorInterceptor`, each returning an id for the matching `Eject*`.
- **Mock adapter for tests** — `MockAdapter` (axios-mock-adapter style): `adapter.On(method, path, ...).Reply(status, body, ...)`, with `/users/{id}`-style path placeholders (captured, readable via `PathParams`) and trailing slashes ignored; plugged in via `Config.Transport` — retries/breaker/limiter/cache all run on top of it exactly as they would over a real network call. See "Mock adapter" below.

## Contracts (extension interfaces)

Anything in the client that could reasonably have an alternative implementation is exposed as an exported interface with a built-in default. The common pattern is a `Store`/`Policy` field on the relevant config: when set, the rest of that sub-config's fields are ignored, and the supplied implementation is used directly.

| Contract      | Plugged in via               | Built-in implementation            | When you'd want your own                                                                   |
|---------------|------------------------------|------------------------------------|--------------------------------------------------------------------------------------------|
| `Cache`       | `CacheConfig.Store`          | none (interface only)              | any backend — in-memory, Redis, a CDN                                                      |
| `Limiter`     | `RateLimitConfig.Store`      | `golang.org/x/time/rate`           | a distributed limiter shared across multiple instances/processes                           |
| `Breaker`     | `CircuitBreakerConfig.Store` | `sony/gobreaker`                   | a different breaker library, or shared trip state across instances                         |
| `RetryPolicy` | `RetryConfig.Policy`         | status codes + exponential backoff | honoring a `Retry-After` header (common on LLM/RPC rate limits), a different backoff curve |

Example custom `RetryPolicy` that honors `Retry-After`:

```go
type retryAfterPolicy struct{ fallback time.Duration }

func (p *retryAfterPolicy) ShouldRetry(err error, attempt int) bool {
    var se *httpclient.StatusError
    return errors.As(err, &se) && se.StatusCode == 429 && attempt < 5
}

func (p *retryAfterPolicy) NextDelay(err error, attempt int) time.Duration {
    var se *httpclient.StatusError
    if errors.As(err, &se) {
        if ra := se.Response.Headers.Get("Retry-After"); ra != "" {
            if secs, err := strconv.Atoi(ra); err == nil {
                return time.Duration(secs) * time.Second
            }
        }
    }
    return p.fallback
}

c := httpclient.NewClient(httpclient.Config{
    Retry: httpclient.RetryConfig{Enabled: true, MaxAttempts: 5, Policy: &retryAfterPolicy{fallback: time.Second}},
})
```

## Configuration reference

Every field across `Config` and its sub-configs, grouped by what it controls. "Default" is the zero value's effect unless noted. Fields marked **overrides siblings** make the rest of that group's fields ignored once set — see [Contracts](#contracts-extension-interfaces) above.

### General (`Config`)

| Field | Default | Options | Effect |
|---|---|---|---|
| `BaseURL` | `""` | any URL | Prepended to a `Request.Path` that isn't already absolute (`http://`/`https://`). |
| `Timeout` | `DefaultTimeout` (30s) when `HTTPClient` is nil | any `time.Duration` | Bounds **one physical attempt**, not the whole retry loop. Ignored if `HTTPClient` is set. |
| `HTTPClient` | `nil` (built from `Timeout`/`Transport`/`CheckRedirect`/`Jar`) | any `*http.Client` | Supplying your own means you own all of `Timeout`/`Transport`/`CheckRedirect`/`Jar` yourself — those four `Config` fields are then ignored. |
| `Transport` | `nil` (→ `http.DefaultTransport`) | any `http.RoundTripper` | The underlying `RoundTripper` — plug in an instrumenting/tracing/mocking one (e.g. `MockAdapter`). Ignored if `HTTPClient` is set. |
| `CheckRedirect` | `nil` (net/http default: follow ≤10 redirects) | `func(req *http.Request, via []*http.Request) error` | Redirect policy. Ignored if `HTTPClient` is set. |
| `Jar` | `nil` (no cookies sent/stored) | any `http.CookieJar` | Set only if the remote API relies on cookie sessions. Ignored if `HTTPClient` is set. |
| `Headers` | `nil` | `http.Header` | Merged into every request; per-request headers win on conflict. |
| `IdempotencyHeaderName` | `DefaultIdempotencyHeaderName` (`"Idempotency-Key"`) | any header name | Header `Request.IdempotencyKey` is sent under. |
| `MaxResponseBytes` | `0` (unlimited) | `>0` byte count | Caps response body size read off the wire. Exceeding it fails with `ErrResponseTooLarge`, which is **never retried**. |
| `MaxConcurrency` | `0` (unlimited) | `>0` | Bulkhead: max requests in flight through this `Client` at once. A request beyond the limit **blocks** (bounded by its context) rather than failing fast. Independent of `RateLimit`. |

### Retry (`Config.Retry`, type `RetryConfig`)

| Field | Default | Options | Effect |
|---|---|---|---|
| `Enabled` | `false` | `bool` | Master switch. `false` → no retry loop at all (one attempt; the rate limiter and breaker still run around it). |
| `MaxAttempts` | `0` → treated as `1` | `≥1` | Total attempts **including** the first — `3` means up to 2 retries. |
| `BaseDelay` | `0` | any `time.Duration` | Backoff before the first retry; doubles each subsequent attempt. Floored at 1ms internally so `0` doesn't hot-loop. |
| `MaxDelay` | `0` (no cap) | any `time.Duration` | Ceiling the exponential backoff can't exceed. |
| `Jitter` | `false` | `bool` | Adds up to +50% random jitter on top of the computed delay. |
| `RetryableStatusCodes` | `nil` → `DefaultRetryableStatusCodes` (429, 500, 502, 503, 504) | `[]int` | Which non-2xx statuses trigger a retry. Network errors (DNS, connection refused, timeout) are always retryable regardless of this list. **Ignored if `Policy` is set.** |
| `Policy` | `nil` → built-in status-code policy | `RetryPolicy` — **overrides siblings** (`RetryableStatusCodes`/`BaseDelay`/`MaxDelay`/`Jitter`) | Replaces the per-attempt retry/backoff decision entirely (e.g. to honor `Retry-After`). `MaxAttempts`/`Enabled` still apply around it. |
| `RetryNonIdempotent` | `false` | `bool` | `false` → `POST`/`PATCH` are only retried if the request sets `Idempotent`/`IdempotencyKey`. `true` → retries ignore idempotency entirely (old "retry everything" behavior). Methods idempotent by HTTP semantics (`GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`, `TRACE`) are always retried regardless. |

### Rate limiting (`Config.RateLimit`, type `RateLimitConfig`)

| Field | Default | Options | Effect |
|---|---|---|---|
| `Enabled` | `false` | `bool` | Master switch. `false` → no throttling. |
| `RPS` | `0` | `float64` (`>0`) | Sustained token refill rate (requests/second). |
| `Burst` | `0` | `int` (`≥0`) | Max tokens that can accumulate, i.e. how many requests can fire back-to-back before `RPS` pacing kicks in. |
| `Wait` | `false` | `bool` | `true` → block until a token is available, bounded by the request's context. `false` → fail immediately with `ErrRateLimited` when no token is free. |
| `Store` | `nil` → built-in `golang.org/x/time/rate` limiter | `Limiter` — **overrides siblings** (`Enabled`/`RPS`/`Burst`/`Wait`) | Swap in a distributed limiter shared across instances/processes. |

### Circuit breaker (`Config.CircuitBreaker`, type `CircuitBreakerConfig`)

| Field | Default | Options | Effect |
|---|---|---|---|
| `Enabled` | `false` | `bool` | Master switch. `false` → every request runs straight through, no breaker. |
| `Threshold` | `0` | `int` (practically `≥1`) | Minimum failure count within `Interval` before the breaker becomes eligible to trip (also needs ≥50% failure ratio). |
| `Interval` | `0` | any `time.Duration` | Rolling window the failure count above is measured over. |
| `Timeout` | `0` | any `time.Duration` | How long the breaker stays open before letting one trial request through (half-open). |
| `HalfOpenMaxCalls` | `0` | `int` (practically `≥1`) | Trial requests allowed through while half-open before the breaker fully closes again. |
| `Store` | `nil` → built-in `sony/gobreaker` wrapper | `Breaker` — **overrides siblings** (`Enabled`/`Threshold`/`Interval`/`Timeout`/`HalfOpenMaxCalls`) | Swap in a different breaker library, or one sharing trip state across instances. |

### Cache (`Config.Cache`, type `CacheConfig`)

| Field | Default | Options | Effect |
|---|---|---|---|
| `Store` | `nil` (caching disabled regardless of `Enabled`) | any `Cache` implementation | The cache backend — this package ships only the interface. |
| `Enabled` | `false` | `bool` | Master switch; also needs `Store` set to actually do anything. |
| `TTL` | `0` | any `time.Duration` | Passed to `Cache.Set` for each entry — interpretation (e.g. "no expiration" for `0`) is up to the `Cache` implementation. |
| `KeyFunc` | `nil` → `DefaultCacheKey` (SHA-256 of method + resolved URL + body) | `func(*Request) string` | How a cache key is derived from a request. |

### Per-request overrides (`Request`, not `Config`)

These aren't `Config` fields — they're set per-request via `NewRequest(...).Set*(...)` — but they directly gate the `Cache`/`Retry` groups above, so they're part of the same picture.

| Field | Default | Options | Effect |
|---|---|---|---|
| `Cacheable` | `false` | `bool` | `GET` is cached automatically regardless of this flag; any other method is cached only when this is `true` (and `Cache.Enabled`/`Store` are set). |
| `Idempotent` | `false` | `bool` | Asserts a non-idempotent method (`POST`/`PATCH`) is safe to retry — see `RetryConfig.RetryNonIdempotent`. No effect on methods already idempotent by HTTP semantics. |
| `IdempotencyKey` | `""` | any string (see `NewIdempotencyKey()`) | Sent under `Config.IdempotencyHeaderName` on every attempt; also implies `Idempotent`. |

## Installation

```bash
go get github.com/andrewdevelop/go-http-client
```

## Quick start

```go
c := httpclient.NewClient(httpclient.Config{
    BaseURL: "https://api.example.com",
    Timeout: 10 * time.Second,
    Retry: httpclient.RetryConfig{
        Enabled:     true,
        MaxAttempts: 4,
        BaseDelay:   200 * time.Millisecond,
        MaxDelay:    5 * time.Second,
        Jitter:      true,
    },
    CircuitBreaker: httpclient.CircuitBreakerConfig{
        Enabled:          true,
        Threshold:        5,
        Interval:         time.Minute,
        Timeout:          30 * time.Second,
        HalfOpenMaxCalls: 1,
    },
    RateLimit: httpclient.RateLimitConfig{
        Enabled: true,
        RPS:     10,
        Burst:   20,
        Wait:    true,
    },
})

resp, err := c.Get(ctx, "/v1/status")
if err != nil {
    var se *httpclient.StatusError
    if errors.As(err, &se) {
        // se.StatusCode, se.Body — the error response's body
    }
    return err
}

var status StatusPayload
if err := json.Unmarshal(resp.Body, &status); err != nil {
    return err
}
```

## Blockchain JSON-RPC

```go
type rpcRequest struct {
    JSONRPC string `json:"jsonrpc"`
    Method  string `json:"method"`
    Params  []any  `json:"params"`
    ID      int    `json:"id"`
}

type rpcResponse struct {
    Result json.RawMessage `json:"result"`
    Error  *struct {
        Code    int    `json:"code"`
        Message string `json:"message"`
    } `json:"error"`
}

payload, err := json.Marshal(rpcRequest{
    JSONRPC: "2.0",
    Method:  "eth_blockNumber",
    ID:      1,
})
if err != nil {
    return err
}

// JSON-RPC sends everything as POST, including reads — opt into
// caching explicitly since the library can't tell a read from a
// write by HTTP method alone here.
req := httpclient.NewRequest(http.MethodPost, "/").SetBody(payload).SetCacheable(true)

resp, err := c.Do(ctx, req)
if err != nil {
    return err
}

var rpc rpcResponse
if err := json.Unmarshal(resp.Body, &rpc); err != nil {
    return err
}
if rpc.Error != nil {
    return fmt.Errorf("rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
}
```

## LLM

The client doesn't know about any specific provider — you describe its request/response shape and wire up auth via config headers or an interceptor:

```go
llm := httpclient.NewClient(httpclient.Config{
    BaseURL: "https://api.openai.com/v1",
    Headers: http.Header{
        "Authorization": []string{"Bearer " + apiKey},
    },
    Retry: httpclient.RetryConfig{Enabled: true, MaxAttempts: 3, BaseDelay: time.Second},
})

payload, err := json.Marshal(map[string]any{
    "model":    "gpt-4o-mini",
    "messages": []map[string]string{{"role": "user", "content": "hi"}},
})
if err != nil {
    return err
}

resp, err := llm.Post(ctx, "/chat/completions", payload)
```

The same `Client`, with a different `BaseURL`/headers, works just as well for Gemini or Claude — request/response shape differences stay on the calling code's side.

## Interceptors

```go
c.UseRequestInterceptor(func(ctx context.Context, req *httpclient.Request) (*httpclient.Request, error) {
    req.SetHeader("X-Request-Id", uuid.NewString())
    return req, nil
})

id := c.UseErrorInterceptor(func(ctx context.Context, err error) error {
    log.Printf("request failed: %v", err)
    return err
})
defer c.EjectErrorInterceptor(id)
```

## Cache

```go
type myCache struct{ /* ... */ }

func (m *myCache) Get(ctx context.Context, key string) ([]byte, bool)              { /* ... */ }
func (m *myCache) Set(ctx context.Context, key string, v []byte, ttl time.Duration) { /* ... */ }
func (m *myCache) Delete(ctx context.Context, key string)                           { /* ... */ }

c := httpclient.NewClient(httpclient.Config{
    Cache: httpclient.CacheConfig{
        Enabled: true,
        Store:   &myCache{},
        TTL:     time.Minute,
    },
})
```

`GET` is cached automatically. Any other method is cached only if the request itself is marked `SetCacheable(true)` (see the JSON-RPC example above) — the library has no way to tell a safe-to-cache read from a mutation by HTTP method alone. The default key is a SHA-256 of the method, resolved URL and request body (`DefaultCacheKey`), overridable via `CacheConfig.KeyFunc`.

## Idempotency and retries

By default, only methods that are idempotent by HTTP semantics (`GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`, `TRACE`) are retried. `POST`/`PATCH` are retried only if the request itself asserts it's safe — otherwise there's a real risk of double-submitting a mutation (e.g. re-sending a blockchain transaction) if the server already received the original request but its response was lost to a network error.

```go
// Option 1: just assert that this particular POST is idempotent
// (e.g. a JSON-RPC read method sent as POST).
req := httpclient.NewRequest(http.MethodPost, "/rpc").
    SetBody(payload).
    SetIdempotent(true)

// Option 2: idempotency via a key — the server deduplicates repeated
// attempts carrying the same key.
req := httpclient.NewRequest(http.MethodPost, "/orders").
    SetBody(payload).
    SetIdempotencyKey(httpclient.NewIdempotencyKey()) // or your own business ID

resp, err := c.Do(ctx, req)
```

The key is sent in the `Idempotency-Key` header (configurable via `Config.IdempotencyHeaderName`) on every attempt, including retries — the same value each time, so the server can deduplicate. To disable this gate entirely and go back to "retry everything" — `RetryConfig.RetryNonIdempotent: true`.

### Stripe example

Stripe is the API that made `Idempotency-Key` a convention in the first place, and its semantics are a good concrete illustration of what the header actually buys you: Stripe stores the (key, endpoint) pair for 24 hours. A repeated request with the same key against the same endpoint is **not re-executed** — Stripe just replays the stored response from the first attempt, success or failure. Send the same key with *different* parameters, though, and Stripe rejects it outright — that mismatch usually means a bug (a key got reused across two different charges).

```go
stripe := httpclient.NewClient(httpclient.Config{
    BaseURL: "https://api.stripe.com/v1",
    Headers: http.Header{
        "Authorization": []string{"Bearer " + secretKey},
    },
    Retry: httpclient.RetryConfig{
        Enabled:   true,
        MaxAttempts: 4,
        BaseDelay:   time.Second,
        Jitter:      true,
    },
})

// Stripe's classic APIs take form-encoded bodies, not JSON — this
// package doesn't care either way, it only ever moves raw []byte.
form := url.Values{
    "amount":   {"2000"},
    "currency": {"usd"},
    "source":   {"tok_visa"},
}

req := httpclient.NewRequest(http.MethodPost, "/charges").
    SetBody([]byte(form.Encode())).
    SetHeader("Content-Type", "application/x-www-form-urlencoded").
    SetIdempotencyKey(httpclient.NewIdempotencyKey())

resp, err := stripe.Do(ctx, req)
```

`Config.IdempotencyHeaderName` doesn't even need setting here — it already defaults to `"Idempotency-Key"`, Stripe's own header name.

One easy mistake worth calling out: generate the key **once per logical operation**, not once per `Do` call. Our retry loop already reuses the same `*Request` (and so the same key) across every physical attempt automatically — that part's free. But if the *caller* retries at a higher level (e.g. a user clicks "Pay" again after their browser timed out, with no idea whether the first charge went through), a fresh `httpclient.NewIdempotencyKey()` on that second click defeats the whole point. Generate the key once up front — e.g. when the order is created — persist it alongside the order, and reuse it for every attempt at charging that specific order, however many layers of retry sit above this client.

## Resource limits

```go
c := httpclient.NewClient(httpclient.Config{
    // Guards against an unbounded response body — a misbehaving or
    // malicious server can't exhaust the process's memory.
    MaxResponseBytes: 10 * 1024 * 1024, // 10 MiB

    // Bulkhead: at most 5 requests in flight at once through this
    // Client, independent of RateLimit (which paces rate, not
    // parallelism). A request beyond the limit waits for a free slot,
    // bounded by its context, rather than failing fast.
    MaxConcurrency: 5,
})

resp, err := c.Get(ctx, "/big-report")
if errors.Is(err, httpclient.ErrResponseTooLarge) {
    // body exceeded MaxResponseBytes — never retried, since the
    // problem won't go away on a second attempt.
}
```

## Mock adapter

`MockAdapter` is an `http.RoundTripper` that answers requests from pre-registered rules instead of actually going over the network. It plugs in wherever any other `RoundTripper` would — via `Config.Transport` — so retries, the circuit breaker, the rate limiter and the cache all run on top of it exactly as they would over a real network call. Useful not just for unit-testing individual calls, but for exercising the retry/breaker logic itself without spinning up an `httptest.Server`.

```go
adapter := httpclient.NewMockAdapter()

adapter.On(http.MethodGet, "/users/1").
    Reply(http.StatusOK, []byte(`{"id":1,"name":"Ann"}`))

adapter.On(http.MethodPost, "/orders", httpclient.WithQueryParam("dry_run", "true")).
    Reply(http.StatusCreated, []byte(`{"id":"ord_1"}`), httpclient.WithDelay(50*time.Millisecond))

c := httpclient.NewClient(httpclient.Config{
    BaseURL:   "https://api.example.com",
    Transport: adapter,
})

resp, err := c.Get(ctx, "/users/1") // -> 200, {"id":1,"name":"Ann"}
```

Rules are checked in registration order; the first one matching method + path + matchers wins. A request matching no rule returns `ErrNoMockMatch` — never a silent fall-through to the real network. Leading/trailing slashes never affect matching (`/users/1`, `/users/1/` and `users/1` are all equivalent), and a path segment wrapped in braces — `/users/{id}` — matches any single segment and is captured, readable off the request via `PathParams`/`PathParam`:

```go
adapter.On(http.MethodGet, "/users/{id}").ReplyFunc(func(req *http.Request) (*http.Response, error) {
    id := httpclient.PathParam(req, "id") // "42" for a request to /users/42
    // ...
})
```

A `{placeholder}` matches exactly one path segment, not an arbitrary suffix — `/users/{id}` won't match `/users/42/orders`; register a separate, more specific rule for that. Since the first match wins, put more specific rules (literal paths, or ones with more matchers) before more general ones if their methods overlap.

Matchers for `On` (and `MockRule.WithMatch` — the same thing, applied to an already-built rule):

- `httpclient.WithQuery(url.Values)` / `WithQueryParam(key, value)` — query parameters (subset match: the listed keys must match, everything else is ignored).
- `httpclient.WithRequestHeader(key, value)` — a request header.
- `httpclient.WithBodyMatch(func([]byte) bool)` — an arbitrary body check (e.g. a specific JSON-RPC method inside a `POST` body).

Options for `Reply`:

- `httpclient.WithDelay(d time.Duration)` — respond after waiting `d` instead of instantly (bounded by the request's context, same as a real network call would be) — useful for exercising `Timeout`, backoff, or `MaxConcurrency`.
- `httpclient.WithReplyHeader(key, value)` — a header on the mocked response.

For a dynamic response (echoing the body, varying by attempt count, etc.) use `MockRule.ReplyFunc(func(*http.Request) (*http.Response, error))` instead of `Reply`. `MockAdapter.Calls(rule)` returns how many times a given rule has matched — handy for asserting retries actually happened the expected number of times:

```go
rule := adapter.On(http.MethodGet, "/flaky").ReplyFunc(func(req *http.Request) (*http.Response, error) {
    // ...
})
// after calling c.Get(ctx, "/flaky"):
adapter.Calls(rule) // -> how many times this rule fired
```

## Edge cases & trade-offs

Design decisions that trade something away deliberately — worth knowing before you lean on them.

**Circuit breaker is per-`Client`, not per-endpoint.** One bad path on a `BaseURL` can trip the breaker for every other request through that same `Client`, even to healthy endpoints. If you need per-endpoint isolation, use one `Client` per logical endpoint, or a custom `Breaker` (via `CircuitBreakerConfig.Store`) that keys trip state by path itself.

**Idempotency is a client-side signal only — not a guarantee.** `Request.Idempotent`/`IdempotencyKey` and the retry gate reduce the *risk* of double-submitting a mutation; actual deduplication has to happen server-side (the server is what sees the repeated key and decides to no-op). A server that ignores the `Idempotency-Key` header will still process a retried request twice. The gate is also conservative by method, not by failure mode: a `POST` that failed with "connection refused" before any bytes reached the server is just as safe to retry as a `GET`, but the library can't distinguish that from "the server processed it and the response got lost" — so it blocks both alike unless you explicitly opt in.

**`MaxConcurrency` queues, it doesn't fail fast.** A request beyond the limit blocks until a slot frees up or its context is done. Pair it with a context that has a deadline, or an unbounded burst of callers can pile up waiting instead of getting a quick, actionable error.

**`MaxResponseBytes` is a memory guard, not a bandwidth guard.** It stops `io.ReadAll` from buffering an arbitrarily large body into memory, via `http.MaxBytesReader`. It doesn't pre-empt a slow server, and it doesn't prevent the bytes up to the limit (+1) from actually being transferred — pair it with `Config.Timeout` for the time dimension.

**`RateLimit` and `MaxConcurrency` are orthogonal on purpose.** One paces request *rate*, the other bounds simultaneous *in-flight* requests. Setting only one gives you half a backpressure story — e.g. a generous rate limit with no concurrency cap still lets a slow backend accumulate an unbounded number of in-flight requests if callers keep retrying faster than it responds.

**Setting a contract's `Store` ignores the rest of that sub-config.** `CacheConfig{Store: myCache, TTL: time.Minute}` — if you *also* set `Store`, `TTL`/`KeyFunc` are silently ignored; your `Cache` implementation owns its own expiry notion entirely. Same pattern for `RateLimitConfig.Store`, `CircuitBreakerConfig.Store`, `RetryConfig.Policy`. Easy to mis-set both halves and wonder why the "other" config has no effect.

**Request and response bodies are fully buffered in memory — no streaming.** `Request.SetBody` takes a `[]byte`, and `doOnce` reads the whole response via `io.ReadAll` (bounded by `MaxResponseBytes` if set). This is what makes retry replay and cache-key hashing straightforward, but it rules out streaming very large uploads or downloads through this client as-is.

**Caching is not HTTP-cache-semantics-aware.** There's no `ETag`/`Cache-Control`/`Vary` handling — `Cache` is an opaque key→bytes store keyed by method+URL+body (or your own `KeyFunc`), with a flat TTL. It won't respect a server's `no-store`, won't revalidate, and `Cache.Delete` is never called automatically by the library — invalidation beyond TTL expiry is entirely on the caller.

**`Config.Timeout` bounds one physical attempt, not the whole retry loop.** With `MaxAttempts: 5` and `Timeout: 30s`, a worst case can take several minutes end to end. Bound the total with a `context.WithTimeout` on the `ctx` you pass to `Do`/`Get`/etc. if that matters to you.

**`Config.HTTPClient` silently overrides `Transport`/`CheckRedirect`/`Jar`.** Supplying your own `*http.Client` means you own all three of those — if you also set `Config.Transport` (e.g. for `MockAdapter`), it's ignored. Pick one: either build the `http.Client` yourself, or configure these fields individually and let the package build the client.

**`MockAdapter` path matching is segment-based, not a full router.** `{placeholder}` captures exactly one path segment — there's no wildcard for "the rest of the path", no regex, and no precedence beyond registration order (so a more specific rule must be registered before a more general one it could otherwise shadow). Fine for unit tests; not meant to simulate a full routing layer like `net/http.ServeMux` or `chi`.
