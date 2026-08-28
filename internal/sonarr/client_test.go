package sonarr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backuparr/internal/backup"
)

// ---- small helper unit tests ----

func TestDerefString(t *testing.T) {
	s := "hello"
	if derefString(&s) != "hello" {
		t.Errorf("derefString(&s) = %q, want %q", derefString(&s), "hello")
	}
	if derefString(nil) != "" {
		t.Errorf("derefString(nil) = %q, want empty string", derefString(nil))
	}
}

func TestDerefInt64(t *testing.T) {
	var i int64 = 42
	if derefInt64(&i) != 42 {
		t.Errorf("derefInt64(&i) = %d, want 42", derefInt64(&i))
	}
	if derefInt64(nil) != 0 {
		t.Errorf("derefInt64(nil) = %d, want 0", derefInt64(nil))
	}
}

func TestDerefTime(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := derefTime(&now); !got.Equal(now) {
		t.Errorf("derefTime(&now) = %v, want %v", got, now)
	}
	if got := derefTime(nil); !got.IsZero() {
		t.Errorf("derefTime(nil) = %v, want zero time", got)
	}
}

func TestName(t *testing.T) {
	c, err := NewSonarrClient("http://localhost:8989", "key", "", "", nil)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	if got := c.Name(); got != "sonarr" {
		t.Errorf("Name() = %q, want %q", got, "sonarr")
	}
}

// NewSonarrClient must wire the credential fields onto the client exactly as
// given, since every downstream request (generated-client calls via
// WithRequestEditorFn, and the manual downloadBackup/Restore-upload
// requests) reads them straight off the struct. This pins that wiring so a
// future refactor that merges the *arr clients can't silently drop a field.
func TestNewSonarrClient_StoresCredentials(t *testing.T) {
	c, err := NewSonarrClient("http://localhost:8989", "my-api-key", "my-user", "test-password-not-real", nil)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	if c.apiKey != "my-api-key" {
		t.Errorf("apiKey = %q, want %q", c.apiKey, "my-api-key")
	}
	if c.username != "my-user" {
		t.Errorf("username = %q, want %q", c.username, "my-user")
	}
	if c.password != "test-password-not-real" {
		t.Errorf("password = %q, want %q", c.password, "test-password-not-real")
	}
	if c.baseURL != "http://localhost:8989" {
		t.Errorf("baseURL = %q, want %q", c.baseURL, "http://localhost:8989")
	}
}

// ---- mock server ----

// mockArrServer is a configurable stand-in for the Sonarr API surface that
// client.go actually talks to. Every field has a working default; tests
// mutate only what they need to exercise before invoking client methods.
type mockArrServer struct {
	srv    *httptest.Server
	apiKey string

	// POST /api/v3/command
	commandPostStatus int
	commandID         int32
	commandNoID       bool

	// GET /api/v3/command/{id}
	commandStatuses  []CommandStatus // sequence of statuses returned; last one repeats
	commandStatusMsg string
	commandGetStatus int
	commandGetCalls  int

	// GET /api/v3/system/backup
	backups       []BackupResource
	backupsStatus int

	// GET /api/v3/system/status
	dbType           *DatabaseType
	systemStatusCode int

	// GET /api/v3/config/host
	authMethod       *AuthenticationType
	hostConfigStatus int

	// download (arbitrary path, keyed by exact request path)
	downloadData        map[string][]byte
	downloadContentType string
	downloadStatus      int

	// POST /login
	loginValidUsername string
	loginValidPassword string
	loginStatus        int
	loginSetCookie     bool

	// POST /api/v3/system/backup/restore/upload
	restoreUploadStatus    int
	restoreRestartRequired bool
	restoreUploadedData    []byte
	restoreContentType     string

	// POST /api/v3/system/restart
	restartStatus int
}

