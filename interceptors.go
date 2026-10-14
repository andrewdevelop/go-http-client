package httpclient

import (
	"context"
	"sort"
	"sync"
)

// RequestInterceptor runs before a request is sent, and may rewrite it
// (add headers, sign it, log it) or reject it outright by returning an
// error. Interceptors run in registration order.
type RequestInterceptor func(ctx context.Context, req *Request) (*Request, error)

// ResponseInterceptor runs after a response is received (including
// non-2xx ones, since those still carry a *Response alongside the
// *StatusError in the Do/Get/etc. return), and may rewrite it or turn it
// into an error. Interceptors run in registration order.
type ResponseInterceptor func(ctx context.Context, resp *Response) (*Response, error)

// ErrorInterceptor runs when a request ultimately fails — after retries,
// the circuit breaker and any ResponseInterceptor are done with it — and
// may translate, wrap, or suppress the error. Interceptors run in
// registration order; the error passed to the next one is whatever the
// previous one returned.
type ErrorInterceptor func(ctx context.Context, err error) error

// interceptorChain holds one kind of interceptor keyed by an
// ever-increasing id, mirroring axios's interceptors.use()/eject() —
// Use returns an id that Eject later removes, without disturbing the
// registration order of the interceptors that remain.
type interceptorChain[T any] struct {
	mu      sync.RWMutex
	nextID  int
	entries map[int]T
}

func newInterceptorChain[T any]() *interceptorChain[T] {
	return &interceptorChain[T]{entries: make(map[int]T)}
}

func (c *interceptorChain[T]) Use(interceptor T) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	c.entries[id] = interceptor
	return id
}

func (c *interceptorChain[T]) Eject(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

// ordered returns the registered interceptors sorted by id (registration
// order), snapshotting under the lock so callers can iterate without
// holding it.
func (c *interceptorChain[T]) ordered() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]int, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	out := make([]T, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.entries[id])
	}
	return out
}
