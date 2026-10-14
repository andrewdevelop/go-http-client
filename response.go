package httpclient

import (
	"fmt"
	"net/http"
)

// Response is the result of a successful (2xx) request. Non-2xx
// responses are reported as a *StatusError instead; see StatusError.Response
// to get at the body of a failed call.
//
// This package does no body parsing of its own: Body is always the raw
// bytes read off the wire. Decode it however fits the call — JSON,
// protobuf, msgpack — with whatever library you'd use anyway.
type Response struct {
	StatusCode int
	Status     string
	Headers    http.Header
	Body       []byte
	Request    *Request
}

// StatusError is returned by Client.Do (wrapped by ErrRetriesExhausted if
// retries were exhausted) when the server responds with a non-2xx status.
type StatusError struct {
	StatusCode int
	Status     string
	Body       []byte

	// Response is the full response that produced this error, so
	// callers that need headers or want to parse the body don't have to
	// go through StatusError.Body a second time.
	Response *Response
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpclient: unexpected status %d (%s)", e.StatusCode, e.Status)
}