func newMockArrServer(t *testing.T, apiKey string) *mockArrServer {
	t.Helper()
	completed := CommandStatusCompleted
	m := &mockArrServer{
		apiKey:            apiKey,
		commandPostStatus: http.StatusOK,
		commandID:         1,
		commandStatuses:   []CommandStatus{completed},
		commandGetStatus:  http.StatusOK,
		backups: []BackupResource{
			{
				Name: strPtr("backup.zip"),
				Path: strPtr("/backup/backup.zip"),
				Size: int64Ptr(20),
				Time: timePtr(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
			},
		},
		backupsStatus:       http.StatusOK,
		systemStatusCode:    http.StatusOK,
		hostConfigStatus:    http.StatusOK,
		downloadData:        map[string][]byte{"/backup/backup.zip": []byte("zip-bytes-here-000")},
		downloadContentType: "application/zip",
		downloadStatus:      http.StatusOK,
		loginStatus:         http.StatusOK,
		loginSetCookie:      true,
		restoreUploadStatus: http.StatusOK,
		restartStatus:       http.StatusOK,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/command", m.handleCommandPost)
	mux.HandleFunc("/api/v3/command/", m.handleCommandGet)
	mux.HandleFunc("/api/v3/system/backup", m.handleBackups)
	mux.HandleFunc("/api/v3/system/status", m.handleSystemStatus)
	mux.HandleFunc("/api/v3/config/host", m.handleHostConfig)
	mux.HandleFunc("/api/v3/system/backup/restore/upload", m.handleRestoreUpload)
	mux.HandleFunc("/api/v3/system/restart", m.handleRestart)
	mux.HandleFunc("/login", m.handleLogin)
	mux.HandleFunc("/", m.handleDownload)

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func strPtr(s string) *string        { return &s }
func int64Ptr(i int64) *int64        { return &i }
func timePtr(t time.Time) *time.Time { return &t }

// checkAPIKey rejects any request whose X-Api-Key header doesn't match the
// configured key with 401, so every test below that reaches a 2xx response
// implicitly proves the header was sent and correct on that endpoint.
func (m *mockArrServer) checkAPIKey(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Api-Key") != m.apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid api key"}`))
		return false
	}
	return true
}

func (m *mockArrServer) handleCommandPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !m.checkAPIKey(w, r) {
		return
	}
	if m.commandPostStatus != 0 && m.commandPostStatus != http.StatusOK {
		w.WriteHeader(m.commandPostStatus)
		w.Write([]byte(`{"error":"command post failed"}`))
		return
	}
	resp := CommandResource{Status: &m.commandStatuses[0]}
	if !m.commandNoID {
		resp.Id = &m.commandID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockArrServer) handleCommandGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	m.commandGetCalls++
	if !m.checkAPIKey(w, r) {
		return
	}
	if m.commandGetStatus != 0 && m.commandGetStatus != http.StatusOK {
		w.WriteHeader(m.commandGetStatus)
		w.Write([]byte(`{"error":"command get failed"}`))
		return
	}
	idx := m.commandGetCalls - 1
	if idx >= len(m.commandStatuses) {
		idx = len(m.commandStatuses) - 1
	}
	status := m.commandStatuses[idx]

	resp := CommandResource{Status: &status}
	if m.commandStatusMsg != "" {
		resp.Message = &m.commandStatusMsg
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockArrServer) handleBackups(w http.ResponseWriter, r *http.Request) {
	if !m.checkAPIKey(w, r) {
		return
	}
	if m.backupsStatus != 0 && m.backupsStatus != http.StatusOK {
		w.WriteHeader(m.backupsStatus)
		w.Write([]byte(`{"error":"backups failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m.backups)
}

func (m *mockArrServer) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if !m.checkAPIKey(w, r) {
		return
	}
	if m.systemStatusCode != 0 && m.systemStatusCode != http.StatusOK {
		w.WriteHeader(m.systemStatusCode)
		w.Write([]byte(`{"error":"status failed"}`))
		return
	}
	status := SystemResource{DatabaseType: m.dbType}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (m *mockArrServer) handleHostConfig(w http.ResponseWriter, r *http.Request) {
	if !m.checkAPIKey(w, r) {
		return
	}
	if m.hostConfigStatus != 0 && m.hostConfigStatus != http.StatusOK {
		w.WriteHeader(m.hostConfigStatus)
		w.Write([]byte(`{"error":"host config failed"}`))
		return
	}
	cfg := HostConfigResource{AuthenticationMethod: m.authMethod}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func (m *mockArrServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !m.checkAPIKey(w, r) {
		return
	}
	data, ok := m.downloadData[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if m.downloadStatus != 0 && m.downloadStatus != http.StatusOK {
		w.WriteHeader(m.downloadStatus)
		w.Write([]byte("download error"))
		return
	}
	if m.downloadContentType != "" {
		w.Header().Set("Content-Type", m.downloadContentType)
	}
	w.Write(data)
}

func (m *mockArrServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")

	if m.loginStatus != 0 && m.loginStatus != http.StatusOK {
		w.WriteHeader(m.loginStatus)
		return
	}

	valid := user == m.loginValidUsername && pass == m.loginValidPassword
	if valid && m.loginSetCookie {
		http.SetCookie(w, &http.Cookie{Name: "SonarrAuth", Value: "session-token"})
	}
	w.WriteHeader(http.StatusOK)
}

func (m *mockArrServer) handleRestoreUpload(w http.ResponseWriter, r *http.Request) {
	if !m.checkAPIKey(w, r) {
		return
	}
	m.restoreContentType = r.Header.Get("Content-Type")
	if m.restoreUploadStatus != 0 && m.restoreUploadStatus != http.StatusOK {
		w.WriteHeader(m.restoreUploadStatus)
		w.Write([]byte("upload failed"))
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err == nil {
		if f, _, err := r.FormFile("restore"); err == nil {
			defer f.Close()
			m.restoreUploadedData, _ = io.ReadAll(f)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"RestartRequired": m.restoreRestartRequired})
}

func (m *mockArrServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !m.checkAPIKey(w, r) {
		return
	}
	if m.restartStatus != 0 && m.restartStatus != http.StatusOK {
		w.WriteHeader(m.restartStatus)
		w.Write([]byte("restart failed"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// buildZip creates a zip archive from the given filename->contents map.
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip.Create(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write(%q): %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	return buf.Bytes()
}

// configXML renders a config.xml body with the given postgres fields (blank means omit the value).
func configXML(host, port, user, password, mainDB, logDB string) string {
	type cfg struct {
		XMLName          xml.Name `xml:"Config"`
		PostgresHost     string   `xml:"PostgresHost"`
		PostgresPort     string   `xml:"PostgresPort"`
		PostgresUser     string   `xml:"PostgresUser"`
		PostgresPassword string   `xml:"PostgresPassword"`
		PostgresMainDb   string   `xml:"PostgresMainDb"`
		PostgresLogDb    string   `xml:"PostgresLogDb"`
	}
	out, _ := xml.Marshal(cfg{
		PostgresHost:     host,
		PostgresPort:     port,
		PostgresUser:     user,
		PostgresPassword: password,
		PostgresMainDb:   mainDB,
		PostgresLogDb:    logDB,
	})
	return string(out)
}

// ---- Backup(): happy path & selection ----

func TestBackup_Success(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	sqlite := DatabaseType("sqLite")
	m.dbType = &sqlite
	none := AuthenticationType("none")
	m.authMethod = &none

	c, err := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}

	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()

	if result.Name != "backup.zip" {
		t.Errorf("result.Name = %q, want %q", result.Name, "backup.zip")
	}
	if result.Path != "/backup/backup.zip" {
		t.Errorf("result.Path = %q, want %q", result.Path, "/backup/backup.zip")
	}
	wantData := m.downloadData["/backup/backup.zip"]
	if result.Size != int64(len(wantData)) {
		t.Errorf("result.Size = %d, want %d", result.Size, len(wantData))
	}
	if !result.CreatedAt.Equal(*m.backups[0].Time) {
		t.Errorf("result.CreatedAt = %v, want %v", result.CreatedAt, *m.backups[0].Time)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, wantData) {
		t.Errorf("backup data = %q, want %q", got, wantData)
	}
}

// The client takes backups[0] as "the latest" without sorting by Time.
// This pins that (buggy-looking) selection behavior; see the linked
// "no sort on backups[0]" issue — do not fix here.
func TestBackup_SelectsFirstEntryRegardlessOfRecency(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	older := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m.backups = []BackupResource{
		{Name: strPtr("old.zip"), Path: strPtr("/backup/old.zip"), Size: int64Ptr(3), Time: &older},
		{Name: strPtr("new.zip"), Path: strPtr("/backup/new.zip"), Size: int64Ptr(3), Time: &newer},
	}
	m.downloadData["/backup/old.zip"] = []byte("old")
	m.downloadData["/backup/new.zip"] = []byte("new")

	c, err := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()

	if result.Name != "old.zip" {
		t.Errorf("result.Name = %q, want %q (index-0 selection, not most recent)", result.Name, "old.zip")
	}
}

func TestBackup_NoBackupsFound(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.backups = []BackupResource{}

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when no backups are returned")
	}
	if !strings.Contains(err.Error(), "no backup files found") {
		t.Errorf("error = %q, want mention of no backup files", err.Error())
	}
}

func TestBackup_GetBackupFilesAPIError(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.backupsStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when backups list API errors")
	}
	if !strings.Contains(err.Error(), "failed to get backup files") {
		t.Errorf("error = %q, want mention of failed to get backup files", err.Error())
	}
}

func TestBackup_DownloadPathEmpty(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.backups = []BackupResource{{Name: strPtr("x.zip"), Path: strPtr(""), Size: int64Ptr(0)}}

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when backup path is empty")
	}
	if !strings.Contains(err.Error(), "backup path is empty") {
		t.Errorf("error = %q, want mention of empty backup path", err.Error())
	}
}

// ---- runBackupCommand / waitForCommand (trigger + poll) ----

func TestBackup_CommandPostAPIError(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.commandPostStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when the command POST errors")
	}
	if !strings.Contains(err.Error(), "API error: 500") {
		t.Errorf("error = %q, want mention of API error 500", err.Error())
	}
}

func TestBackup_CommandNoID(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.commandNoID = true

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when command response has no ID")
	}
	if !strings.Contains(err.Error(), "no ID") {
		t.Errorf("error = %q, want mention of missing ID", err.Error())
	}
}

func TestBackup_CommandFailedStatus(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	failed := CommandStatusFailed
	m.commandStatuses = []CommandStatus{failed}
	m.commandStatusMsg = "disk full"

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when command status is failed")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want mention of failure message", err.Error())
	}
}

func TestBackup_CommandCancelledStatus(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	cancelled := CommandStatusCancelled
	m.commandStatuses = []CommandStatus{cancelled}

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when command is cancelled")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %q, want mention of cancellation", err.Error())
	}
}

