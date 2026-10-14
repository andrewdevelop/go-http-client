package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_GetRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL})
	resp, err := c.Get(context.Background(), "/ping")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// This package does no JSON parsing of its own — decode with
	// whatever the caller brings, e.g. encoding/json here.
	var out struct{ OK bool }
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !out.OK {
		t.Fatalf("expected ok=true, got %+v", out)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClient_CustomTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var used atomic.Bool
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return http.DefaultTransport.RoundTrip(r)
	})

	c := NewClient(Config{BaseURL: srv.URL, Transport: transport})
	if _, err := c.Get(context.Background(), "/ping"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !used.Load() {
		t.Fatal("expected custom Transport to be used")
	}
}

func TestClient_CheckRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})

	resp, err := c.Get(context.Background(), "/redirect")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect to be left unfollowed (302), got %d", resp.StatusCode)
	}
}

func TestClient_Jar(t *testing.T) {
	var sawCookie atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err == nil {
			sawCookie.Store(true)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc"})
	}))
	defer srv.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	c := NewClient(Config{BaseURL: srv.URL, Jar: jar})
	ctx := context.Background()
	if _, err := c.Get(ctx, "/login"); err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	if _, err := c.Get(ctx, "/login"); err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if !sawCookie.Load() {
		t.Fatal("expected cookie from first response to be sent back on second request")
	}
}

// customLimiter is a minimal Limiter that just counts calls and always
// rejects the Nth one — stands in for e.g. a distributed/Redis-backed
// limiter a caller might plug in via RateLimitConfig.Store.
type customLimiter struct {
	calls   atomic.Int32
	rejectN int32
}

func (l *customLimiter) Acquire(ctx context.Context) error {
	if l.calls.Add(1) == l.rejectN {
		return ErrRateLimited
	}
	return nil
}

func TestClient_CustomLimiter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	limiter := &customLimiter{rejectN: 2}
	c := NewClient(Config{
		BaseURL:   srv.URL,
		RateLimit: RateLimitConfig{Store: limiter},
	})

	ctx := context.Background()
	if _, err := c.Get(ctx, "/ping"); err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	if _, err := c.Get(ctx, "/ping"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected custom limiter to reject call 2 with ErrRateLimited, got %v", err)
	}
	if got := limiter.calls.Load(); got != 2 {
		t.Fatalf("expected custom limiter to be consulted twice, got %d", got)
	}
}

// customBreaker is a minimal Breaker that trips open permanently after
// the first failure — stands in for a different breaker library or one
// sharing state across Client instances, plugged in via
// CircuitBreakerConfig.Store.
type customBreaker struct {
	open atomic.Bool
}

func (b *customBreaker) Execute(fn func() error) error {
	if b.open.Load() {
		return ErrCircuitOpen
	}
	if err := fn(); err != nil {
		b.open.Store(true)
		return err
	}
	return nil
}

func (b *customBreaker) IsAvailable() bool { return !b.open.Load() }

func TestClient_CustomBreaker(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	breaker := &customBreaker{}
	c := NewClient(Config{
		BaseURL:        srv.URL,
		CircuitBreaker: CircuitBreakerConfig{Store: breaker},
	})

	ctx := context.Background()
	if _, err := c.Get(ctx, "/fail"); err == nil {
		t.Fatal("expected first call to fail with 500")
	}
	if _, err := c.Get(ctx, "/fail"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected second call to be rejected by open custom breaker, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected only 1 upstream hit (second blocked by breaker), got %d", got)
	}
}

func TestClient_RetryThenSucceed(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL,
		Retry: RetryConfig{
			Enabled:     true,
			MaxAttempts: 5,
			BaseDelay:   time.Millisecond,
		},
	})

	resp, err := c.Get(context.Background(), "/flaky")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestClient_NonRetryableStatus(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL,
		Retry:   RetryConfig{Enabled: true, MaxAttempts: 5, BaseDelay: time.Millisecond},
	})

	_, err := c.Get(context.Background(), "/missing")
	if err == nil {
		t.Fatal("expected error")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if se.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", se.StatusCode)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt (404 is not retryable), got %d", got)
	}
}

// memCache is a minimal in-memory Cache used only by tests — this
// package ships the Cache interface, not an implementation.
type memCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemCache() *memCache { return &memCache{m: map[string][]byte{}} }

func (c *memCache) Get(ctx context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *memCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
}

