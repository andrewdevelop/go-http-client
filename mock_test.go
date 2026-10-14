package httpclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestMockAdapter_BasicReply(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/users/1").Reply(http.StatusOK, []byte(`{"id":1}`))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})
	resp, err := c.Get(context.Background(), "/users/1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if string(resp.Body) != `{"id":1}` {
		t.Fatalf("unexpected body: %s", resp.Body)
	}
}

func TestMockAdapter_NoMatch(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/users/1").Reply(http.StatusOK, nil)

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})
	_, err := c.Get(context.Background(), "/users/2")
	if !errors.Is(err, ErrNoMockMatch) {
		t.Fatalf("expected ErrNoMockMatch, got %v", err)
	}
}

func TestMockAdapter_QueryMatch(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/search", WithQueryParam("q", "golang")).
		Reply(http.StatusOK, []byte("golang results"))
	adapter.On(http.MethodGet, "/search").Reply(http.StatusOK, []byte("generic results"))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	resp, err := c.Do(context.Background(), NewRequest(http.MethodGet, "/search").SetQuery("q", "golang"))
	if err != nil {
		t.Fatalf("Do 1: %v", err)
	}
	if string(resp.Body) != "golang results" {
		t.Fatalf("expected query-specific rule to win, got %q", resp.Body)
	}

	resp, err = c.Do(context.Background(), NewRequest(http.MethodGet, "/search").SetQuery("q", "rust"))
	if err != nil {
		t.Fatalf("Do 2: %v", err)
	}
	if string(resp.Body) != "generic results" {
		t.Fatalf("expected fallback rule for a non-matching query, got %q", resp.Body)
	}
}

func TestMockAdapter_BodyMatch(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodPost, "/rpc", WithBodyMatch(func(b []byte) bool {
		return string(b) == `{"method":"eth_blockNumber"}`
	})).Reply(http.StatusOK, []byte(`{"result":"0x1"}`))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})
	resp, err := c.Post(context.Background(), "/rpc", []byte(`{"method":"eth_blockNumber"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if string(resp.Body) != `{"result":"0x1"}` {
		t.Fatalf("unexpected body: %s", resp.Body)
	}

	_, err = c.Post(context.Background(), "/rpc", []byte(`{"method":"other"}`))
	if !errors.Is(err, ErrNoMockMatch) {
		t.Fatalf("expected non-matching body to miss, got %v", err)
	}
}

func TestMockAdapter_Delay(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/slow").Reply(http.StatusOK, nil, WithDelay(50*time.Millisecond))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	start := time.Now()
	if _, err := c.Get(context.Background(), "/slow"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("expected WithDelay to actually wait, elapsed %v", elapsed)
	}
}

func TestMockAdapter_DelayBoundByContext(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/slow").Reply(http.StatusOK, nil, WithDelay(time.Second))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := c.Get(ctx, "/slow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline to cut the mock delay short, got %v", err)
	}
}

func TestMockAdapter_ReplyFunc(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodPost, "/echo").ReplyFunc(func(req *http.Request) (*http.Response, error) {
		body, err := readRequestBody(req)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})
	resp, err := c.Post(context.Background(), "/echo", []byte("hello"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if string(resp.Body) != "hello" {
		t.Fatalf("expected ReplyFunc to echo the request body, got %q", resp.Body)
	}
}

func TestMockAdapter_CallsAndRetryIntegration(t *testing.T) {
	adapter := NewMockAdapter()
	attempts := 0
	rule := adapter.On(http.MethodGet, "/flaky").ReplyFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		status := http.StatusServiceUnavailable
		if attempts >= 3 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Body:       http.NoBody,
			Header:     http.Header{},
			Request:    req,
		}, nil
	})

	c := NewClient(Config{
		BaseURL:   "https://api.example.com",
		Transport: adapter,
		Retry:     RetryConfig{Enabled: true, MaxAttempts: 5, BaseDelay: time.Millisecond},
	})

	resp, err := c.Get(context.Background(), "/flaky")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := adapter.Calls(rule); got != 3 {
		t.Fatalf("expected the rule to have matched 3 times, got %d", got)
	}
}

func TestMockAdapter_PathPlaceholder(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/users/{id}").ReplyFunc(func(req *http.Request) (*http.Response, error) {
		id := PathParam(req, "id")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(id))),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	resp, err := c.Get(context.Background(), "/users/42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Body) != "42" {
		t.Fatalf("expected captured {id}=42, got %q", resp.Body)
	}
}

func TestMockAdapter_PathPlaceholderMultiSegment(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/orgs/{org}/repos/{repo}").ReplyFunc(func(req *http.Request) (*http.Response, error) {
		params := PathParams(req)
		body := params["org"] + "/" + params["repo"]
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	resp, err := c.Get(context.Background(), "/orgs/golang/repos/go")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Body) != "golang/go" {
		t.Fatalf("expected captured org/repo, got %q", resp.Body)
	}
}

func TestMockAdapter_PathPlaceholderDoesNotCrossSegments(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/users/{id}").Reply(http.StatusOK, []byte("matched"))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	// A {placeholder} matches exactly one segment, not "the rest of the path".
	_, err := c.Get(context.Background(), "/users/42/orders")
	if !errors.Is(err, ErrNoMockMatch) {
		t.Fatalf("expected a deeper path to miss a single-segment placeholder, got %v", err)
	}
}

func TestMockAdapter_TrailingSlashIgnored(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/users/1").Reply(http.StatusOK, []byte("ok"))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	// Rule registered without a trailing slash; request has one.
	resp, err := c.Get(context.Background(), "/users/1/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Body) != "ok" {
		t.Fatalf("unexpected body: %q", resp.Body)
	}
}

func TestMockAdapter_TrailingSlashIgnoredOnRulePath(t *testing.T) {
	adapter := NewMockAdapter()
	adapter.On(http.MethodGet, "/users/1/").Reply(http.StatusOK, []byte("ok"))

	c := NewClient(Config{BaseURL: "https://api.example.com", Transport: adapter})

	// Rule registered with a trailing slash; request has none.
	resp, err := c.Get(context.Background(), "/users/1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Body) != "ok" {
		t.Fatalf("unexpected body: %q", resp.Body)
	}
}