func TestBackup_CommandAbortedStatus(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	aborted := CommandStatusAborted
	m.commandStatuses = []CommandStatus{aborted}

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when command is aborted")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error = %q, want mention of abort", err.Error())
	}
}

// waitForCommand never checks the HTTP status code of the command-status
// response — unlike every other endpoint in this client. A non-2xx response
// with a decodable-but-empty JSON body just yields a CommandResource with a
// nil Status, which the loop treats as "not terminal yet" and keeps polling
// forever. This pins that (surprising) behavior via a context timeout, since
// the loop never exits on its own.
func TestBackup_CommandGetNonOKStatus_PollsForeverUntilContextCancelled(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.commandGetStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := c.Backup(ctx)
	if err == nil {
		t.Fatal("Backup() should eventually stop once the context is cancelled")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %q, want context deadline exceeded (the 500 status itself is never surfaced)", err.Error())
	}
	if m.commandGetCalls < 1 {
		t.Errorf("commandGetCalls = %d, want at least 1 poll before the context gave up", m.commandGetCalls)
	}
}

// The polling loop re-polls every 2s until a terminal status is seen. This
// drives it through one non-terminal ("queued") response before completion,
// which costs one real ~2s wait — the loop offers no faster path to exercise.
func TestBackup_PollingLoopMultipleIterations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 2s polling-loop test in short mode")
	}
	m := newMockArrServer(t, "test-api-key")
	queued := CommandStatusQueued
	completed := CommandStatusCompleted
	m.commandStatuses = []CommandStatus{queued, completed}

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()

	if m.commandGetCalls < 2 {
		t.Errorf("commandGetCalls = %d, want at least 2 (loop should have polled more than once)", m.commandGetCalls)
	}
}

