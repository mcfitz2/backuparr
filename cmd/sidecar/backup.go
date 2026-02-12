package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sqliteMagic is the first 16 bytes of every SQLite database file.
var sqliteMagic = []byte("SQLite format 3\000")

// isSQLiteFile checks whether the file at path is a SQLite database
// by reading its magic bytes header.
func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	header := make([]byte, 16)
	n, err := f.Read(header)
	if err != nil || n < 16 {
		return false
	}
	return bytes.Equal(header, sqliteMagic)
}

// safeCopySQLite creates a consistent copy of a SQLite database using
// the sqlite3 .backup command. This ensures the copy is not corrupted
// by in-progress writes or WAL transactions.
// Falls back to a direct file copy if sqlite3 is not available.
func safeCopySQLite(src, dst string) error {
	// Ensure destination directory exists
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Try sqlite3 .backup first for a consistent snapshot
	if sqlite3Path, err := exec.LookPath("sqlite3"); err == nil {
		cmd := exec.Command(sqlite3Path, src, fmt.Sprintf(".backup '%s'", dst))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			// If sqlite3 backup fails, fall back to direct copy
			log.Printf("[sidecar] Warning: sqlite3 .backup failed for %s (%v), falling back to direct copy", src, err)
			return directCopy(src, dst)
		}
		return nil
	}

	// sqlite3 not available — direct copy with warning
	log.Printf("[sidecar] Warning: sqlite3 not found, copying %s directly (may be inconsistent if app is writing)", src)
	return directCopy(src, dst)
}

// directCopy copies a file from src to dst.
func directCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// shouldExclude returns true if relPath matches any of the glob patterns.
func shouldExclude(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		// Match against the full relative path
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}

		// Also match against just the filename
		if matched, _ := filepath.Match(pattern, filepath.Base(relPath)); matched {
			return true
		}

		// Match directory prefix patterns like "cache/*"
		if strings.HasSuffix(pattern, "/*") {
			dir := strings.TrimSuffix(pattern, "/*")
			if strings.HasPrefix(relPath, dir+"/") || relPath == dir {
				return true
			}
		}
	}
	return false
}

// createBackup creates a ZIP backup of backupPath, writing it to w.
// SQLite databases are automatically detected and safely copied.
// Auxiliary SQLite files (-wal, -journal, -shm) are excluded since
// the .backup command produces a self-contained copy.
func createBackup(backupPath string, excludes []string, w io.Writer) (*backupStats, error) {
	backupPath = filepath.Clean(backupPath)

	// First pass: find all SQLite files so we can identify their auxiliary files
	sqliteFiles := map[string]bool{} // absolute paths of detected SQLite DBs
	err := filepath.Walk(backupPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if isSQLiteFile(path) {
			sqliteFiles[path] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan backup path: %w", err)
	}

	// Build set of auxiliary files to skip
	auxFiles := map[string]bool{}
	for sqlPath := range sqliteFiles {
		auxFiles[sqlPath+"-journal"] = true
		auxFiles[sqlPath+"-wal"] = true
		auxFiles[sqlPath+"-shm"] = true
	}

	// Create temp directory for SQLite safe copies
	tempDir, err := os.MkdirTemp("", "sidecar-backup-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Safe-copy all SQLite files
	for sqlPath := range sqliteFiles {
		relPath, _ := filepath.Rel(backupPath, sqlPath)
		tempPath := filepath.Join(tempDir, relPath)
		if err := safeCopySQLite(sqlPath, tempPath); err != nil {
			return nil, fmt.Errorf("failed to safe-copy SQLite %s: %w", relPath, err)
		}
		log.Printf("[sidecar] SQLite detected and safely copied: %s", relPath)
	}

	// Second pass: build the ZIP
	stats := &backupStats{}
	zw := zip.NewWriter(w)

	err = filepath.Walk(backupPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(backupPath, path)
		if relPath == "." {
			return nil
		}

		// Skip excluded paths
		if shouldExclude(relPath, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip SQLite auxiliary files
		if auxFiles[path] {
			return nil
		}

		// Handle directories
		if info.IsDir() {
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return fmt.Errorf("failed to create dir header for %s: %w", relPath, err)
			}
			header.Name = relPath + "/"
			_, err = zw.CreateHeader(header)
			return err
		}

		// Determine source: use safe copy for SQLite, original for everything else
		sourcePath := path
		if sqliteFiles[path] {
			sourcePath = filepath.Join(tempDir, relPath)
			stats.SQLiteFiles++
		}

		// Create ZIP entry preserving permissions
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("failed to create header for %s: %w", relPath, err)
		}
		header.Name = relPath
		header.Method = zip.Deflate

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("failed to create zip entry %s: %w", relPath, err)
		}

		f, err := os.Open(sourcePath)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", sourcePath, err)
		}
		defer f.Close()

		n, err := io.Copy(writer, f)
		if err != nil {
			return fmt.Errorf("failed to write %s: %w", relPath, err)
		}

		stats.TotalFiles++
		stats.TotalBytes += n

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize zip: %w", err)
	}

	return stats, nil
}

// backupStats holds metadata about a completed backup.
type backupStats struct {
	TotalFiles  int
	SQLiteFiles int
	TotalBytes  int64
}
