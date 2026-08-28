package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeNetError simulates errors returned by the real net/http transport
// (e.g. *net.OpError, the unexported httpError behind "TLS handshake
// timeout", *net.DNSError) that carry Timeout()/Temporary() but no
// exported sentinel to compare against with errors.Is.
type fakeNetError struct {
	msg       string
	timeout   bool
	temporary bool
}

func (e *fakeNetError) Error() string   { return e.msg }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return e.temporary }

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
			{err: fmt.Errorf("read tcp 10.0.0.1:80: %w", io.ErrUnexpectedEOF)},
			{err: fmt.Errorf("read tcp 10.0.0.1:80: %w", io.ErrUnexpectedEOF)},
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
			{err: &fakeNetError{msg: "context deadline exceeded (Client.Timeout exceeded while awaiting headers)", timeout: true}},
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
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
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
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
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

// TestRetryTransport_PreservesRequestBody covers a POST whose context was
// explicitly marked via WithReplayableRequest. The body is built with
// strings.NewReader (so GetBody is also auto-populated), but it's the
// explicit context opt-in — not GetBody alone — that authorizes retrying
// this non-idempotent method.
func TestRetryTransport_PreservesRequestBody(t *testing.T) {
	var bodies []string
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{status: 200, body: "ok"},
		},
	}
	// Wrap mock to capture bodies
	captureMock := &bodyCapturingTransport{inner: mock, bodies: &bodies}

	rt := &RetryTransport{Base: captureMock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	body := `{"command":"Backup"}`
	ctx := WithReplayableRequest(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://example.com", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	if req.GetBody == nil {
		t.Fatal("expected http.NewRequest to auto-populate GetBody for a strings.Reader body")
	}

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
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
			{err: fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF)},
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
		name      string
		err       error
		retryable bool
	}{
		{"bare io.EOF", io.EOF, true},
		{"wrapped unexpected EOF", fmt.Errorf("read tcp: %w", io.ErrUnexpectedEOF), true},
		{"wrapped connection reset", fmt.Errorf("read tcp: %w", syscall.ECONNRESET), true},
		{"wrapped connection refused", fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), true},
		{"wrapped broken pipe", fmt.Errorf("write tcp: %w", syscall.EPIPE), true},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped context deadline exceeded", fmt.Errorf("request failed: %w", context.DeadlineExceeded), true},
		{"net.Error timeout (e.g. TLS handshake timeout)", &fakeNetError{msg: "net/http: TLS handshake timeout", timeout: true}, true},
		{"net.Error temporary (e.g. DNS failure)", &fakeNetError{msg: "lookup host: temporary failure in name resolution", temporary: true}, true},
		{"server closed idle connection", errors.New("http: server closed idle connection"), true},
		// These used to be misclassified by naive substring matching on
		// err.Error(); they are plain errors carrying none of the
		// structural signals above, so they must not be retried.
		{"unrelated error containing 'eof' substring", fmt.Errorf("payload contained invalid geofence data"), false},
		{"unrelated error containing 'timeout' substring", errors.New("request timeout budget exceeded in config"), false},
		{"unknown scheme", fmt.Errorf("unknown scheme: ftp"), false},
		{"invalid URL", fmt.Errorf("invalid URL"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.retryable {
				t.Errorf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.retryable)
			}
		})
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

