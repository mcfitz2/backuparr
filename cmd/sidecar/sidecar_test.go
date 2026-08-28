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
	"os/exec"
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

// requireSQLite3 skips the test if the sqlite3 CLI isn't on PATH, and
// returns its path. safeCopySQLite's `.backup` path (success, forced
// failure, and quoting) can only be exercised with the real binary.
func requireSQLite3(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	return path
}

// writeRealSQLiteDB creates a genuine (not just magic-header-stubbed)
// SQLite database at path, so that `sqlite3 .backup` succeeds against it.
func writeRealSQLiteDB(t *testing.T, sqlite3Path, path string) {
	t.Helper()
	cmd := exec.Command(sqlite3Path, path, "CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (1);")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create real sqlite3 fixture: %v (%s)", err, out)
	}
}

func TestCreateBackup(t *testing.T) {
	sqlite3Path := requireSQLite3(t)
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(dir, "config.xml"), []byte("<config/>"), 0o644)
	os.WriteFile(filepath.Join(dir, "subdir", "data.txt"), []byte("some data"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.log"), []byte("log entry"), 0o644)

	// A real SQLite database, so that `.backup` succeeds against it rather
	// than falling back or hard-failing.
	writeRealSQLiteDB(t, sqlite3Path, filepath.Join(dir, "app.db"))

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
	if stats.SQLiteFallback != 0 {
		t.Errorf("SQLiteFallback = %d, want 0 (a real db backed up via `.backup` is not a fallback)", stats.SQLiteFallback)
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

// TestCreateBackup_SkipsSymlink verifies that a symlink in the backup tree
// is neither dereferenced into the archive nor left to silently replace a
// real file. Before the fix, filepath.Walk's Lstat-based info fell through
// to the file-copy branch, and os.Open(sourcePath) followed the link,
// copying the target's bytes into the archive under the link's name.
func TestCreateBackup_SkipsSymlink(t *testing.T) {
	dir := t.TempDir()

	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	os.WriteFile(secretPath, []byte("outside-the-backup-root"), 0o644)

	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real content"), 0o644)
	linkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink(secretPath, linkPath); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	var logBuf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOutput) })

	var buf bytes.Buffer
	stats, err := createBackup(dir, nil, &buf)
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}
	if stats.TotalFiles != 1 {
		t.Errorf("TotalFiles = %d, want 1 (only real.txt; symlink skipped)", stats.TotalFiles)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == "link.txt" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			t.Fatalf("archive contains symlink entry %q with content %q; want it absent", f.Name, data)
		}
	}

	if !strings.Contains(logBuf.String(), "link.txt") {
		t.Errorf("expected symlink skip to be logged, got: %q", logBuf.String())
	}
}

// TestCreateBackup_SQLiteBackupFailureIsHardError verifies that when sqlite3
// is present but `.backup` fails against a corrupt/invalid database, the
// whole backup fails loudly (rather than silently falling back to copying
// the live file while still reporting SQLiteFiles as if a safe snapshot had
// been taken), and that the captured stderr is part of the error.
func TestCreateBackup_SQLiteBackupFailureIsHardError(t *testing.T) {
	requireSQLite3(t)
	dir := t.TempDir()

	// Magic header only — not a real database, so `.backup` fails against it.
	sqliteData := make([]byte, 100)
	copy(sqliteData, []byte("SQLite format 3\000"))
	os.WriteFile(filepath.Join(dir, "corrupt.db"), sqliteData, 0o644)

	var buf bytes.Buffer
	stats, err := createBackup(dir, nil, &buf)
	if err == nil {
		t.Fatalf("createBackup succeeded (stats: %+v), want error since `.backup` cannot produce a consistent snapshot of a corrupt database", stats)
	}
	if !strings.Contains(err.Error(), "not a database") {
		t.Errorf("error = %q, want it to include the sqlite3 stderr (\"not a database\")", err.Error())
	}
}

