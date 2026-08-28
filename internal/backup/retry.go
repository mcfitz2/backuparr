package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// RetryTransport wraps an http.RoundTripper with automatic retry logic
// for transient failures (connection resets, EOF, timeouts, 5xx responses).
// It buffers request bodies so they can be replayed on retries.
type RetryTransport struct {
	// Base is the underlying transport to use. If nil, http.DefaultTransport is used.
	Base http.RoundTripper
	// MaxRetries is the maximum number of retry attempts after the initial request.
	MaxRetries int
	// BaseDelay is the initial delay between retries; it doubles on each attempt.
	BaseDelay time.Duration
}

// maxBufferedErrorBody caps how much of a retryable error response body is
// buffered for return to the caller after retries are exhausted. This bounds
// memory use if a server sends an unexpectedly large error page; anything
// beyond this limit is discarded.
const maxBufferedErrorBody = 64 * 1024 // 64KB

// maxBufferedRequestBody caps how much of a request body RetryTransport will
// buffer itself in order to replay it on retry. Requests larger than this
// with no caller-supplied GetBody are sent once, unretried, instead of being
// held entirely in memory.
const maxBufferedRequestBody = 1 << 20 // 1MB

// NewRetryTransport creates a RetryTransport with sensible defaults:
// 3 retries with 2s base delay (2s, 4s, 8s backoff).
// If base is nil, http.DefaultTransport is used.
func NewRetryTransport(base http.RoundTripper) *RetryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &RetryTransport{
		Base:       base,
		MaxRetries: 3,
		BaseDelay:  2 * time.Second,
	}
}

// RoundTrip executes the request with automatic retries on transient failures.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A request can only be retried if resending it is safe: GET/HEAD/PUT/DELETE
	// are idempotent by convention, and anything else needs the caller to have
	// opted in via GetBody. Otherwise a single attempt is made and its result
	// (success or failure) is returned as-is — retrying could duplicate a real
	// side effect, e.g. a restore upload or a restart command.
	getBody := req.GetBody
	if !isReplayableMethod(req.Method) && getBody == nil {
		return t.Base.RoundTrip(req)
	}

	var bodyBytes []byte
	if getBody == nil && req.Body != nil && req.Body != http.NoBody {
		buf, err := io.ReadAll(io.LimitReader(req.Body, maxBufferedRequestBody+1))
		if err != nil {
			req.Body.Close()
			return nil, err
		}
		if len(buf) > maxBufferedRequestBody {
			// Too large to hold in memory for replay; reassemble the body
			// from what's already been read and what's left, then send it
			// once, unretried.
			req.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), req.Body), req.Body}
			return t.Base.RoundTrip(req)
		}
		req.Body.Close()
		bodyBytes = buf
	}

	maxRetries := t.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	baseDelay := t.BaseDelay
	if baseDelay <= 0 {
		baseDelay = 2 * time.Second
	}

	var lastErr error
	var lastResp *http.Response

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Check if the caller's context is already done
		if err := req.Context().Err(); err != nil {
			break
		}

		// Clone the request for each attempt to avoid modifying the original
		attemptReq := req.Clone(req.Context())
		switch {
		case getBody != nil:
			rc, err := getBody()
			if err != nil {
				return nil, err
			}
			attemptReq.Body = rc
		case bodyBytes != nil:
			attemptReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			attemptReq.ContentLength = int64(len(bodyBytes))
		default:
			attemptReq.Body = http.NoBody
			attemptReq.ContentLength = 0
		}

		resp, err := t.Base.RoundTrip(attemptReq)

		// Success — return immediately
		if err == nil && !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}

		// If the context is done after the attempt, return whatever we got
		if req.Context().Err() != nil {
			if err != nil {
				return nil, err
			}
			return resp, nil
		}

		// Check if the error is retryable
		if err != nil {
			lastErr = err
			if !isRetryableError(err) {
				return nil, err
			}
		} else {
			// Retryable HTTP status — drain the body (bounded), then restore
			// it as a fresh reader so the response stays usable if we end up
			// returning it after exhausting retries.
			if resp.Body != nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBufferedErrorBody))
				resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(body))
			}
			lastResp = resp
		}

		// Log and wait before retrying
		if attempt < maxRetries {
			delay := baseDelay * time.Duration(1<<uint(attempt))
			if lastErr != nil {
				log.Printf("[retry] Attempt %d/%d failed: %v (retrying in %v)", attempt+1, maxRetries+1, lastErr, delay)
			} else if lastResp != nil {
				log.Printf("[retry] Attempt %d/%d got HTTP %d (retrying in %v)", attempt+1, maxRetries+1, lastResp.StatusCode, delay)
			}

			select {
			case <-req.Context().Done():
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, req.Context().Err()
			case <-time.After(delay):
			}
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	if lastResp != nil {
		return lastResp, nil
	}
	return nil, req.Context().Err()
}

// isReplayableMethod returns true for HTTP methods that are safe to retry
// without a caller-supplied GetBody, because resending them is idempotent
// by convention.
func isReplayableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// isRetryableStatus returns true for HTTP status codes that indicate a transient server error.
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,     // 504
		http.StatusTooManyRequests:    // 429
		return true
	}
	return false
}

// isRetryableError returns true for errors that indicate a transient network failure.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// net.Error covers dial/read/write timeouts and TLS handshake timeouts
	// (all of which implement Timeout() == true), plus transient DNS
	// resolution failures, which only expose Temporary() == true.
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	// http.Transport's "server closed idle connection" error has no
	// exported sentinel or type to compare against.
	return strings.Contains(err.Error(), "server closed idle connection")
}
