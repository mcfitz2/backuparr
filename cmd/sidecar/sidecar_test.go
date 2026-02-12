package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestHandlerBackup(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("backup me"), 0o644)

	cfg := &config{BackupPath: dir}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/backup", nil)
	handleBackup(cfg)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
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