func (c *memCache) Delete(ctx context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

func TestClient_Cache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"n":1}`))
	}))
	defer srv.Close()

	cache := newMemCache()
	c := NewClient(Config{
		BaseURL: srv.URL,
		Cache:   CacheConfig{Enabled: true, Store: cache, TTL: time.Minute},
	})

	ctx := context.Background()
	if _, err := c.Get(ctx, "/data"); err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	if _, err := c.Get(ctx, "/data"); err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 upstream hit (second should be served from cache), got %d", got)
	}
}

func TestClient_Interceptors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Trace") != "abc" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL})
	c.UseRequestInterceptor(func(ctx context.Context, req *Request) (*Request, error) {
		req.SetHeader("X-Trace", "abc")
		return req, nil
	})

	var sawErr error
	c.UseErrorInterceptor(func(ctx context.Context, err error) error {
		sawErr = err
		return err
	})

	resp, err := c.Get(context.Background(), "/secure")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if sawErr != nil {
		t.Fatalf("error interceptor should not have fired, got %v", sawErr)
	}
}

func TestClient_PostRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL})

	// Caller marshals the body itself — this package only moves bytes.
	payload, err := json.Marshal(map[string]string{"hello": "world"})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	resp, err := c.Post(context.Background(), "/echo", payload)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if out["hello"] != "world" {
		t.Fatalf("unexpected body: %+v", out)
	}
}

// fixedDelayPolicy is a minimal RetryPolicy that retries any error
// exactly twice with a fixed delay — stands in for e.g. a policy that
// honors a Retry-After header instead of exponential backoff.
type fixedDelayPolicy struct {
	delay time.Duration
}

func (p *fixedDelayPolicy) ShouldRetry(err error, attempt int) bool        { return attempt < 3 }
func (p *fixedDelayPolicy) NextDelay(err error, attempt int) time.Duration { return p.delay }

func TestClient_CustomRetryPolicy(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusTeapot) // not in DefaultRetryableStatusCodes
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL,
		Retry: RetryConfig{
			Enabled:     true,
			MaxAttempts: 5,
			Policy:      &fixedDelayPolicy{delay: time.Millisecond},
		},
	})

	resp, err := c.Get(context.Background(), "/flaky")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected custom policy to retry a normally-non-retryable 418 twice, got %d attempts", got)
	}
}

func TestClient_CacheNonGET(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cache := newMemCache()
	c := NewClient(Config{
		BaseURL: srv.URL,
		Cache:   CacheConfig{Enabled: true, Store: cache, TTL: time.Minute},
	})
	ctx := context.Background()

	// POST without Cacheable: not cached, hits upstream every time.
	plain := func() *Request { return NewRequest(http.MethodPost, "/rpc").SetBody([]byte(`{"method":"read"}`)) }
	if _, err := c.Do(ctx, plain()); err != nil {
		t.Fatalf("Do 1: %v", err)
	}
	if _, err := c.Do(ctx, plain()); err != nil {
		t.Fatalf("Do 2: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected POST without Cacheable to bypass the cache (2 hits), got %d", got)
	}

	// POST with Cacheable: cached after the first call.
	cacheable := func() *Request {
		return NewRequest(http.MethodPost, "/rpc").SetBody([]byte(`{"method":"read2"}`)).SetCacheable(true)
	}
	if _, err := c.Do(ctx, cacheable()); err != nil {
		t.Fatalf("Do 3: %v", err)
	}
	if _, err := c.Do(ctx, cacheable()); err != nil {
		t.Fatalf("Do 4: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("expected POST with Cacheable to hit upstream once more (3 total), got %d", got)
	}
}

func TestClient_RetryGatedByIdempotency(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL,
		Retry:   RetryConfig{Enabled: true, MaxAttempts: 3, BaseDelay: time.Millisecond},
	})

	// Plain POST: retries are gated off without Idempotent/IdempotencyKey.
	_, err := c.Do(context.Background(), NewRequest(http.MethodPost, "/submit").SetBody([]byte(`{}`)))
	if err == nil {
		t.Fatal("expected an error from the 503")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected non-idempotent POST to NOT be retried (1 attempt), got %d", got)
	}

	// Same POST, but marked Idempotent: retries proceed normally.
	hits.Store(0)
	_, err = c.Do(context.Background(), NewRequest(http.MethodPost, "/submit").SetBody([]byte(`{}`)).SetIdempotent(true))
	if err == nil {
		t.Fatal("expected an error from the 503")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("expected Idempotent POST to be retried up to MaxAttempts (3), got %d", got)
	}
}

func TestClient_IdempotencyKeyHeader(t *testing.T) {
	var mu sync.Mutex
	var seenKeys []string
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenKeys = append(seenKeys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL: srv.URL,
		Retry:   RetryConfig{Enabled: true, MaxAttempts: 5, BaseDelay: time.Millisecond},
	})

	key := NewIdempotencyKey()
	_, err := c.Do(context.Background(), NewRequest(http.MethodPost, "/submit").SetBody([]byte(`{}`)).SetIdempotencyKey(key))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenKeys) != 3 {
		t.Fatalf("expected 3 attempts to be recorded, got %d", len(seenKeys))
	}
	for i, k := range seenKeys {
		if k == "" {
			t.Fatalf("attempt %d: Idempotency-Key header was empty", i)
		}
		if k != key {
			t.Fatalf("attempt %d: Idempotency-Key %q differs from original %q", i, k, key)
		}
	}
}

func TestClient_MaxResponseBytes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(make([]byte, 1024))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:          srv.URL,
		MaxResponseBytes: 16,
		Retry:            RetryConfig{Enabled: true, MaxAttempts: 3, BaseDelay: time.Millisecond},
	})

	_, err := c.Get(context.Background(), "/big")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected ErrResponseTooLarge to never be retried (1 attempt), got %d", got)
	}
}

func TestClient_MaxConcurrency(t *testing.T) {
	var inflight atomic.Int32
	var maxInflight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			cur := maxInflight.Load()
			if n <= cur || maxInflight.CompareAndSwap(cur, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inflight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, MaxConcurrency: 1})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), "/slow"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("expected MaxConcurrency=1 to cap simultaneous in-flight requests at 1, observed %d", got)
	}
}
