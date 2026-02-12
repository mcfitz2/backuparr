package sidecar

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestName(t *testing.T) {
	c, err := NewClient("http://localhost:8484", "", "transmission")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.Name(); got != "transmission" {
		t.Errorf("Name() = %q, want %q", got, "transmission")
	}
}

func TestNewClient_Validation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		apiKey  string
		appName string
		wantErr bool
	}{
		{"valid", "http://localhost:8484", "", "transmission", false},
		{"valid with key", "http://localhost:8484", "secret", "nzbget", false},
		{"missing url", "", "", "transmission", true},
		{"missing name", "http://localhost:8484", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.url, tt.apiKey, tt.appName)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBackup_Success(t *testing.T) {
	// Create a minimal valid ZIP to serve
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("test.txt")
	w.Write([]byte("hello"))
	zw.Close()
	zipData := zipBuf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/backup" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipData)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "testapp")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	defer reader.Close()

	if result.Size != int64(len(zipData)) {
		t.Errorf("Size = %d, want %d", result.Size, len(zipData))
	}
	if result.Name == "" {
		t.Error("Name should not be empty")
	}

	// Verify we can read the data back
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(data, zipData) {
		t.Error("backup data mismatch")
	}
}

func TestBackup_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"disk full"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "", "testapp")
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestBackup_APIKey(t *testing.T) {
	var receivedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedKey = r.Header.Get("X-Api-Key")
		// Return a minimal valid ZIP
		var zipBuf bytes.Buffer
		zw := zip.NewWriter(&zipBuf)
		zw.Close()
		w.Write(zipBuf.Bytes())
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "my-secret", "testapp")
	c.Backup(context.Background())

	if receivedKey != "my-secret" {
		t.Errorf("API key = %q, want %q", receivedKey, "my-secret")
	}
}

func TestRestore_Success(t *testing.T) {
	var receivedContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/restore" {
			http.NotFound(w, r)
			return
		}
		receivedContentType = r.Header.Get("Content-Type")

		// Verify we received a multipart form with "backup" field
		file, _, err := r.FormFile("backup")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": err.Error()})
			return
		}
		defer file.Close()

		data, _ := io.ReadAll(file)
		if len(data) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "empty file"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success":       true,
			"filesRestored": 5,
			"bytesRestored": len(data),
			"restart":       map[string]any{"attempted": false},
			"message":       "Restored 5 files",
		})
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "", "testapp")

	// Create test ZIP data
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("config.xml")
	w.Write([]byte("<config/>"))
	zw.Close()

	err := c.Restore(context.Background(), &zipBuf)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Check that multipart content type was sent
	if receivedContentType == "" {
		t.Error("Content-Type header was empty")
	}
}

func TestRestore_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "disk full"})
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "", "testapp")
	err := c.Restore(context.Background(), bytes.NewReader([]byte("fake-zip")))
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
