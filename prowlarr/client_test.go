package prowlarr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type mockProwlarr struct {
	server            *httptest.Server
	backupCommandFail bool
	commandStatusFail bool
	noBackups         bool
	downloadFail      bool
	restoreFail       bool
	authMethod        string
}

func newMockProwlarr() *mockProwlarr {
	m := &mockProwlarr{authMethod: "none"}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/command", m.handlePostCommand)
	mux.HandleFunc("GET /api/v1/command/{id}", m.handleGetCommand)
	mux.HandleFunc("GET /api/v1/system/backup", m.handleListBackups)
	mux.HandleFunc("GET /api/v1/system/backup/download/", m.handleDownloadBackup)
	mux.HandleFunc("GET /api/v1/config/host", m.handleConfigHost)
	mux.HandleFunc("POST /api/v1/system/backup/restore/upload", m.handleRestore)
	mux.HandleFunc("POST /api/v1/system/restart", m.handleRestart)

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockProwlarr) close() { m.server.Close() }

func (m *mockProwlarr) handlePostCommand(w http.ResponseWriter, r *http.Request) {
	if m.backupCommandFail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"command failed"}`)
		return
	}
	id := int32(1)
	name := "Backup"
	status := Queued
	resp := CommandResource{Id: &id, Name: &name, Status: &status}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockProwlarr) handleGetCommand(w http.ResponseWriter, r *http.Request) {
	var status CommandStatus
	if m.commandStatusFail {
		status = Failed
	} else {
		status = Completed
	}
	id := int32(1)
	name := "Backup"
	msg := "done"
	if m.commandStatusFail {
		msg = "Backup command failed"
	}
	resp := CommandResource{Id: &id, Name: &name, Status: &status, Message: &msg}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockProwlarr) handleListBackups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if m.noBackups {
		json.NewEncoder(w).Encode([]BackupResource{})
		return
	}
	id := int32(1)
	name := "prowlarr_backup_2025-02-09.zip"
	path := "/api/v1/system/backup/download/1"
	size := int64(1024)
	ts := time.Date(2025, 2, 9, 12, 0, 0, 0, time.UTC)
	json.NewEncoder(w).Encode([]BackupResource{{Id: &id, Name: &name, Path: &path, Size: &size, Time: &ts}})
}

func (m *mockProwlarr) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if m.downloadFail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "download failed")
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, _ := zw.Create("config.xml")
	fw.Write([]byte("<Config><ApiKey>test123</ApiKey></Config>"))
	zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Write(buf.Bytes())
}

func (m *mockProwlarr) handleConfigHost(w http.ResponseWriter, r *http.Request) {
	authType := AuthenticationType(m.authMethod)
	config := HostConfigResource{AuthenticationMethod: &authType}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

func (m *mockProwlarr) handleRestore(w http.ResponseWriter, r *http.Request) {
	if m.restoreFail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"restore failed"}`)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "expected multipart/form-data")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "parse form: %v", err)
		return
	}
	file, _, err := r.FormFile("restore")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "missing restore file: %v", err)
		return
	}
	file.Close()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"RestartRequired": false})
}

func (m *mockProwlarr) handleRestart(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func makeZip() []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, _ := zw.Create("config.xml")
	fw.Write([]byte("<Config><ApiKey>test123</ApiKey></Config>"))
	zw.Close()
	return buf.Bytes()
}

func TestName(t *testing.T) {
	mock := newMockProwlarr()
	defer mock.close()
	client, err := NewProwlarrClient(mock.server.URL, "test-api-key", "", "")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if got := client.Name(); got != "prowlarr" {
		t.Errorf("Name() = %q, want %q", got, "prowlarr")
	}
}

func TestBackup(t *testing.T) {
	mock := newMockProwlarr()
	defer mock.close()
	client, err := NewProwlarrClient(mock.server.URL, "test-api-key", "", "")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	result, reader, err := client.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	defer reader.Close()
	if result.Name == "" {
		t.Error("result.Name is empty")
	}
	if result.Path == "" {
		t.Error("result.Path is empty")
	}
	if result.Size == 0 {
		t.Error("result.Size is 0")
	}
	data, _ := io.ReadAll(reader)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a valid ZIP: %v", err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "config.xml" {
			found = true
		}
	}
	if !found {
		t.Error("ZIP missing config.xml")
	}
}