func TestBackup_PollingContextCancellation(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	queued := CommandStatusQueued
	m.commandStatuses = []CommandStatus{queued}

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := c.Backup(ctx)
	if err == nil {
		t.Fatal("Backup() should fail when context is cancelled during polling")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %q, want context deadline exceeded", err.Error())
	}
}

// ---- getDatabaseType ----

// A failed database-type lookup is only logged, not surfaced — the backup
// proceeds as if it were SQLite. This pins that silent-swallow behavior.
func TestBackup_DatabaseTypeAPIError_ContinuesAsSQLite(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.systemStatusCode = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() should succeed despite database-type lookup failure: %v", err)
	}
	defer reader.Close()
	if result == nil {
		t.Fatal("expected a non-nil result")
	}
}

func TestBackup_DatabaseTypeMissing_DefaultsToSQLite(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.dbType = nil // system/status omits databaseType entirely

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()
}

// ---- postgres branch ----

func TestBackup_PostgresDetected_NoConfigXMLInBackup(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	pg := DatabaseType("postgreSQL")
	m.dbType = &pg
	// zip has no config.xml at all
	m.downloadData["/backup/backup.zip"] = buildZip(t, map[string]string{"other.txt": "hi"})

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when config.xml is missing from a postgres backup")
	}
	if !strings.Contains(err.Error(), "failed to parse postgres config") {
		t.Errorf("error = %q, want mention of postgres config parse failure", err.Error())
	}
}

