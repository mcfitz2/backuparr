package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SQLite detection
// ---------------------------------------------------------------------------

func TestIsSQLiteFile(t *testing.T) {
	dir := t.TempDir()

	// Create a file with SQLite magic bytes
	sqliteFile := filepath.Join(dir, "test.db")
	data := make([]byte, 100)
	copy(data, []byte("SQLite format 3\000"))
	os.WriteFile(sqliteFile, data, 0o644)

	// A plain text file
	textFile := filepath.Join(dir, "test.txt")
	os.WriteFile(textFile, []byte("hello world"), 0o644)

	// A file too small to have the magic header
	tinyFile := filepath.Join(dir, "tiny")
	os.WriteFile(tinyFile, []byte("hi"), 0o644)

	tests := []struct {
		path string
		want bool
	}{
		{sqliteFile, true},
		{textFile, false},
		{tinyFile, false},
		{filepath.Join(dir, "nonexistent"), false},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.path), func(t *testing.T) {
			if got := isSQLiteFile(tt.path); got != tt.want {
				t.Errorf("isSQLiteFile(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Exclude patterns
// ---------------------------------------------------------------------------

func TestShouldExclude(t *testing.T) {
	tests := []struct {
		relPath  string
		patterns []string
		want     bool
	}{
		{"debug.log", []string{"*.log"}, true},
		{"config.xml", []string{"*.log"}, false},
		{"cache/data.bin", []string{"cache/*"}, true},
		{"cache", []string{"cache/*"}, true},
		{"other/file", []string{"cache/*"}, false},
		{"temp.log", []string{"*.tmp", "*.log"}, true},
		{"keep.txt", []string{"*.tmp", "*.log"}, false},
		{"sub/debug.log", []string{"*.log"}, true},
		{"anything", nil, false},
		{"anything", []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			if got := shouldExclude(tt.relPath, tt.patterns); got != tt.want {
				t.Errorf("shouldExclude(%q, %v) = %v, want %v", tt.relPath, tt.patterns, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Backup creation
// ---------------------------------------------------------------------------

func TestCreateBackup(t *testing.T) {
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(dir, "config.xml"), []byte("<config/>"), 0o644)
	os.WriteFile(filepath.Join(dir, "subdir", "data.txt"), []byte("some data"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.log"), []byte("log entry"), 0o644)

	// Fake SQLite file (magic header)
	sqliteData := make([]byte, 100)
	copy(sqliteData, []byte("SQLite format 3\000"))
	os.WriteFile(filepath.Join(dir, "app.db"), sqliteData, 0o644)

	// Auxiliary files that should be auto-skipped
	os.WriteFile(filepath.Join(dir, "app.db-wal"), []byte("wal data"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.db-shm"), []byte("shm data"), 0o644)

	var buf bytes.Buffer
	stats, err := createBackup(dir, []string{"*.log"}, &buf)
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}

	if stats.TotalFiles != 3 { // config.xml, data.txt, app.db
		t.Errorf("TotalFiles = %d, want 3", stats.TotalFiles)
	}
	if stats.SQLiteFiles != 1 {
		t.Errorf("SQLiteFiles = %d, want 1", stats.SQLiteFiles)
	}

	// Verify ZIP contents
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}

	entries := map[string]bool{}
	for _, f := range zr.File {
		entries[f.Name] = true
	}

	for _, want := range []string{"config.xml", "subdir/data.txt", "app.db"} {
		if !entries[want] {
			t.Errorf("ZIP missing expected entry: %s (have: %v)", want, entries)
		}
	}
	for _, notWant := range []string{"app.log", "app.db-wal", "app.db-shm"} {
		if entries[notWant] {
			t.Errorf("ZIP should not contain: %s", notWant)
		}
	}
}

func TestCreateBackup_EmptyDir(t *testing.T) {
	var buf bytes.Buffer
	stats, err := createBackup(t.TempDir(), nil, &buf)
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}
	if stats.TotalFiles != 0 {
		t.Errorf("TotalFiles = %d, want 0", stats.TotalFiles)
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

func TestRestoreFromZip(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("config.xml")
	w.Write([]byte("<config>restored</config>"))
	w, _ = zw.Create("subdir/data.txt")
	w.Write([]byte("restored data"))
	zw.Close()

	destDir := t.TempDir()
	stats, err := restoreFromZip(destDir, zipBuf.Bytes())
	if err != nil {
		t.Fatalf("restoreFromZip: %v", err)
	}
	if stats.FilesRestored != 2 {
		t.Errorf("FilesRestored = %d, want 2", stats.FilesRestored)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "config.xml"))
	if err != nil {
		t.Fatalf("ReadFile config.xml: %v", err)
	}
	if string(data) != "<config>restored</config>" {
		t.Errorf("config.xml = %q", data)
	}

	data, err = os.ReadFile(filepath.Join(destDir, "subdir", "data.txt"))
	if err != nil {
		t.Fatalf("ReadFile subdir/data.txt: %v", err)
	}
	if string(data) != "restored data" {
		t.Errorf("subdir/data.txt = %q", data)
	}
}

func TestRestoreFromZip_Overwrite(t *testing.T) {
	destDir := t.TempDir()
	os.WriteFile(filepath.Join(destDir, "config.xml"), []byte("old"), 0o644)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("config.xml")
	w.Write([]byte("new"))
	zw.Close()

	_, err := restoreFromZip(destDir, zipBuf.Bytes())
	if err != nil {
		t.Fatalf("restoreFromZip: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(destDir, "config.xml"))
	if string(data) != "new" {
		t.Errorf("expected overwritten content, got %q", data)
	}
}

func TestRestoreFromZip_ZipSlip(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	hdr := &zip.FileHeader{Name: "../../../etc/passwd"}
	w, _ := zw.CreateHeader(hdr)
	w.Write([]byte("malicious"))
	zw.Close()

	destDir := t.TempDir()
	stats, err := restoreFromZip(destDir, zipBuf.Bytes())
	if err != nil {
		t.Fatalf("should not error on zip slip (skips file): %v", err)
	}
	if stats.FilesRestored != 0 {
		t.Errorf("FilesRestored = %d, want 0 (malicious entry should be skipped)", stats.FilesRestored)
	}
}

// ---------------------------------------------------------------------------
// Roundtrip (backup → restore)
// ---------------------------------------------------------------------------

func TestBackupRestore_Roundtrip(t *testing.T) {
	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("file a"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "b.txt"), []byte("file b"), 0o644)

	var buf bytes.Buffer
	_, err := createBackup(srcDir, nil, &buf)
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}

	dstDir := t.TempDir()
	_, err = restoreFromZip(dstDir, buf.Bytes())
	if err != nil {
		t.Fatalf("restoreFromZip: %v", err)
	}

	for _, rel := range []string{"a.txt", filepath.Join("sub", "b.txt")} {
		orig, _ := os.ReadFile(filepath.Join(srcDir, rel))
		restored, err := os.ReadFile(filepath.Join(dstDir, rel))
		if err != nil {
			t.Errorf("missing restored file %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(orig, restored) {
			t.Errorf("content mismatch for %s", rel)
		}
	}
}

// ---------------------------------------------------------------------------
// Direct copy helper
// ---------------------------------------------------------------------------

func TestDirectCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("copy me"), 0o644)

	if err := directCopy(src, dst); err != nil {
		t.Fatalf("directCopy: %v", err)
	}

	data, _ := os.ReadFile(dst)
	if string(data) != "copy me" {
		t.Errorf("got %q, want %q", data, "copy me")
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func TestHandlerHealth(t *testing.T) {
	cfg := &config{
		BackupPath:      t.TempDir(),
		DockerContainer: "myapp",
	}

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/health", nil)
	handleHealth(cfg)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
}

// leftoverBackupTempFiles counts files matching the sidecar's backup temp
// file pattern still sitting in the OS temp dir, so tests can assert the
// handler cleans up after itself on both the success and failure paths.
func leftoverBackupTempFiles(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "sidecar-backup-*.zip"))
	if err != nil {
		t.Fatalf("glob temp dir: %v", err)
	}
	return len(matches)
}

func TestHandlerBackup(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("backup me"), 0o644)

	before := leftoverBackupTempFiles(t)

	cfg := &config{BackupPath: dir}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/backup", nil)
	handleBackup(cfg)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	body := rr.Body.Bytes()

	wantLen := strconv.Itoa(len(body))
	if got := rr.Header().Get("Content-Length"); got != wantLen {
		t.Errorf("Content-Length = %q, want %q", got, wantLen)
	}

	wantSum := sha256.Sum256(body)
	wantSumHex := hex.EncodeToString(wantSum[:])
	if got := rr.Header().Get("X-Backup-Sha256"); got != wantSumHex {
		t.Errorf("X-Backup-Sha256 = %q, want %q (sha256 of the actual response body)", got, wantSumHex)
	}

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("invalid ZIP: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "test.txt" {
		t.Errorf("unexpected ZIP entries: %v", zr.File)
	}

	rc, _ := zr.File[0].Open()
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "backup me" {
		t.Errorf("content = %q", data)
	}

	if got := leftoverBackupTempFiles(t); got != before {
		t.Errorf("leftover backup temp files = %d, want %d (no leak on success path)", got, before)
	}
}

// TestHandlerBackup_CreateBackupFailsReturnsError verifies that when the
// archive build fails, the handler returns a real error status rather than
// a 200 OK with an empty or truncated body. Prior to building the archive
// in a temp file before writing any header, this test fails: the handler
// wrote Content-Type/Content-Disposition (committing a 200) before calling
// createBackup, so a mid-build failure surfaced as HTTP 200 with a
// zero-length body.
func TestHandlerBackup_CreateBackupFailsReturnsError(t *testing.T) {
	before := leftoverBackupTempFiles(t)

	// A BackupPath that doesn't exist makes createBackup fail while walking
	// the directory tree, before any bytes are written.
	cfg := &config{BackupPath: filepath.Join(t.TempDir(), "does-not-exist")}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/backup", nil)
	handleBackup(cfg)(rr, req)

	if rr.Code < 500 || rr.Code >= 600 {
		t.Fatalf("status = %d, want 5xx", rr.Code)
	}
	if rr.Body.Len() == 0 {
		t.Error("expected a non-empty error body, got zero-length body")
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("error response is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}
	if resp["success"] != false {
		t.Errorf(`response["success"] = %v, want false`, resp["success"])
	}

	if got := leftoverBackupTempFiles(t); got != before {
		t.Errorf("leftover backup temp files = %d, want %d (no leak on failure path)", got, before)
	}
}

func TestHandlerBackup_MethodNotAllowed(t *testing.T) {
	cfg := &config{BackupPath: t.TempDir()}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/backup", nil)
	handleBackup(cfg)(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestAuthMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no key configured passes through", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		authMiddleware("", inner)(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rr.Code)
		}
	})

	t.Run("valid key", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Api-Key", "secret")
		authMiddleware("secret", inner)(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rr.Code)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Api-Key", "wrong")
		authMiddleware("secret", inner)(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		authMiddleware("secret", inner)(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rr.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// Restart (unit-testable parts)
// ---------------------------------------------------------------------------

func TestTryRestart_NoneConfigured(t *testing.T) {
	cfg := &config{BackupPath: t.TempDir()}
	result := tryRestart(cfg)
	if result.Attempted {
		t.Error("should not attempt restart when nothing is configured")
	}
}

// ---------------------------------------------------------------------------
// loadConfig — API key requirement
// ---------------------------------------------------------------------------

// withEnv sets env vars for the duration of the test and restores the
// previous values (or absence) afterward.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

func TestLoadConfig_RequiresAPIKeyByDefault(t *testing.T) {
	withEnv(t, map[string]string{
		"BACKUP_PATH":   t.TempDir(),
		"API_KEY":       "",
		"ALLOW_NO_AUTH": "",
	})

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() succeeded with no API_KEY and no ALLOW_NO_AUTH opt-in, want error")
	}
	if !strings.Contains(err.Error(), "API_KEY") {
		t.Errorf("error = %q, want mention of API_KEY", err)
	}
}

func TestLoadConfig_AllowNoAuthOptIn(t *testing.T) {
	withEnv(t, map[string]string{
		"BACKUP_PATH":   t.TempDir(),
		"API_KEY":       "",
		"ALLOW_NO_AUTH": "1",
	})

	var logBuf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	})

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() with ALLOW_NO_AUTH=1 failed: %v", err)
	}
	if !cfg.AllowNoAuth {
		t.Error("AllowNoAuth = false, want true")
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.APIKey)
	}
	if !strings.Contains(strings.ToUpper(logBuf.String()), "WARNING") {
		t.Errorf("expected a startup warning to be logged, got: %q", logBuf.String())
	}
}

func TestLoadConfig_WithAPIKeySucceeds(t *testing.T) {
	withEnv(t, map[string]string{
		"BACKUP_PATH":   t.TempDir(),
		"API_KEY":       "dummy-test-key",
		"ALLOW_NO_AUTH": "",
	})

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() with API_KEY set failed: %v", err)
	}
	if cfg.APIKey != "dummy-test-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "dummy-test-key")
	}
}

// ---------------------------------------------------------------------------
// /restore — auth enforcement
// ---------------------------------------------------------------------------

// newRestoreRequest builds a multipart POST to /api/v1/restore carrying a
// minimal (empty) ZIP as the "backup" field.
func newRestoreRequest(t *testing.T, apiKey string) *http.Request {
	t.Helper()

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("backup", "backup.zip")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(zipBuf.Bytes()); err != nil {
		t.Fatalf("write zip part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/restore", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	return req
}

func TestHandlerRestore_Unauthorized(t *testing.T) {
	cfg := &config{BackupPath: t.TempDir()}
	handler := authMiddleware("supersecretkey", handleRestore(cfg))

	rr := httptest.NewRecorder()
	req := newRestoreRequest(t, "")
	handler(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestHandlerRestore_WithValidKey(t *testing.T) {
	cfg := &config{BackupPath: t.TempDir()}
	handler := authMiddleware("supersecretkey", handleRestore(cfg))

	rr := httptest.NewRecorder()
	req := newRestoreRequest(t, "supersecretkey")
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRestore_WrongKeySameLength(t *testing.T) {
	cfg := &config{BackupPath: t.TempDir()}
	// "supersecretkey" and "zzzzzzzzzzzzzz" are both 14 bytes.
	handler := authMiddleware("supersecretkey", handleRestore(cfg))

	rr := httptest.NewRecorder()
	req := newRestoreRequest(t, "zzzzzzzzzzzzzz")
	handler(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAuthMiddleware_WrongKeySameLength(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Api-Key", "zzzzzz") // same length as "secret"
	authMiddleware("secret", inner)(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}
