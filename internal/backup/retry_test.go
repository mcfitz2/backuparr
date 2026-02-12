package backup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockTransport is a test RoundTripper that returns configurable responses.
type mockTransport struct {
	responses []mockResponse
	calls     atomic.Int32
}

type mockResponse struct {
	status int
	body   string
	err    error
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	idx := int(m.calls.Add(1)) - 1
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}
	r := m.responses[idx]
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     http.Header{},
	}, nil
}

func TestRetryTransport_SuccessNoRetry(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_RetriesOnEOF(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_RetriesOnTimeout(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_RetriesOnConnectionReset(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: connection reset by peer")},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_RetriesOn502(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{status: 502, body: "bad gateway"},
			{status: 503, body: "service unavailable"},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_NoRetryOn4xx(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{status: 401, body: "unauthorized"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected 1 call (no retries for 4xx), got %d", mock.calls.Load())
	}
}

func TestRetryTransport_NoRetryOnNonTransientError(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("unknown scheme: ftp")},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown scheme") {
		t.Errorf("expected 'unknown scheme' error, got: %v", err)
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected 1 call (no retries for non-transient), got %d", mock.calls.Load())
	}
}

func TestRetryTransport_ExhaustsRetries(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "EOF") {
		t.Errorf("expected EOF error, got: %v", err)
	}
	// 1 initial + 3 retries = 4 calls
	if mock.calls.Load() != 4 {
		t.Errorf("expected 4 calls, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_RespectsContextCancellation(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 10, BaseDelay: 100 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://example.com", nil)
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error due to context cancellation")
	}

	// Should have made fewer calls than MaxRetries+1 due to context cancellation
	calls := mock.calls.Load()
	if calls > 3 {
		t.Errorf("expected fewer calls due to context cancellation, got %d", calls)
	}
}

func TestRetryTransport_PreservesRequestBody(t *testing.T) {
	var bodies []string
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("connection reset by peer")},
			{status: 200, body: "ok"},
		},
	}
	// Wrap mock to capture bodies
	captureMock := &bodyCapturingTransport{inner: mock, bodies: &bodies}

	rt := &RetryTransport{Base: captureMock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	body := `{"command":"Backup"}`
	req, _ := http.NewRequest("POST", "http://example.com", strings.NewReader(body))
	req.ContentLength = int64(len(body))

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	for i, b := range bodies {
		if b != body {
			t.Errorf("attempt %d: expected body %q, got %q", i+1, body, b)
		}
	}
}

func TestRetryTransport_RetriesOn429(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{status: 429, body: "too many requests"},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if mock.calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_ExponentialBackoff(t *testing.T) {
	var timestamps []time.Time
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
			{err: fmt.Errorf("unexpected EOF")},
			{status: 200, body: "ok"},
		},
	}
	// Wrap to capture timestamps
	timestampMock := &timestampTransport{inner: mock, timestamps: &timestamps}

	rt := &RetryTransport{Base: timestampMock, MaxRetries: 3, BaseDelay: 50 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if len(timestamps) != 4 {
		t.Fatalf("expected 4 timestamps, got %d", len(timestamps))
	}

	// Verify exponential backoff: delays should roughly be 50ms, 100ms, 200ms
	for i := 1; i < len(timestamps); i++ {
		gap := timestamps[i].Sub(timestamps[i-1])
		expectedMin := 50 * time.Millisecond * time.Duration(1<<uint(i-1)) / 2 // Allow 50% tolerance
		if gap < expectedMin {
			t.Errorf("gap between attempt %d and %d too short: %v (expected at least ~%v)",
				i, i+1, gap, expectedMin)
		}
	}
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		err       error
		retryable bool
	}{
		{fmt.Errorf("unexpected EOF"), true},
		{fmt.Errorf("read tcp: connection reset by peer"), true},
		{fmt.Errorf("dial tcp: connection refused"), true},
		{fmt.Errorf("write tcp: broken pipe"), true},
		{fmt.Errorf("context deadline exceeded"), true},
		{fmt.Errorf("TLS handshake timeout"), true},
		{fmt.Errorf("net/http: request canceled while waiting for connection (Client.Timeout exceeded)"), true},
		{fmt.Errorf("http: server closed idle connection"), true},
		{fmt.Errorf("unknown scheme: ftp"), false},
		{fmt.Errorf("invalid URL"), false},
		{nil, false},
	}
	for _, tt := range tests {
		got := isRetryableError(tt.err)
		errStr := "<nil>"
		if tt.err != nil {
			errStr = tt.err.Error()
		}
		if got != tt.retryable {
			t.Errorf("isRetryableError(%q) = %v, want %v", errStr, got, tt.retryable)
		}
	}
}

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		status    int
		retryable bool
	}{
		{200, false},
		{301, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{500, false},
		{429, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, tt := range tests {
		got := isRetryableStatus(tt.status)
		if got != tt.retryable {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", tt.status, got, tt.retryable)
		}
	}
}

// bodyCapturingTransport captures request bodies and delegates to inner transport.
type bodyCapturingTransport struct {
	inner  http.RoundTripper
	bodies *[]string
}

func (t *bodyCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		*t.bodies = append(*t.bodies, string(body))
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	return t.inner.RoundTrip(req)
}

// timestampTransport records when each RoundTrip is called.
type timestampTransport struct {
	inner      http.RoundTripper
	timestamps *[]time.Time
}

func (t *timestampTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*t.timestamps = append(*t.timestamps, time.Now())
	return t.inner.RoundTrip(req)
}