// When dbType reports postgreSQL but the backup's config.xml has no postgres
// settings (and no override is configured), the client silently treats it
// like a SQLite backup and returns the data unmodified.
func TestBackup_PostgresDetected_ConfigXMLHasNoPostgresInfo(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	pg := DatabaseType("postgreSQL")
	m.dbType = &pg
	zipData := buildZip(t, map[string]string{"config.xml": configXML("", "", "", "", "", "")})
	m.downloadData["/backup/backup.zip"] = zipData

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()
	got, _ := io.ReadAll(reader)
	if !bytes.Equal(got, zipData) {
		t.Error("backup data should be returned unmodified when no postgres info is present")
	}
	if result.Size != int64(len(zipData)) {
		t.Errorf("result.Size = %d, want %d", result.Size, len(zipData))
	}
}

// When the backup's config.xml has no postgres info but a pgOverride is
// configured, the override becomes the full postgres config. With empty
// MainDB/LogDB, DumpAllDatabases is a no-op (no pg_dump invocation), and the
// enhanced backup is created with zero dump files added.
func TestBackup_PostgresDetected_OverrideUsedWhenConfigXMLEmpty(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	pg := DatabaseType("postgreSQL")
	m.dbType = &pg
	zipData := buildZip(t, map[string]string{
		"config.xml": configXML("", "", "", "", "", ""),
		"other.txt":  "keep-me",
	})
	m.downloadData["/backup/backup.zip"] = zipData

	override := &backup.PostgresConfig{Host: "overridehost", Port: "5432"}
	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", override)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("resulting backup is not a valid zip: %v", err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "other.txt" {
			found = true
		}
	}
	if !found {
		t.Error("enhanced backup should still contain the original files")
	}
	if result.Size != int64(len(got)) {
		t.Errorf("result.Size = %d, want %d", result.Size, len(got))
	}
}

// When config.xml has postgres settings AND a pgOverride is set, the override
// fields are merged on top of the parsed config rather than replacing it.
func TestBackup_PostgresDetected_OverrideMergedOntoConfigXML(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	pg := DatabaseType("postgreSQL")
	m.dbType = &pg
	zipData := buildZip(t, map[string]string{
		"config.xml": configXML("cfghost", "5432", "cfguser", "cfgpass", "", ""),
	})
	m.downloadData["/backup/backup.zip"] = zipData

	override := &backup.PostgresConfig{Host: "overridehost"}
	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", override)
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()
}

// With a real MainDB configured, DumpAllDatabases actually shells out to
// pg_dump. Against an unreachable host this fails fast and the error is
// wrapped by Backup(), regardless of whether pg_dump is even installed.
func TestBackup_PostgresDumpFailure(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	pg := DatabaseType("postgreSQL")
	m.dbType = &pg
	zipData := buildZip(t, map[string]string{
		"config.xml": configXML("127.0.0.1", "1", "user", "pass", "maindb", ""),
	})
	m.downloadData["/backup/backup.zip"] = zipData

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when pg_dump cannot reach the database")
	}
	if !strings.Contains(err.Error(), "failed to dump postgres databases") {
		t.Errorf("error = %q, want mention of postgres dump failure", err.Error())
	}
}