func TestRetryTransport_ExhaustedRetryBodyReadable(t *testing.T) {
	wantBody := "service unavailable, try again later"
	mock := &mockTransport{
		responses: []mockResponse{
			{status: 503, body: wantBody},
			{status: 503, body: wantBody},
			{status: 503, body: wantBody},
			{status: 503, body: wantBody},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Errorf("expected status 503, got %d", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("expected body %q, got %q", wantBody, string(got))
	}
}

func TestRetryTransport_ExhaustedRetryBodyTruncated(t *testing.T) {
	longBody := strings.Repeat("x", maxBufferedErrorBody+1024)
	mock := &mockTransport{
		responses: []mockResponse{
			{status: 503, body: longBody},
			{status: 503, body: longBody},
			{status: 503, body: longBody},
			{status: 503, body: longBody},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if len(got) != maxBufferedErrorBody {
		t.Errorf("expected buffered body to be truncated to %d bytes, got %d", maxBufferedErrorBody, len(got))
	}
}

func TestRetryTransport_PostNotRetriedByDefault(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	body := `{"name":"Backup"}`
	// io.NopCloser hides the concrete *strings.Reader type from
	// http.NewRequest so it does NOT auto-populate GetBody, simulating a
	// caller that hasn't opted into replay.
	req, _ := http.NewRequest(http.MethodPost, "http://example.com", io.NopCloser(strings.NewReader(body)))
	if req.GetBody != nil {
		t.Fatal("test setup invalid: expected GetBody to be nil")
	}

	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected the single attempt's error to be returned, got nil")
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected exactly 1 attempt for a POST without GetBody, got %d", mock.calls.Load())
	}
}

// TestRetryTransport_AutoGetBodyAloneDoesNotAuthorizeReplay builds a POST
// body exactly the way internal/sidecar/client.go's Restore does: a
// multipart form written into a bytes.Buffer, then passed by address to
// http.NewRequestWithContext. That makes http.NewRequest auto-populate
// req.GetBody for the *bytes.Buffer body — but since the request's context
// was never marked with WithReplayableRequest, that auto-populated GetBody
// must NOT be treated as caller opt-in. The request must get exactly one
// attempt.
func TestRetryTransport_AutoGetBodyAloneDoesNotAuthorizeReplay(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("backup", "backup.zip")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := part.Write([]byte("fake zip contents")); err != nil {
		t.Fatalf("failed to write form data: %v", err)
	}
	writer.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com/api/v1/restore", &body)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if req.GetBody == nil {
		t.Fatal("test setup invalid: expected http.NewRequest to auto-populate GetBody for a *bytes.Buffer body")
	}

	_, err = rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected the single attempt's error to be returned, got nil")
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected exactly 1 attempt for a POST with only an auto-populated GetBody (no explicit opt-in), got %d", mock.calls.Load())
	}
}

// TestRetryTransport_ExplicitOptInAuthorizesReplay mirrors the test above,
// except the context is marked via WithReplayableRequest, which is the only
// thing that should authorize retrying a POST.
func TestRetryTransport_ExplicitOptInAuthorizesReplay(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	body := `{"name":"Backup"}`
	ctx := WithReplayableRequest(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.com", strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	if req.GetBody == nil {
		t.Fatal("test setup invalid: expected GetBody to be set")
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if mock.calls.Load() != 3 {
		t.Errorf("expected 3 attempts for a POST with explicit WithReplayableRequest opt-in, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_GetStillRetriesUpToMaxRetries(t *testing.T) {
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{status: 200, body: "ok"},
		},
	}

	rt := &RetryTransport{Base: mock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if mock.calls.Load() != 4 {
		t.Errorf("expected 1 initial + 3 retries = 4 attempts for GET, got %d", mock.calls.Load())
	}
}

func TestRetryTransport_OversizedBodyNotBufferedForRetry(t *testing.T) {
	var bodies []string
	mock := &mockTransport{
		responses: []mockResponse{
			{err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET)},
			{status: 200, body: "ok"},
		},
	}
	captureMock := &bodyCapturingTransport{inner: mock, bodies: &bodies}

	rt := &RetryTransport{Base: captureMock, MaxRetries: 3, BaseDelay: 10 * time.Millisecond}
	bigBody := strings.Repeat("x", maxBufferedRequestBody+1024)
	// PUT is a safe method, but the body exceeds maxBufferedRequestBody and
	// carries no GetBody (io.NopCloser hides the reader's concrete type),
	// so it must fall through to a single non-retried attempt instead of
	// being read entirely into memory.
	req, _ := http.NewRequest(http.MethodPut, "http://example.com", io.NopCloser(strings.NewReader(bigBody)))

	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected the single attempt's error to be returned, got nil")
	}
	if mock.calls.Load() != 1 {
		t.Errorf("expected exactly 1 attempt for an oversized body, got %d", mock.calls.Load())
	}
	if len(bodies) != 1 {
		t.Fatalf("expected the transport to see exactly 1 request body, got %d", len(bodies))
	}
	if len(bodies[0]) != len(bigBody) {
		t.Errorf("expected the full oversized body (%d bytes) to reach the transport intact, got %d bytes", len(bigBody), len(bodies[0]))
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
