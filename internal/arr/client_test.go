package arr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backuparr/internal/backup"
)

// ---- deref / sort helpers ----

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

func strPtr(s string) *string        { return &s }
func int64Ptr(i int64) *int64        { return &i }
func timePtr(t time.Time) *time.Time { return &t }

func TestSortBackupsByTimeDesc(t *testing.T) {
	older := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	backups := []BackupFile{
		{Name: strPtr("no-time"), Time: nil},
		{Name: strPtr("old"), Time: &older},
		{Name: strPtr("new"), Time: &newer},
	}
	sortBackupsByTimeDesc(backups)
	got := []string{*backups[0].Name, *backups[1].Name, *backups[2].Name}
	want := []string{"new", "old", "no-time"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// ---- fake server + Operations adapter ----
//
// fakeOps implements Operations (and, when withDB is true, DatabaseTyper)
// by making real HTTP calls against a fakeServer, mirroring how the real
// per-package adapters call their generated clients. This exercises the
// shared Client logic end-to-end without depending on any package's
// generated code.

type fakeServer struct {
	srv    *httptest.Server
	apiKey string

	commandPostStatus int
	commandID         int32
	commandNoID       bool

	commandStatuses  []string
	commandStatusMsg string
	commandGetStatus int
	commandGetCalls  int

	backups           []BackupFile
	preTriggerBackups []BackupFile
	backupsStatus     int
	backupsCallCount  int

	dbType           *string
	systemStatusCode int

	authMethod       *string
	hostConfigStatus int

	downloadData        map[string][]byte
	downloadContentType string
	downloadStatus      int
	downloadCalls       int

	loginValidUsername string
	loginValidPassword string
	loginStatus        int
	loginSetCookie     bool

	restoreUploadStatus    int
	restoreRestartRequired bool
	restoreUploadedData    []byte

	restartStatus int
}

func newFakeServer(t *testing.T, apiKey string) *fakeServer {
	t.Helper()
	f := &fakeServer{
		apiKey:            apiKey,
		commandPostStatus: http.StatusOK,
		commandID:         1,
		commandStatuses:   []string{"completed"},
		commandGetStatus:  http.StatusOK,
		backups: []BackupFile{
			{Name: strPtr("backup.zip"), Path: strPtr("/backup/backup.zip"), Size: int64Ptr(20), Time: timePtr(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))},
		},
		backupsStatus:       http.StatusOK,
		systemStatusCode:    http.StatusOK,
		hostConfigStatus:    http.StatusOK,
		downloadData:        map[string][]byte{"/backup/backup.zip": []byte("zip-bytes")},
		downloadContentType: "application/zip",
		downloadStatus:      http.StatusOK,
		loginStatus:         http.StatusOK,
		loginSetCookie:      true,
		restoreUploadStatus: http.StatusOK,
		restartStatus:       http.StatusOK,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/command", f.handleCommandPost)
	mux.HandleFunc("/command/", f.handleCommandGet)
	mux.HandleFunc("/backup", f.handleBackups)
	mux.HandleFunc("/status", f.handleSystemStatus)
	mux.HandleFunc("/host", f.handleHostConfig)
	mux.HandleFunc("/api/v3/system/backup/restore/upload", f.handleRestoreUpload)
	mux.HandleFunc("/restart", f.handleRestart)
	mux.HandleFunc("/login", f.handleLogin)
	mux.HandleFunc("/", f.handleDownload)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) checkAPIKey(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Api-Key") != f.apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeServer) handleCommandPost(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.commandPostStatus != http.StatusOK {
		w.WriteHeader(f.commandPostStatus)
		w.Write([]byte(`{"error":"command post failed"}`))
		return
	}
	resp := commandStatus{Status: &f.commandStatuses[0]}
	if !f.commandNoID {
		resp.Id = &f.commandID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeServer) handleCommandGet(w http.ResponseWriter, r *http.Request) {
	f.commandGetCalls++
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.commandGetStatus != http.StatusOK {
		w.WriteHeader(f.commandGetStatus)
		w.Write([]byte(`{"error":"command get failed"}`))
		return
	}
	idx := f.commandGetCalls - 1
	if idx >= len(f.commandStatuses) {
		idx = len(f.commandStatuses) - 1
	}
	status := f.commandStatuses[idx]
	resp := commandStatus{Status: &status}
	if f.commandStatusMsg != "" {
		resp.Message = &f.commandStatusMsg
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeServer) handleBackups(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.backupsStatus != http.StatusOK {
		w.WriteHeader(f.backupsStatus)
		return
	}
	f.backupsCallCount++
	list := f.backups
	if f.backupsCallCount == 1 {
		list = f.preTriggerBackups
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (f *fakeServer) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.systemStatusCode != http.StatusOK {
		w.WriteHeader(f.systemStatusCode)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		DatabaseType *string `json:"databaseType,omitempty"`
	}{DatabaseType: f.dbType})
}

func (f *fakeServer) handleHostConfig(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.hostConfigStatus != http.StatusOK {
		w.WriteHeader(f.hostConfigStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		AuthenticationMethod *string `json:"authenticationMethod,omitempty"`
	}{AuthenticationMethod: f.authMethod})
}

func (f *fakeServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	f.downloadCalls++
	data, ok := f.downloadData[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.downloadStatus != http.StatusOK {
		w.WriteHeader(f.downloadStatus)
		return
	}
	if f.downloadContentType != "" {
		w.Header().Set("Content-Type", f.downloadContentType)
	}
	w.Write(data)
}

func (f *fakeServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.loginStatus != http.StatusOK {
		w.WriteHeader(f.loginStatus)
		return
	}
	valid := r.FormValue("username") == f.loginValidUsername && r.FormValue("password") == f.loginValidPassword
	if valid && f.loginSetCookie {
		http.SetCookie(w, &http.Cookie{Name: "AppAuth", Value: "session-token"})
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeServer) handleRestoreUpload(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.restoreUploadStatus != http.StatusOK {
		w.WriteHeader(f.restoreUploadStatus)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err == nil {
		if file, _, err := r.FormFile("restore"); err == nil {
			defer file.Close()
			f.restoreUploadedData, _ = io.ReadAll(file)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"RestartRequired": f.restoreRestartRequired})
}

func (f *fakeServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !f.checkAPIKey(w, r) {
		return
	}
	if f.restartStatus != http.StatusOK {
		w.WriteHeader(f.restartStatus)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// fakeOps adapts Operations to fakeServer over real HTTP, exactly as the
// production adapters do against their generated clients.
type fakeOps struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

func (f *fakeOps) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", f.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return f.httpClient.Do(req)
}

func (f *fakeOps) PostCommand(ctx context.Context, name string) (*http.Response, error) {
	return f.do(ctx, http.MethodPost, "/command", strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
}

func (f *fakeOps) GetCommand(ctx context.Context, id int32) (*http.Response, error) {
	return f.do(ctx, http.MethodGet, fmt.Sprintf("/command/%d", id), nil)
}

func (f *fakeOps) GetConfigHost(ctx context.Context) (*http.Response, error) {
	return f.do(ctx, http.MethodGet, "/host", nil)
}

func (f *fakeOps) GetSystemBackup(ctx context.Context) (*http.Response, error) {
	return f.do(ctx, http.MethodGet, "/backup", nil)
}

func (f *fakeOps) PostSystemRestart(ctx context.Context) (*http.Response, error) {
	return f.do(ctx, http.MethodPost, "/restart", nil)
}

// fakeOpsWithDB additionally implements DatabaseTyper, mirroring sonarr/radarr.
type fakeOpsWithDB struct {
	*fakeOps
}

func (f *fakeOpsWithDB) GetSystemStatus(ctx context.Context) (*http.Response, error) {
	return f.do(ctx, http.MethodGet, "/status", nil)
}

func newTestClient(t *testing.T, srv *fakeServer, withDB bool, pgOverride *backup.PostgresConfig) *Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	httpClient := &http.Client{Jar: jar}
	base := &fakeOps{httpClient: httpClient, baseURL: srv.srv.URL, apiKey: srv.apiKey}

	var ops Operations = base
	if withDB {
		ops = &fakeOpsWithDB{fakeOps: base}
	}

	return NewClient(Config{
		AppName:    "testapp",
		APIVersion: "v3",
		BaseURL:    srv.srv.URL,
		APIKey:     srv.apiKey,
		HTTPClient: httpClient,
		PgOverride: pgOverride,
		Ops:        ops,
	})
}

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
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	return buf.Bytes()
}

// ---- Config / accessors ----

func TestClient_NameAndAccessors(t *testing.T) {
	c := NewClient(Config{
		AppName:  "testapp",
		BaseURL:  "http://localhost:1234",
		APIKey:   "key",
		Username: "user",
		Password: "pass",
	})
	if c.Name() != "testapp" {
		t.Errorf("Name() = %q, want testapp", c.Name())
	}
	if c.APIKey() != "key" || c.Username() != "user" || c.Password() != "pass" || c.BaseURL() != "http://localhost:1234" {
		t.Errorf("accessors did not round-trip Config: %+v", c)
	}
}

// ---- Backup() ----

func TestBackup_Success_NoDatabaseTyper(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	none := "none"
	srv.authMethod = &none

	c := newTestClient(t, srv, false, nil)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()

	if result.Name != "backup.zip" {
		t.Errorf("result.Name = %q, want backup.zip", result.Name)
	}
	got, _ := io.ReadAll(reader)
	if !bytes.Equal(got, srv.downloadData["/backup/backup.zip"]) {
		t.Errorf("backup data mismatch")
	}
}

func TestBackup_Success_DatabaseTyperSQLite(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	sqlite := "sqLite"
	srv.dbType = &sqlite
	none := "none"
	srv.authMethod = &none

	c := newTestClient(t, srv, true, nil)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()
	if result.Size != int64(len(srv.downloadData["/backup/backup.zip"])) {
		t.Errorf("result.Size = %d, want unmodified backup size", result.Size)
	}
}

func TestBackup_DatabaseTyperPostgresNoConfigXML(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	pg := "postgreSQL"
	srv.dbType = &pg
	none := "none"
	srv.authMethod = &none
	srv.downloadData["/backup/backup.zip"] = buildZip(t, map[string]string{"other.txt": "hi"})

	c := newTestClient(t, srv, true, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when config.xml is missing from a postgres backup")
	}
	if !strings.Contains(err.Error(), "failed to parse postgres config") {
		t.Errorf("error = %q, want postgres config parse failure", err.Error())
	}
}

func TestBackup_DatabaseTyperError_ContinuesAsSQLite(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.systemStatusCode = http.StatusInternalServerError
	none := "none"
	srv.authMethod = &none

	c := newTestClient(t, srv, true, nil)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() should succeed despite database-type lookup failure: %v", err)
	}
	defer reader.Close()
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestBackup_SelectsNewestEntry(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	none := "none"
	srv.authMethod = &none
	older := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	srv.backups = []BackupFile{
		{Name: strPtr("old.zip"), Path: strPtr("/backup/old.zip"), Size: int64Ptr(3), Time: &older},
		{Name: strPtr("new.zip"), Path: strPtr("/backup/new.zip"), Size: int64Ptr(3), Time: &newer},
	}
	srv.downloadData["/backup/old.zip"] = []byte("old")
	srv.downloadData["/backup/new.zip"] = []byte("new")

	c := newTestClient(t, srv, false, nil)
	result, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	defer reader.Close()
	if result.Name != "new.zip" {
		t.Errorf("result.Name = %q, want new.zip (newest by Time)", result.Name)
	}
}

func TestBackup_NoFreshBackup_Fails(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	existing := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	srv.backups = []BackupFile{{Name: strPtr("backup.zip"), Path: strPtr("/backup/backup.zip"), Size: int64Ptr(3), Time: &existing}}
	srv.preTriggerBackups = srv.backups

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup() should fail when no newer backup appears")
	}
	if !strings.Contains(err.Error(), "no fresh backup found") {
		t.Errorf("error = %q, want no fresh backup found", err.Error())
	}
	if srv.downloadCalls != 0 {
		t.Errorf("downloadCalls = %d, want 0", srv.downloadCalls)
	}
}

func TestBackup_NoBackupsFound(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.backups = []BackupFile{}

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no backup files found") {
		t.Errorf("error = %v, want no backup files found", err)
	}
}

func TestBackup_GetBackupFilesAPIError(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.backupsStatus = http.StatusInternalServerError

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to get backup files") {
		t.Errorf("error = %v, want failed to get backup files", err)
	}
}

func TestBackup_CommandNoID(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.commandNoID = true

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no ID") {
		t.Errorf("error = %v, want mention of missing ID", err)
	}
}

func TestBackup_CommandFailedStatus(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.commandStatuses = []string{"failed"}
	srv.commandStatusMsg = "disk full"

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %v, want mention of failure message", err)
	}
}

func TestBackup_CommandCancelledAndAbortedStatus(t *testing.T) {
	for _, status := range []string{"cancelled", "aborted"} {
		srv := newFakeServer(t, "test-key")
		srv.commandStatuses = []string{status}

		c := newTestClient(t, srv, false, nil)
		_, _, err := c.Backup(context.Background())
		if err == nil || !strings.Contains(err.Error(), status) {
			t.Errorf("status %s: error = %v, want mention of %s", status, err, status)
		}
	}
}

// waitForCommand never checks the HTTP status code of the command-status
// response, so a non-2xx response decodes to a nil Status and the loop
// treats it as "not terminal yet" and polls forever. This pins that
// (surprising, tracked separately) behavior via a context timeout.
func TestWaitForCommand_NonOKStatus_PollsUntilContextCancelled(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.commandGetStatus = http.StatusInternalServerError

	c := newTestClient(t, srv, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := c.Backup(ctx)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %v, want context deadline exceeded", err)
	}
	if srv.commandGetCalls < 1 {
		t.Errorf("commandGetCalls = %d, want at least 1", srv.commandGetCalls)
	}
}

func TestBackup_PollingContextCancellation(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.commandStatuses = []string{"queued"}

	c := newTestClient(t, srv, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := c.Backup(ctx)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %v, want context deadline exceeded", err)
	}
}

func TestBackup_DownloadPathEmpty(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.backups = []BackupFile{{Name: strPtr("x.zip"), Path: strPtr(""), Size: int64Ptr(0)}}

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "backup path is empty") {
		t.Errorf("error = %v, want mention of empty backup path", err)
	}
}