// ---- downloadBackup / getAuthMethod ----

func TestBackup_DownloadContentTypeMismatch(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.downloadContentType = "text/html"

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail on unexpected content type")
	}
	if !strings.Contains(err.Error(), "unexpected content type") {
		t.Errorf("error = %q, want mention of content type", err.Error())
	}
}

func TestBackup_DownloadAPIError(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.downloadStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when download errors")
	}
	if !strings.Contains(err.Error(), "download error") {
		t.Errorf("error = %q, want mention of download error", err.Error())
	}
}

func TestBackup_GetAuthMethodAPIError(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.hostConfigStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when auth method lookup errors")
	}
	if !strings.Contains(err.Error(), "failed to get auth method") {
		t.Errorf("error = %q, want mention of auth method failure", err.Error())
	}
}

func TestBackup_AuthMethodBasic_SendsBasicAuth(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	basic := AuthenticationType("basic")
	m.authMethod = &basic

	var gotUser, gotPass string
	var gotOK bool
	origDownload := m.downloadData["/backup/backup.zip"]
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/command", m.handleCommandPost)
	mux.HandleFunc("/api/v3/command/", m.handleCommandGet)
	mux.HandleFunc("/api/v3/system/backup", m.handleBackups)
	mux.HandleFunc("/api/v3/system/status", m.handleSystemStatus)
	mux.HandleFunc("/api/v3/config/host", m.handleHostConfig)
	mux.HandleFunc("/backup/backup.zip", func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.Header().Set("Content-Type", "application/zip")
		w.Write(origDownload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := NewSonarrClient(srv.URL, "test-api-key", "myuser", "mypass", nil)
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()

	if !gotOK {
		t.Fatal("expected basic auth credentials on the download request")
	}
	if gotUser != "myuser" || gotPass != "mypass" {
		t.Errorf("basic auth = %q/%q, want %q/%q", gotUser, gotPass, "myuser", "mypass")
	}
}

func TestBackup_AuthMethodForms_LoginSucceeds(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	forms := AuthenticationType("forms")
	m.authMethod = &forms
	m.loginValidUsername = "admin"
	m.loginValidPassword = "secret"

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "admin", "secret", nil)
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()
}

func TestBackup_AuthMethodForms_InvalidCredentials(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	forms := AuthenticationType("forms")
	m.authMethod = &forms
	m.loginValidUsername = "admin"
	m.loginValidPassword = "secret"
	m.loginStatus = http.StatusUnauthorized

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "admin", "wrong", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail with invalid form login credentials")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %q, want mention of invalid credentials", err.Error())
	}
}

func TestBackup_AuthMethodForms_NoCookieOnSuccessStatus(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	forms := AuthenticationType("forms")
	m.authMethod = &forms
	m.loginValidUsername = "admin"
	m.loginValidPassword = "secret"
	m.loginSetCookie = false // server returns 200 but never sets the auth cookie

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "admin", "secret", nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when no auth cookie is set despite a 200 login response")
	}
	if !strings.Contains(err.Error(), "no auth cookie") {
		t.Errorf("error = %q, want mention of missing auth cookie", err.Error())
	}
}

func TestBackup_AuthMethodNone_NoSessionAuth(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	none := AuthenticationType("none")
	m.authMethod = &none

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()
}

// ---- API key propagation ----

// checkAPIKey (installed on every mock handler above) rejects a mismatched
// X-Api-Key with 401, so every passing test above already proves the header
// reaches each endpoint it touches with the right value. This test
// additionally captures the raw header value on the backup-listing request
// to pin the exact header name and value used, independent of that
// pass/fail signal.
func TestBackup_APIKeyHeaderReachesServer(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	var gotKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/command", m.handleCommandPost)
	mux.HandleFunc("/api/v3/command/", m.handleCommandGet)
	mux.HandleFunc("/api/v3/system/backup", func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		m.handleBackups(w, r)
	})
	mux.HandleFunc("/api/v3/system/status", m.handleSystemStatus)
	mux.HandleFunc("/api/v3/config/host", m.handleHostConfig)
	mux.HandleFunc("/", m.handleDownload)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewSonarrClient(srv.URL, "test-api-key", "", "", nil)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()

	if gotKey != "test-api-key" {
		t.Errorf("X-Api-Key header on backup-listing request = %q, want %q", gotKey, "test-api-key")
	}
}