func TestBackup_CommandFail(t *testing.T) {
	mock := newMockProwlarr()
	mock.backupCommandFail = true
	defer mock.close()
	client, _ := NewProwlarrClient(mock.server.URL, "k", "", "")
	_, _, err := client.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "backup command failed") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestBackup_CommandStatusFail(t *testing.T) {
	mock := newMockProwlarr()
	mock.commandStatusFail = true
	defer mock.close()
	client, _ := NewProwlarrClient(mock.server.URL, "k", "", "")
	_, _, err := client.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "command failed") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestBackup_NoBackups(t *testing.T) {
	mock := newMockProwlarr()
	mock.noBackups = true
	defer mock.close()
	client, _ := NewProwlarrClient(mock.server.URL, "k", "", "")
	_, _, err := client.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no backup files found") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestBackup_DownloadFail(t *testing.T) {
	mock := newMockProwlarr()
	mock.downloadFail = true
	defer mock.close()
	client, _ := NewProwlarrClient(mock.server.URL, "k", "", "")
	_, _, err := client.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "download") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRestore(t *testing.T) {
	mock := newMockProwlarr()
	defer mock.close()
	client, _ := NewProwlarrClient(mock.server.URL, "k", "", "")
	err := client.Restore(context.Background(), bytes.NewReader(makeZip()))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

func TestRestore_Fail(t *testing.T) {
	mock := newMockProwlarr()
	mock.restoreFail = true
	defer mock.close()
	client, _ := NewProwlarrClient(mock.server.URL, "k", "", "")
	err := client.Restore(context.Background(), bytes.NewReader(makeZip()))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "restore upload failed") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRestore_WithRestart(t *testing.T) {
	restartCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/system/backup/restore/upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"RestartRequired": true})
	})
	mux.HandleFunc("POST /api/v1/system/restart", func(w http.ResponseWriter, r *http.Request) {
		restartCalled = true
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, _ := NewProwlarrClient(server.URL, "k", "", "")
	err := client.Restore(context.Background(), bytes.NewReader(makeZip()))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restartCalled {
		t.Error("restart was not called")
	}
}

func TestBackup_ContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/command", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "Backup"
		status := Queued
		resp := CommandResource{Id: &id, Name: &name, Status: &status}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/v1/command/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "Backup"
		status := Started
		resp := CommandResource{Id: &id, Name: &name, Status: &status}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, _ := NewProwlarrClient(server.URL, "k", "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := client.Backup(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestNewProwlarrClient(t *testing.T) {
	client, err := NewProwlarrClient("http://localhost:99999", "test-key", "user", "pass")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if client == nil {
		t.Fatal("nil client")
	}
	if client.apiKey != "test-key" {
		t.Errorf("apiKey = %q", client.apiKey)
	}
	if client.username != "user" {
		t.Errorf("username = %q", client.username)
	}
}

func TestFormsLogin_BadCredentials(t *testing.T) {
	// Mock a server that returns 200 on login but does NOT set the auth cookie
	// (this is what the *arr apps do on failed login)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/command", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "Backup"
		status := Completed
		resp := CommandResource{Id: &id, Name: &name, Status: &status}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/v1/command/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "Backup"
		status := Completed
		resp := CommandResource{Id: &id, Name: &name, Status: &status}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/v1/system/backup", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "prowlarr_backup.zip"
		path := "/backup/manual/prowlarr_backup.zip"
		size := int64(1024)
		ts := time.Date(2025, 2, 9, 12, 0, 0, 0, time.UTC)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]BackupResource{{Id: &id, Name: &name, Path: &path, Size: &size, Time: &ts}})
	})
	mux.HandleFunc("GET /api/v1/config/host", func(w http.ResponseWriter, r *http.Request) {
		authType := AuthenticationType("forms")
		config := HostConfigResource{AuthenticationMethod: &authType}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
	})
	// Login endpoint returns 200 but no cookie (simulating failed login)
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "<html>Login Page</html>")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, _ := NewProwlarrClient(server.URL, "k", "wrong-user", "wrong-pass")
	_, _, err := client.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error for bad credentials")
	}
	if !strings.Contains(err.Error(), "no auth cookie") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestFormsLogin_Success(t *testing.T) {
	// Mock a server that returns 302 and sets the ProwlarrAuth cookie on login
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/command", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "Backup"
		status := Completed
		resp := CommandResource{Id: &id, Name: &name, Status: &status}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/v1/command/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "Backup"
		status := Completed
		resp := CommandResource{Id: &id, Name: &name, Status: &status}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /api/v1/system/backup", func(w http.ResponseWriter, r *http.Request) {
		id := int32(1)
		name := "prowlarr_backup.zip"
		path := "/backup/manual/prowlarr_backup.zip"
		size := int64(1024)
		ts := time.Date(2025, 2, 9, 12, 0, 0, 0, time.UTC)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]BackupResource{{Id: &id, Name: &name, Path: &path, Size: &size, Time: &ts}})
	})
	mux.HandleFunc("GET /api/v1/config/host", func(w http.ResponseWriter, r *http.Request) {
		authType := AuthenticationType("forms")
		config := HostConfigResource{AuthenticationMethod: &authType}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:  "ProwlarrAuth",
			Value: "test-session-token",
			Path:  "/",
		})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /backup/manual/", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		fw, _ := zw.Create("config.xml")
		fw.Write([]byte("<Config><ApiKey>test</ApiKey></Config>"))
		zw.Close()
		w.Header().Set("Content-Type", "application/zip")
		w.Write(buf.Bytes())
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, _ := NewProwlarrClient(server.URL, "k", "admin", "password")
	result, reader, err := client.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup with forms login should succeed: %v", err)
	}
	defer reader.Close()
	if result.Name == "" {
		t.Error("result.Name is empty")
	}
}