func TestBackup_DownloadContentTypeMismatch(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.downloadContentType = "text/html"

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unexpected content type") {
		t.Errorf("error = %v, want mention of content type", err)
	}
}

func TestBackup_AuthMethodBasic_SendsBasicAuth(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	basic := "basic"
	srv.authMethod = &basic

	var gotUser, gotPass string
	var gotOK bool
	origDownload := srv.downloadData["/backup/backup.zip"]
	mux := http.NewServeMux()
	mux.HandleFunc("/command", srv.handleCommandPost)
	mux.HandleFunc("/command/", srv.handleCommandGet)
	mux.HandleFunc("/backup", srv.handleBackups)
	mux.HandleFunc("/host", srv.handleHostConfig)
	mux.HandleFunc("/backup/backup.zip", func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.Header().Set("Content-Type", "application/zip")
		w.Write(origDownload)
	})
	newSrv := httptest.NewServer(mux)
	defer newSrv.Close()
	srv.srv.Close()
	srv.srv = newSrv

	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: jar}
	c := NewClient(Config{
		AppName:    "testapp",
		BaseURL:    newSrv.URL,
		APIKey:     "test-key",
		Username:   "myuser",
		Password:   "mypass",
		HTTPClient: httpClient,
		Ops:        &fakeOps{httpClient: httpClient, baseURL: newSrv.URL, apiKey: "test-key"},
	})

	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()

	if !gotOK || gotUser != "myuser" || gotPass != "mypass" {
		t.Errorf("basic auth = %q/%q (ok=%v), want myuser/mypass", gotUser, gotPass, gotOK)
	}
}

