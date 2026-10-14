package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client sends HTTP requests with optional retries/backoff, a circuit
// breaker, client-side rate limiting, response caching and axios-style
// interceptors layered on top of net/http. It is safe for concurrent
// use, and is intentionally provider-agnostic: the same Client can drive
// blockchain JSON-RPC calls, LLM chat completions (OpenAI, Gemini,
// Claude, ...), or any other API with a raw request/response body. This
// package does no body parsing of its own — Request.SetBody and
// Response.Body deal in raw []byte, and marshaling/unmarshaling (JSON,
// protobuf, or anything else) is entirely up to the caller.
type Client struct {
	httpClient *http.Client
	baseURL    string
	headers    http.Header

	idempotencyHeader string
	maxResponseBytes  int64
	concurrency       chan struct{}
	retryCfg          RetryConfig
	limiter           Limiter
	breaker           Breaker
	cache             *responseCache

	reqInterceptors  *interceptorChain[RequestInterceptor]
	respInterceptors *interceptorChain[ResponseInterceptor]
	errInterceptors  *interceptorChain[ErrorInterceptor]
}

// NewClient builds a Client from cfg. See Config for defaults.
func NewClient(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		hc = &http.Client{
			Timeout:       timeout,
			Transport:     cfg.Transport,
			CheckRedirect: cfg.CheckRedirect,
			Jar:           cfg.Jar,
		}
	}

	headers := cfg.Headers
	if headers == nil {
		headers = http.Header{}
	}

	limiter := cfg.RateLimit.Store
	if limiter == nil {
		limiter = newRateLimiter(cfg.RateLimit)
	}

	breaker := cfg.CircuitBreaker.Store
	if breaker == nil {
		breaker = newCircuitBreaker(cfg.CircuitBreaker)
	}

	idempotencyHeader := cfg.IdempotencyHeaderName
	if idempotencyHeader == "" {
		idempotencyHeader = DefaultIdempotencyHeaderName
	}

	var concurrency chan struct{}
	if cfg.MaxConcurrency > 0 {
		concurrency = make(chan struct{}, cfg.MaxConcurrency)
	}

	return &Client{
		httpClient:        hc,
		baseURL:           strings.TrimRight(cfg.BaseURL, "/"),
		headers:           headers,
		idempotencyHeader: idempotencyHeader,
		maxResponseBytes:  cfg.MaxResponseBytes,
		concurrency:       concurrency,
		retryCfg:          cfg.Retry,
		limiter:           limiter,
		breaker:           breaker,
		cache:             newResponseCache(cfg.Cache),
		reqInterceptors:   newInterceptorChain[RequestInterceptor](),
		respInterceptors:  newInterceptorChain[ResponseInterceptor](),
		errInterceptors:   newInterceptorChain[ErrorInterceptor](),
	}
}

