package truenas

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWSURL(t *testing.T) {
	tests := []struct {
		baseURL string
		want    string
	}{
		{"http://192.168.1.136", "ws://192.168.1.136/api/current"},
		{"https://truenas.local", "wss://truenas.local/api/current"},
		{"http://host:8080", "ws://host:8080/api/current"},
		{"http://host:8080/", "ws://host:8080/api/current"},
	}
	for _, tt := range tests {
		c := NewClient(tt.baseURL, "test-key")
		got := c.wsURL()
		if got != tt.want {
			t.Errorf("NewClient(%q).wsURL() = %q, want %q", tt.baseURL, got, tt.want)
		}
	}
}

func TestName(t *testing.T) {
	c := NewClient("http://localhost", "key")
	if c.Name() != "truenas" {
		t.Errorf("Name() = %q, want %q", c.Name(), "truenas")
	}
}

func TestRestore(t *testing.T) {
	restoreData := []byte("mock-truenas-config-restore-data")
	mock := newMockTrueNAS("valid-api-key", nil)
	defer mock.close()

	c := NewClient(mock.server.URL, "valid-api-key")
	err := c.Restore(context.Background(), bytes.NewReader(restoreData))
	if err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	// Verify the mock received the upload
	if mock.uploadedData == nil {
		t.Fatal("mock did not receive uploaded file")
	}
	if !bytes.Equal(mock.uploadedData, restoreData) {
		t.Errorf("uploaded data = %q, want %q", mock.uploadedData, restoreData)
	}
}

func TestRestore_AuthFailure(t *testing.T) {
	mock := newMockTrueNAS("valid-api-key", nil)
	defer mock.close()

	c := NewClient(mock.server.URL, "wrong-key")
	err := c.Restore(context.Background(), strings.NewReader("data"))
	if err == nil {
		t.Fatal("Restore() should fail with wrong API key")
	}
	// The upload itself should succeed (uses Bearer auth on HTTP),
	// but the WebSocket job_wait auth should fail.
	if !strings.Contains(err.Error(), "API key was rejected") {
		t.Errorf("error = %q, want mention of API key rejection", err.Error())
	}
}

func TestRestore_JobFailure(t *testing.T) {
	mock := newMockTrueNAS("valid-api-key", nil)
	mock.uploadJobFail = true
	defer mock.close()

	c := NewClient(mock.server.URL, "valid-api-key")
	err := c.Restore(context.Background(), strings.NewReader("data"))
	if err == nil {
		t.Fatal("Restore() should fail when job fails")
	}
	if !strings.Contains(err.Error(), "config.upload job") {
		t.Errorf("error = %q, want mention of job failure", err.Error())
	}
}

type mockTrueNAS struct {
	server        *httptest.Server
	apiKey        string
	upgrader      websocket.Upgrader
	backupData    []byte
	uploadedData  []byte // captured from /_upload/
	uploadJobFail bool   // if true, core.get_jobs returns FAILED state
}

func newMockTrueNAS(apiKey string, backupData []byte) *mockTrueNAS {
	m := &mockTrueNAS{
		apiKey:     apiKey,
		backupData: backupData,
		upgrader:   websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", m.handleWebSocket)
	mux.HandleFunc("/_download/", m.handleDownload)
	mux.HandleFunc("/_upload/", m.handleUpload)
	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockTrueNAS) close() { m.server.Close() }

func (m *mockTrueNAS) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return
		}

		switch req.Method {
		case "auth.login_with_api_key":
			key, _ := req.Params[0].(string)
			result := key == m.apiKey
			conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  result,
			})
		case "core.download":
			downloadPath := "/_download/42?auth_token=test-token"
			conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  []any{int64(42), downloadPath},
			})
		case "core.get_jobs":
			state := "SUCCESS"
			errMsg := ""
			if m.uploadJobFail {
				state = "FAILED"
				errMsg = "config.upload failed"
			}
			conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": []map[string]any{{
					"id":    77,
					"state": state,
					"error": errMsg,
					"progress": map[string]any{
						"percent":     100,
						"description": "done",
					},
				}},
			})
		default:
			conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]any{
					"code":    -32601,
					"message": "method not found",
				},
			})
		}
	}
}

func (m *mockTrueNAS) handleDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(m.backupData)
}

func (m *mockTrueNAS) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the multipart form
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Read the uploaded file
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()

	data, _ := io.ReadAll(f)
	m.uploadedData = data

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"job_id": 77})
}

func TestBackup(t *testing.T) {
	backupData := []byte("mock-truenas-config-backup-data")
	mock := newMockTrueNAS("valid-api-key", backupData)
	defer mock.close()

	c := NewClient(mock.server.URL, "valid-api-key")
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()

	if result.Name != "truenas-config.tar" {
		t.Errorf("result.Name = %q, want %q", result.Name, "truenas-config.tar")
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(backupData) {
		t.Errorf("backup data = %q, want %q", got, backupData)
	}
}

func TestBackup_AuthFailure(t *testing.T) {
	mock := newMockTrueNAS("valid-api-key", nil)
	defer mock.close()

	c := NewClient(mock.server.URL, "wrong-key")
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail with wrong API key")
	}
	if !strings.Contains(err.Error(), "API key was rejected") {
		t.Errorf("error = %q, want mention of API key rejection", err.Error())
	}
}

func TestBackup_RPCError(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req jsonRPCRequest
			json.Unmarshal(raw, &req)
			switch req.Method {
			case "auth.login_with_api_key":
				conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": true})
			case "core.download":
				conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]any{"code": -32001, "message": "method call error"},
				})
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail on RPC error")
	}
	if !strings.Contains(err.Error(), "RPC error") {
		t.Errorf("error = %q, want mention of RPC error", err.Error())
	}
}

func TestBackup_WithNotifications(t *testing.T) {
	backupData := []byte("backup-with-notifications")
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req jsonRPCRequest
			json.Unmarshal(raw, &req)
			switch req.Method {
			case "auth.login_with_api_key":
				conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": true})
			case "core.download":
				// Send a notification first (no ID field)
				conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0",
					"method":  "collection_update",
					"params":  map[string]any{"collection": "core.get_jobs", "msg": "changed"},
				})
				// Then send the actual response
				conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  []any{int64(99), "/_download/99?auth_token=tok"},
				})
			}
		}
	})
	mux.HandleFunc("/_download/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(backupData)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()

	if result.Name != "truenas-config.tar" {
		t.Errorf("result.Name = %q, want %q", result.Name, "truenas-config.tar")
	}
	got, _ := io.ReadAll(reader)
	if string(got) != string(backupData) {
		t.Errorf("backup data = %q, want %q", got, backupData)
	}
}