func TestBackup_AuthMethodForms_LoginSucceedsAndFails(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	forms := "forms"
	srv.authMethod = &forms
	srv.loginValidUsername = "admin"
	srv.loginValidPassword = "secret"

	c := newClientWithCreds(t, srv, "admin", "secret")
	_, reader, err := c.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup() error: %v", err)
	}
	reader.Close()

	srv2 := newFakeServer(t, "test-key")
	srv2.authMethod = &forms
	srv2.loginValidUsername = "admin"
	srv2.loginValidPassword = "secret"
	srv2.loginStatus = http.StatusUnauthorized
	c2 := newClientWithCreds(t, srv2, "admin", "wrong")
	_, _, err = c2.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error = %v, want invalid credentials", err)
	}
}

func newClientWithCreds(t *testing.T, srv *fakeServer, username, password string) *Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	httpClient := &http.Client{Jar: jar}
	return NewClient(Config{
		AppName:    "testapp",
		BaseURL:    srv.srv.URL,
		APIKey:     srv.apiKey,
		Username:   username,
		Password:   password,
		HTTPClient: httpClient,
		Ops:        &fakeOps{httpClient: httpClient, baseURL: srv.srv.URL, apiKey: srv.apiKey},
	})
}

func TestGetAuthMethod_APIError(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.hostConfigStatus = http.StatusInternalServerError

	c := newTestClient(t, srv, false, nil)
	_, _, err := c.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to get auth method") {
		t.Errorf("error = %v, want failed to get auth method", err)
	}
}

