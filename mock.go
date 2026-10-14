package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNoMockMatch is returned by MockAdapter.RoundTrip when an incoming
// request doesn't match any registered rule — a mock has no network to
// silently fall back to, so an unmatched request is always an error
// rather than a real HTTP call going out.
var ErrNoMockMatch = errors.New("httpclient: no mock rule matches this request")

// MockAdapter is an http.RoundTripper that answers requests from a set
// of registered rules instead of hitting the network. Plug it in via
// Config.Transport to unit-test code built on this package:
//
//	adapter := httpclient.NewMockAdapter()
//	adapter.On(http.MethodGet, "/users/1").Reply(200, []byte(`{"id":1}`))
//	adapter.On(http.MethodPost, "/users").
//	    Reply(201, []byte(`{"id":2}`), httpclient.WithDelay(50*time.Millisecond))
//
//	c := httpclient.NewClient(httpclient.Config{
//	    BaseURL:   "https://api.example.com",
//	    Transport: adapter,
//	})
//
// Rules are matched in registration order; the first rule whose method,
// path and matchers (query/header/body, if any were given to On) all
// match wins. Safe for concurrent use — retries, the rate limiter and
// the circuit breaker all layer on top exactly as they would over a
// real network call, since MockAdapter only replaces the RoundTripper.
type MockAdapter struct {
	mu    sync.Mutex
	rules []*MockRule
	calls map[*MockRule]int
}

// NewMockAdapter builds an empty MockAdapter with no rules registered.
func NewMockAdapter() *MockAdapter {
	return &MockAdapter{calls: make(map[*MockRule]int)}
}

// On registers a new rule matching method and path (path is matched
// against the request URL's Path only, not the query string — use
// WithQuery/WithQueryParam in opts to also match query parameters).
// Leading/trailing slashes are ignored on both sides, so "/users/1" and
// "/users/1/" match each other. A segment in path wrapped in braces —
// e.g. "/users/{id}" — matches any single path segment and is captured;
// read it back in a ReplyFunc (or after Do returns, off the *Request
// you built) via PathParams(req) or PathParam(req, "id"). Returns the
// rule so Reply (or ReplyFunc) can finish it.
func (a *MockAdapter) On(method, path string, opts ...MatchOption) *MockRule {
	r := &MockRule{method: strings.ToUpper(method), path: path}
	for _, opt := range opts {
		opt(r)
	}

	a.mu.Lock()
	a.rules = append(a.rules, r)
	a.mu.Unlock()
	return r
}

// Reset removes every registered rule and resets all call counts.
func (a *MockAdapter) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rules = nil
	a.calls = make(map[*MockRule]int)
}

// Calls reports how many times rule has matched an incoming request so
// far.
func (a *MockAdapter) Calls(rule *MockRule) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[rule]
}

// RoundTrip implements http.RoundTripper.
func (a *MockAdapter) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("httpclient: mock adapter reading request body: %w", err)
	}

	a.mu.Lock()
	var matched *MockRule
	var params map[string]string
	for _, r := range a.rules {
		if p, ok := r.matches(req, body); ok {
			matched = r
			params = p
			break
		}
	}
	if matched != nil {
		a.calls[matched]++
	}
	a.mu.Unlock()

	if matched == nil {
		return nil, fmt.Errorf("%w: %s %s", ErrNoMockMatch, req.Method, req.URL.Path)
	}
	if len(params) > 0 {
		req = req.WithContext(withPathParams(req.Context(), params))
	}
	return matched.respond(req)
}

var _ http.RoundTripper = (*MockAdapter)(nil)

// MockRule is one registered method+path match, built via
// MockAdapter.On and finished with Reply or ReplyFunc.
type MockRule struct {
	method string
	path   string

	query  url.Values
	header http.Header
	body   func([]byte) bool

	status     int
	respBody   []byte
	respHeader http.Header
	delay      time.Duration
	fn         func(*http.Request) (*http.Response, error)
}

// MatchOption narrows what a rule matches, passed to On or WithMatch.
type MatchOption func(*MockRule)

// WithQuery requires every key in values to be present in the request's
// query string with the same (first) value. Keys not listed in values
// are ignored — this matches a subset, not an exact set.
func WithQuery(values url.Values) MatchOption {
	return func(r *MockRule) { r.query = values }
}

// WithQueryParam is a single-parameter shorthand for WithQuery.
func WithQueryParam(key, value string) MatchOption {
	return func(r *MockRule) {
		if r.query == nil {
			r.query = url.Values{}
		}
		r.query.Set(key, value)
	}
}

// WithRequestHeader requires the request to carry header set to value.
func WithRequestHeader(key, value string) MatchOption {
	return func(r *MockRule) {
		if r.header == nil {
			r.header = http.Header{}
		}
		r.header.Set(key, value)
	}
}

// WithBodyMatch requires match(body) to return true for the request's
// raw body.
func WithBodyMatch(match func(body []byte) bool) MatchOption {
	return func(r *MockRule) { r.body = match }
}

