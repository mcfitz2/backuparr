package backup

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
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
	// Buffer the request body so we can replay it on retries.
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
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
		if bodyBytes != nil {
			attemptReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			attemptReq.ContentLength = int64(len(bodyBytes))
		} else {
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
			// Retryable HTTP status — drain body and save for potential return
			lastResp = resp
			if resp.Body != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
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
	s := strings.ToLower(err.Error())
	retryable := []string{
		"eof",
		"connection reset",
		"connection refused",
		"broken pipe",
		"timeout",
		"deadline exceeded",
		"tls handshake",
		"temporary failure",
		"server closed",
		"transport connection broken",
	}
	for _, pattern := range retryable {
		if strings.Contains(s, pattern) {
			return true
		}
	}
	return false
}