// ---- Restore() ----

func TestRestore_Success_NoRestartRequired(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.restoreRestartRequired = false

	c := newTestClient(t, srv, false, nil)
	zipData := buildZip(t, map[string]string{"config.xml": "<Config/>"})
	if err := c.Restore(context.Background(), bytes.NewReader(zipData)); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	if !bytes.Equal(srv.restoreUploadedData, zipData) {
		t.Errorf("uploaded data mismatch")
	}
}

func TestRestore_RestartRequired_TriggersRestart(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.restoreRestartRequired = true

	c := newTestClient(t, srv, false, nil)
	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
}

func TestRestore_RestartFails(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.restoreRestartRequired = true
	srv.restartStatus = http.StatusInternalServerError

	c := newTestClient(t, srv, false, nil)
	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err == nil || !strings.Contains(err.Error(), "failed to restart") {
		t.Errorf("error = %v, want failed to restart", err)
	}
}

func TestRestore_UploadAPIError(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	srv.restoreUploadStatus = http.StatusInternalServerError

	c := newTestClient(t, srv, false, nil)
	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err == nil || !strings.Contains(err.Error(), "restore upload failed") {
		t.Errorf("error = %v, want restore upload failed", err)
	}
}

func TestRestore_PostgresDumpsButNoPostgresConfig(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	zipData := buildZip(t, map[string]string{
		"config.xml":     "<Config/>",
		"postgres/x.sql": "SELECT 1;",
	})

	c := newTestClient(t, srv, false, nil)
	err := c.Restore(context.Background(), bytes.NewReader(zipData))
	if err == nil || !strings.Contains(err.Error(), "no postgres settings") {
		t.Errorf("error = %v, want mention of missing postgres settings", err)
	}
}

// ---- restoreUpload URL versioning ----

// The restore-upload URL is built by hand (the generated upload operation
// takes no body), so Config.APIVersion must control its "/api/vN/" segment
// independent of whatever the Operations adapter uses internally.
func TestRestore_UsesConfiguredAPIVersion(t *testing.T) {
	srv := newFakeServer(t, "test-key")
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/system/backup/restore/upload", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"RestartRequired": false})
	})
	newSrv := httptest.NewServer(mux)
	defer newSrv.Close()

	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: jar}
	c := NewClient(Config{
		AppName:    "testapp",
		APIVersion: "v1",
		BaseURL:    newSrv.URL,
		APIKey:     srv.apiKey,
		HTTPClient: httpClient,
		Ops:        &fakeOps{httpClient: httpClient, baseURL: newSrv.URL, apiKey: srv.apiKey},
	})

	err := c.Restore(context.Background(), bytes.NewReader(buildZip(t, map[string]string{"config.xml": "<Config/>"})))
	if err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	if gotPath != "/api/v1/system/backup/restore/upload" {
		t.Errorf("restore upload path = %q, want /api/v1/system/backup/restore/upload", gotPath)
	}
}