// A mismatched API key is rejected by the (real) *arr API with 401; this
// pins that Backup() surfaces that as a plain API error rather than masking
// it. (401 is not in the retry-transport's retryable status set, so this
// fails on the first attempt, not after a retry delay.)
func TestBackup_WrongAPIKeyRejected(t *testing.T) {
	m := newMockArrServer(t, "correct-key")

	c, err := NewSonarrClient(m.srv.URL, "wrong-key", "", "", nil)
	if err != nil {
		t.Fatalf("NewSonarrClient: %v", err)
	}
	_, _, err = c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when the client's API key doesn't match the server's")
	}
}

// ---- Restore ----

func TestRestore_Success_NoRestartRequired(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.restoreRestartRequired = false

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	zipData := buildZip(t, map[string]string{"config.xml": "<Config/>"})
	err := c.Restore(context.Background(), bytes.NewReader(zipData))
	if err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	if !bytes.Equal(m.restoreUploadedData, zipData) {
		t.Errorf("uploaded data = %q, want %q", m.restoreUploadedData, zipData)
	}
	if !strings.HasPrefix(m.restoreContentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", m.restoreContentType)
	}
}

func TestRestore_RestartRequired_TriggersRestart(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.restoreRestartRequired = true
	restartCalled := false
	origRestart := m.restartStatus
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/backup/restore/upload", m.handleRestoreUpload)
	mux.HandleFunc("/api/v3/system/restart", func(w http.ResponseWriter, r *http.Request) {
		restartCalled = true
		if origRestart != 0 && origRestart != http.StatusOK {
			w.WriteHeader(origRestart)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := NewSonarrClient(srv.URL, "test-api-key", "", "", nil)
	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	if !restartCalled {
		t.Error("expected restart endpoint to be called when RestartRequired is true")
	}
}

func TestRestore_RestartFails(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.restoreRestartRequired = true
	m.restartStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err == nil {
		t.Fatal("Restore() should fail when restart fails")
	}
	if !strings.Contains(err.Error(), "failed to restart") {
		t.Errorf("error = %q, want mention of restart failure", err.Error())
	}
}

func TestRestore_UploadAPIError(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	m.restoreUploadStatus = http.StatusInternalServerError

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err == nil {
		t.Fatal("Restore() should fail when upload errors")
	}
	if !strings.Contains(err.Error(), "restore upload failed") {
		t.Errorf("error = %q, want mention of restore upload failure", err.Error())
	}
}

func TestRestore_PostgresDumpsButNoPostgresConfig(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	zipData := buildZip(t, map[string]string{
		"config.xml":     configXML("", "", "", "", "", ""),
		"postgres/x.sql": "SELECT 1;",
	})

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	err := c.Restore(context.Background(), bytes.NewReader(zipData))
	if err == nil {
		t.Fatal("Restore() should fail when postgres dumps exist but config.xml has no postgres settings")
	}
	if !strings.Contains(err.Error(), "no postgres settings") {
		t.Errorf("error = %q, want mention of missing postgres settings", err.Error())
	}
}

// With postgres dumps present and a resolvable config, RestoreAllDatabases
// shells out to psql. Against an unreachable host this fails fast and the
// error is wrapped by Restore(), regardless of whether psql is installed.
func TestRestore_PostgresRestoreFailure(t *testing.T) {
	m := newMockArrServer(t, "test-api-key")
	zipData := buildZip(t, map[string]string{
		"config.xml":          configXML("127.0.0.1", "1", "user", "pass", "maindb", ""),
		"postgres/maindb.sql": "SELECT 1;",
	})

	c, _ := NewSonarrClient(m.srv.URL, "test-api-key", "", "", nil)
	err := c.Restore(context.Background(), bytes.NewReader(zipData))
	if err == nil {
		t.Fatal("Restore() should fail when psql cannot reach the database")
	}
	if !strings.Contains(err.Error(), "failed to restore postgres databases") {
		t.Errorf("error = %q, want mention of postgres restore failure", err.Error())
	}
}