// acquireSlot blocks until a concurrency slot is free (bounded by ctx),
// or returns immediately if Config.MaxConcurrency was left unset.
func (c *Client) acquireSlot(ctx context.Context) error {
	if c.concurrency == nil {
		return nil
	}
	select {
	case c.concurrency <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSlot is a no-op when Config.MaxConcurrency was left unset.
func (c *Client) releaseSlot() {
	if c.concurrency == nil {
		return
	}
	<-c.concurrency
}

// UseRequestInterceptor registers i and returns an id for EjectRequestInterceptor.
func (c *Client) UseRequestInterceptor(i RequestInterceptor) int {
	return c.reqInterceptors.Use(i)
}

// EjectRequestInterceptor removes a previously registered request interceptor.
func (c *Client) EjectRequestInterceptor(id int) {
	c.reqInterceptors.Eject(id)
}

// UseResponseInterceptor registers i and returns an id for EjectResponseInterceptor.
func (c *Client) UseResponseInterceptor(i ResponseInterceptor) int {
	return c.respInterceptors.Use(i)
}

// EjectResponseInterceptor removes a previously registered response interceptor.
func (c *Client) EjectResponseInterceptor(id int) {
	c.respInterceptors.Eject(id)
}

// UseErrorInterceptor registers i and returns an id for EjectErrorInterceptor.
func (c *Client) UseErrorInterceptor(i ErrorInterceptor) int {
	return c.errInterceptors.Use(i)
}

// EjectErrorInterceptor removes a previously registered error interceptor.
func (c *Client) EjectErrorInterceptor(id int) {
	c.errInterceptors.Eject(id)
}

// Get issues a GET request for path.
func (c *Client) Get(ctx context.Context, path string) (*Response, error) {
	return c.Do(ctx, NewRequest(http.MethodGet, path))
}

// Post issues a POST request for path with the given raw body (nil for
// none). Marshal body yourself first — encoding/json, protobuf,
// whatever the API expects — and set any headers it needs (e.g.
// Content-Type) via NewRequest/SetHeader/Do directly if this shortcut's
// signature isn't enough.
func (c *Client) Post(ctx context.Context, path string, body []byte) (*Response, error) {
	return c.doWithBody(ctx, http.MethodPost, path, body)
}

// Put issues a PUT request for path with the given raw body (nil for none).
func (c *Client) Put(ctx context.Context, path string, body []byte) (*Response, error) {
	return c.doWithBody(ctx, http.MethodPut, path, body)
}

// Patch issues a PATCH request for path with the given raw body (nil for none).
func (c *Client) Patch(ctx context.Context, path string, body []byte) (*Response, error) {
	return c.doWithBody(ctx, http.MethodPatch, path, body)
}

// Delete issues a DELETE request for path.
func (c *Client) Delete(ctx context.Context, path string) (*Response, error) {
	return c.Do(ctx, NewRequest(http.MethodDelete, path))
}

func (c *Client) doWithBody(ctx context.Context, method, path string, body []byte) (*Response, error) {
	req := NewRequest(method, path)
	if body != nil {
		req.SetBody(body)
	}
	return c.Do(ctx, req)
}

// Do sends req through the request interceptors, the cache, the rate
// limiter, circuit breaker and retry loop, the response interceptors,
// and finally the cache write-back. Caching only applies to GET or a
// request with Cacheable set (see Request.Cacheable); retrying on
// failure only applies to methods that are idempotent by HTTP semantics
// or a request that asserts Idempotent/IdempotencyKey (see
// RetryConfig.RetryNonIdempotent to disable this gate entirely). Any
// error returned — from an interceptor, the retry loop, or a non-2xx
// response wrapped as *StatusError — is passed through the error
// interceptors before being returned.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	resolved, err := c.resolveURL(req)
	if err != nil {
		return nil, c.runErrorInterceptors(ctx, err)
	}
	req.url = resolved

	for _, interceptor := range c.reqInterceptors.ordered() {
		req, err = interceptor(ctx, req)
		if err != nil {
			return nil, c.runErrorInterceptors(ctx, err)
		}
	}

	cacheKey := ""
	if c.cache.appliesTo(req) {
		cacheKey = c.cache.key(req)
		if resp, ok := c.cache.lookup(ctx, cacheKey, req); ok {
			return resp, nil
		}
	}

	if err := c.acquireSlot(ctx); err != nil {
		return nil, c.runErrorInterceptors(ctx, err)
	}
	defer c.releaseSlot()

	retryCfg := c.retryCfg
	if retryCfg.Enabled && !isSafeToRetry(req, retryCfg) {
		retryCfg.Enabled = false
	}

	var resp *Response
	err = withRetry(ctx, retryCfg, c.limiter, c.breaker, func(ctx context.Context) error {
		r, doErr := c.doOnce(ctx, req)
		resp = r
		return doErr
	})
	if err != nil {
		return resp, c.runErrorInterceptors(ctx, err)
	}

	for _, interceptor := range c.respInterceptors.ordered() {
		resp, err = interceptor(ctx, resp)
		if err != nil {
			return resp, c.runErrorInterceptors(ctx, err)
		}
	}

	if cacheKey != "" {
		c.cache.store(ctx, cacheKey, resp)
	}

	return resp, nil
}

// doOnce performs a single physical HTTP round trip for req. It is the
// unit withRetry calls repeatedly on failure.
func (c *Client) doOnce(ctx context.Context, req *Request) (*Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.url, req.bodyReader())
	if err != nil {
		return nil, err
	}
	c.applyHeaders(httpReq, req)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	bodyReader := io.Reader(httpResp.Body)
	if c.maxResponseBytes > 0 {
		bodyReader = http.MaxBytesReader(nil, httpResp.Body, c.maxResponseBytes)
	}

	body, err := io.ReadAll(bodyReader)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("httpclient: %w (limit %d bytes)", ErrResponseTooLarge, c.maxResponseBytes)
		}
		return nil, fmt.Errorf("httpclient: reading response body: %w", err)
	}

	resp := &Response{
		StatusCode: httpResp.StatusCode,
		Status:     httpResp.Status,
		Headers:    httpResp.Header,
		Body:       body,
		Request:    req,
	}

	if httpResp.StatusCode >= 400 {
		return resp, &StatusError{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Body:       body,
			Response:   resp,
		}
	}

	return resp, nil
}

// applyHeaders copies the client's default headers and then req's own
// headers onto httpReq, in that order so per-request headers win on
// conflicts.
func (c *Client) applyHeaders(httpReq *http.Request, req *Request) {
	for k, v := range c.headers {
		httpReq.Header[k] = append([]string(nil), v...)
	}
	for k, v := range req.Headers {
		httpReq.Header[k] = append([]string(nil), v...)
	}
	if req.IdempotencyKey != "" {
		httpReq.Header.Set(c.idempotencyHeader, req.IdempotencyKey)
	}
}

// resolveURL combines req.Path with the client's BaseURL (unless Path is
// already absolute) and merges in req.Query.
func (c *Client) resolveURL(req *Request) (string, error) {
	full := req.Path
	if c.baseURL != "" && !strings.HasPrefix(full, "http://") && !strings.HasPrefix(full, "https://") {
		full = c.baseURL + "/" + strings.TrimLeft(full, "/")
	}

	if len(req.Query) == 0 {
		return full, nil
	}

	u, err := url.Parse(full)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for key, values := range req.Query {
		for _, v := range values {
			q.Add(key, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) runErrorInterceptors(ctx context.Context, err error) error {
	for _, interceptor := range c.errInterceptors.ordered() {
		err = interceptor(ctx, err)
	}
	return err
}