// WithMatch applies opts to an already-registered rule. Returns the
// receiver for chaining, so matchers can be added either via On's
// variadic opts or fluently afterwards.
func (r *MockRule) WithMatch(opts ...MatchOption) *MockRule {
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ReplyOption customizes the response a rule sends on a match, passed
// to Reply.
type ReplyOption func(*MockRule)

// WithDelay makes the rule wait d before responding — useful for
// exercising Config.Timeout, retry backoff, or MaxConcurrency in tests.
// The wait is bounded by the request's context, same as a real network
// call would be.
func WithDelay(d time.Duration) ReplyOption {
	return func(r *MockRule) { r.delay = d }
}

// WithReplyHeader sets a header on the mocked response.
func WithReplyHeader(key, value string) ReplyOption {
	return func(r *MockRule) {
		if r.respHeader == nil {
			r.respHeader = http.Header{}
		}
		r.respHeader.Set(key, value)
	}
}

// Reply finishes the rule: status and body are returned verbatim on a
// match, after waiting out any WithDelay. Returns the receiver, mostly
// so a caller can hold onto it for MockAdapter.Calls.
func (r *MockRule) Reply(status int, body []byte, opts ...ReplyOption) *MockRule {
	r.status = status
	r.respBody = body
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ReplyFunc finishes the rule with a dynamic responder instead of a
// fixed status/body — fn receives the matched *http.Request (its body
// already restored and re-readable) and builds the *http.Response
// itself, e.g. to echo back something from the request. Whichever of
// Reply/ReplyFunc is called last on a rule wins.
func (r *MockRule) ReplyFunc(fn func(*http.Request) (*http.Response, error)) *MockRule {
	r.fn = fn
	return r
}

// matches reports whether req (with its already-buffered body) matches
// r, returning any path parameters captured along the way (e.g.
// {"id": "42"} for a rule registered as "/users/{id}" against
// "/users/42"). A nil, true result means "matched, no parameters".
func (r *MockRule) matches(req *http.Request, body []byte) (map[string]string, bool) {
	if r.method != "" && r.method != req.Method {
		return nil, false
	}
	params, ok := matchPath(r.path, req.URL.Path)
	if !ok {
		return nil, false
	}
	if len(r.query) > 0 {
		actual := req.URL.Query()
		for k := range r.query {
			if actual.Get(k) != r.query.Get(k) {
				return nil, false
			}
		}
	}
	if r.header != nil {
		for k, want := range r.header {
			if req.Header.Get(k) != want[0] {
				return nil, false
			}
		}
	}
	if r.body != nil && !r.body(body) {
		return nil, false
	}
	return params, true
}

// matchPath compares pattern against actual segment by segment,
// ignoring any leading/trailing slashes on both sides (so "/users/1",
// "/users/1/" and "users/1" are all equivalent). A segment in pattern
// wrapped in braces — "{id}" — matches any single segment of actual
// and is captured under that name; an empty pattern matches any path
// with no captures.
func matchPath(pattern, actual string) (map[string]string, bool) {
	if pattern == "" {
		return nil, true
	}

	patternSegs := splitPathSegments(pattern)
	actualSegs := splitPathSegments(actual)
	if len(patternSegs) != len(actualSegs) {
		return nil, false
	}

	var params map[string]string
	for i, seg := range patternSegs {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			if params == nil {
				params = make(map[string]string, len(patternSegs))
			}
			params[seg[1:len(seg)-1]] = actualSegs[i]
			continue
		}
		if seg != actualSegs[i] {
			return nil, false
		}
	}
	return params, true
}

// splitPathSegments splits a URL path into its non-empty segments,
// so leading/trailing slashes never affect matching: "/users/1/" and
// "users/1" both become ["users", "1"], and "/" (or "") becomes nil.
func splitPathSegments(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

type pathParamsKey struct{}

func withPathParams(ctx context.Context, params map[string]string) context.Context {
	return context.WithValue(ctx, pathParamsKey{}, params)
}

// PathParams returns the path parameters a MockAdapter rule captured
// for req — e.g. {"id": "42"} for a rule registered via
// adapter.On(http.MethodGet, "/users/{id}") that matched "/users/42".
// Returns nil if the matching rule's path had no {placeholders}, or if
// req wasn't served by a MockAdapter at all.
func PathParams(req *http.Request) map[string]string {
	params, _ := req.Context().Value(pathParamsKey{}).(map[string]string)
	return params
}

// PathParam is a shorthand for PathParams(req)[name].
func PathParam(req *http.Request, name string) string {
	return PathParams(req)[name]
}

func (r *MockRule) respond(req *http.Request) (*http.Response, error) {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}

	if r.fn != nil {
		return r.fn(req)
	}

	header := r.respHeader
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:     http.StatusText(r.status),
		StatusCode: r.status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(r.respBody)),
		Request:    req,
	}, nil
}

// readRequestBody reads req.Body fully and restores it (so matches and
// any ReplyFunc can both read it), returning nil if there was none.
func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}