// TestHandlerBackup_SQLiteBackupFailureLogsStderr drives the same forced
// failure through the HTTP handler and asserts the captured stderr reaches
// the server log, and that the response honestly reports failure instead of
// a 200 with stats implying a safe snapshot was taken.
func TestHandlerBackup_SQLiteBackupFailureLogsStderr(t *testing.T) {
	requireSQLite3(t)
	dir := t.TempDir()

	sqliteData := make([]byte, 100)
	copy(sqliteData, []byte("SQLite format 3\000"))
	os.WriteFile(filepath.Join(dir, "corrupt.db"), sqliteData, 0o644)

	var logBuf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOutput) })

	cfg := &config{BackupPath: dir}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/backup", nil)
	handleBackup(cfg)(rr, req)

	if rr.Code < 500 || rr.Code >= 600 {
		t.Fatalf("status = %d, want 5xx", rr.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("error response is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}
	if resp["success"] != false {
		t.Errorf(`response["success"] = %v, want false`, resp["success"])
	}

	if !strings.Contains(logBuf.String(), "not a database") {
		t.Errorf("expected sqlite3 stderr to reach the log, got: %q", logBuf.String())
	}
}

// TestCreateBackup_SQLitePathWithSingleQuote exercises a source tree whose
// SQLite file lives under a path containing a single quote. Before the fix,
// the `.backup 'DEST'` argument couldn't represent a destination containing
// a quote (sqlite3's dot-command tokenizer has no working escape for it),
// so `.backup` would fail to open the malformed path.
func TestCreateBackup_SQLitePathWithSingleQuote(t *testing.T) {
	sqlite3Path := requireSQLite3(t)
	dir := t.TempDir()

	subdir := filepath.Join(dir, "app's data")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(subdir, "app's.db")
	writeRealSQLiteDB(t, sqlite3Path, dbPath)

	var buf bytes.Buffer
	stats, err := createBackup(dir, nil, &buf)
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}
	if stats.SQLiteFiles != 1 {
		t.Errorf("SQLiteFiles = %d, want 1", stats.SQLiteFiles)
	}
	if stats.SQLiteFallback != 0 {
		t.Errorf("SQLiteFallback = %d, want 0", stats.SQLiteFallback)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	wantName := "app's data/app's.db"
	var found *zip.File
	for _, f := range zr.File {
		if f.Name == wantName {
			found = f
		}
	}
	if found == nil {
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		t.Fatalf("ZIP missing entry %q (have: %v)", wantName, names)
	}

	rc, err := found.Open()
	if err != nil {
		t.Fatalf("open zip entry: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(data[:len(sqliteMagic)], sqliteMagic) {
		t.Errorf("zip entry %q does not look like a SQLite backup", wantName)
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
// Restore extraction hardening (archive modes, symlinks, size cap, rollback)
// ---------------------------------------------------------------------------

func TestRestore_StripsSetuidBit(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	hdr := &zip.FileHeader{Name: "evil.sh", Method: zip.Deflate}
	hdr.SetMode(0o755 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	w.Write([]byte("#!/bin/sh\necho pwned\n"))
	zw.Close()

	destDir := t.TempDir()
	if _, err := restoreFromZip(destDir, zipBuf.Bytes()); err != nil {
		t.Fatalf("restoreFromZip: %v", err)
	}

	fi, err := os.Stat(filepath.Join(destDir, "evil.sh"))
	if err != nil {
		t.Fatalf("Stat evil.sh: %v", err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("extracted file retained a special mode bit: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("extracted file perm = %o, want 0644", perm)
	}
}

func TestRestore_StripsSetuidBitOnDirectory(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	hdr := &zip.FileHeader{Name: "evildir/", Method: zip.Store}
	hdr.SetMode(os.ModeDir | 0o755 | os.ModeSetgid | os.ModeSticky)
	_, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	zw.Close()

	destDir := t.TempDir()
	if _, err := restoreFromZip(destDir, zipBuf.Bytes()); err != nil {
		t.Fatalf("restoreFromZip: %v", err)
	}

	fi, err := os.Stat(filepath.Join(destDir, "evildir"))
	if err != nil {
		t.Fatalf("Stat evildir: %v", err)
	}
	if fi.Mode()&(os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("extracted directory retained a special mode bit: %v", fi.Mode())
	}
}

func TestRestore_SymlinkDestinationNotFollowed(t *testing.T) {
	backupDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("original"), 0o644); err != nil {
		t.Fatalf("WriteFile outsideFile: %v", err)
	}

	linkPath := filepath.Join(backupDir, "config.xml")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("config.xml")
	w.Write([]byte("malicious overwrite"))
	zw.Close()

	if _, err := restoreFromZip(backupDir, zipBuf.Bytes()); err != nil {
		t.Fatalf("restoreFromZip: %v", err)
	}

	data, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("ReadFile outsideFile: %v", err)
	}
	if string(data) != "original" {
		t.Errorf("outside target was modified: got %q, want %q", data, "original")
	}

	restored, err := os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("ReadFile linkPath: %v", err)
	}
	if string(restored) != "malicious overwrite" {
		t.Errorf("config.xml = %q, want the restored content", restored)
	}
	if fi, err := os.Lstat(linkPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("config.xml is still a symlink after restore")
	}
}

func TestRestore_SizeCapExceeded(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("big.bin")
	w.Write(bytes.Repeat([]byte("A"), 64))
	zw.Close()

	destDir := t.TempDir()
	_, err := restoreFromZipWithLimit(destDir, zipBuf.Bytes(), 10)
	if err == nil {
		t.Fatal("expected an error when uncompressed size exceeds the cap, got nil")
	}

	if _, statErr := os.Stat(filepath.Join(destDir, "big.bin")); statErr == nil {
		t.Errorf("big.bin should not exist in destDir after a capped restore failure")
	}
}

// TestRestore_FailurePartwayLeavesOriginalIntact forces extraction to fail on
// the second of two entries (a corrupted CRC32, detected by archive/zip
// itself while reading, independent of any size cap) and asserts the
// original destination directory is completely unaffected. This uses only
// restoreFromZip (not the WithLimit test seam) so the same test can be run
// against the pre-fix implementation to demonstrate the in-place-overwrite
// bug it exercises.
func TestRestore_FailurePartwayLeavesOriginalIntact(t *testing.T) {
	destDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(destDir, "config.xml"), []byte("original config"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "untouched.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("config.xml")
	w.Write([]byte("new config that should never land"))

	corrupt := []byte("this entry has a deliberately wrong CRC32")
	hdr := &zip.FileHeader{Name: "corrupt.bin", Method: zip.Store}
	hdr.CompressedSize64 = uint64(len(corrupt))
	hdr.UncompressedSize64 = uint64(len(corrupt))
	hdr.CRC32 = 0x11111111 // deliberately wrong, triggers zip.ErrChecksum on read
	cw, err := zw.CreateRaw(hdr)
	if err != nil {
		t.Fatalf("CreateRaw: %v", err)
	}
	cw.Write(corrupt)
	zw.Close()

	_, err = restoreFromZip(destDir, zipBuf.Bytes())
	if err == nil {
		t.Fatal("expected an error from the corrupted second entry, got nil")
	}

	data, err := os.ReadFile(filepath.Join(destDir, "config.xml"))
	if err != nil {
		t.Fatalf("ReadFile config.xml: %v", err)
	}
	if string(data) != "original config" {
		t.Errorf("config.xml was modified despite a failed restore: got %q", data)
	}

	data, err = os.ReadFile(filepath.Join(destDir, "untouched.txt"))
	if err != nil {
		t.Fatalf("ReadFile untouched.txt: %v", err)
	}
	if string(data) != "keep me" {
		t.Errorf("untouched.txt was modified despite a failed restore: got %q", data)
	}

	// No staging leftovers should remain in the destination directory.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir destDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), restoreStagingPrefix) {
			t.Errorf("leftover staging directory found after failed restore: %s", e.Name())
		}
	}
}

func TestRestore_NormalArchiveStillWorks(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	dirHdr := &zip.FileHeader{Name: "nested/deeper/", Method: zip.Store}
	dirHdr.SetMode(os.ModeDir | 0o755)
	if _, err := zw.CreateHeader(dirHdr); err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	w, _ := zw.Create("config.xml")
	w.Write([]byte("<config>ok</config>"))
	w, _ = zw.Create("nested/deeper/data.txt")
	w.Write([]byte("deep data"))
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
	if err != nil || string(data) != "<config>ok</config>" {
		t.Errorf("config.xml = %q, err = %v", data, err)
	}

	data, err = os.ReadFile(filepath.Join(destDir, "nested", "deeper", "data.txt"))
	if err != nil || string(data) != "deep data" {
		t.Errorf("nested/deeper/data.txt = %q, err = %v", data, err)
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
