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

// safeCopySQLite creates a consistent copy of a SQLite database using the
// sqlite3 .backup command. This ensures the copy is not corrupted by
// in-progress writes or WAL transactions.
//
// If sqlite3 is unavailable, it falls back to a direct file copy of the live
// database and reports fallback=true so the caller can reflect that in
// backup stats instead of claiming a consistent snapshot was taken. If
// sqlite3 IS available but the .backup command itself fails, that is treated
// as a hard error rather than a silent downgrade: a copy of a live database
// taken without .backup is not guaranteed restorable, and returning success
// anyway would make a backup that can't be trusted look identical to one
// that can.
func safeCopySQLite(src, dst string) (fallback bool, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("failed to create temp dir: %w", err)
	}

	sqlite3Path, lookErr := exec.LookPath("sqlite3")
	if lookErr != nil {
		log.Printf("[sidecar] Warning: sqlite3 not found, copying %s directly (may be inconsistent if app is writing)", src)
		if err := directCopy(src, dst); err != nil {
			return false, err
		}
		return true, nil
	}

	// sqlite3's `.backup` dot-command uses its own ad-hoc argument tokenizer,
	// not full SQL string escaping: a destination wrapped in single quotes
	// that itself contains a single quote cannot be escaped (doubling it, or
	// switching to double quotes, does not round-trip either). Sidestep the
	// whole problem by having sqlite3 write to a scratch path we generate
	// ourselves — guaranteed free of quote characters — then move the result
	// into place with a plain rename, which has no quoting concerns at all.
	scratchDir, err := os.MkdirTemp("", "sqlite3-backup-*")
	if err != nil {
		return false, fmt.Errorf("failed to create scratch dir: %w", err)
	}
	defer os.RemoveAll(scratchDir)
	scratchDst := filepath.Join(scratchDir, "backup.db")

	cmd := exec.Command(sqlite3Path, src, fmt.Sprintf(".backup '%s'", scratchDst))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("sqlite3 .backup failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	if err := os.Rename(scratchDst, dst); err != nil {
		if err := directCopy(scratchDst, dst); err != nil {
			return false, fmt.Errorf("failed to move sqlite3 backup into place: %w", err)
		}
	}
	return false, nil
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
		// A symlink is never dereferenced for SQLite detection; it is
		// skipped outright when building the archive below.
		if info.Mode()&os.ModeSymlink != 0 {
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
	fallbackFiles := map[string]bool{} // absolute paths copied directly instead of via `.backup`
	for sqlPath := range sqliteFiles {
		relPath, _ := filepath.Rel(backupPath, sqlPath)
		tempPath := filepath.Join(tempDir, relPath)
		fallback, err := safeCopySQLite(sqlPath, tempPath)
		if err != nil {
			return nil, fmt.Errorf("failed to safe-copy SQLite %s: %w", relPath, err)
		}
		if fallback {
			fallbackFiles[sqlPath] = true
			log.Printf("[sidecar] SQLite copied directly (not a consistent snapshot): %s", relPath)
		} else {
			log.Printf("[sidecar] SQLite detected and safely copied: %s", relPath)
		}
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

		// Symlinks are skipped rather than archived: Walk uses Lstat, so
		// info here describes the link itself, but opening sourcePath below
		// would follow it and copy whatever it points at (possibly outside
		// backupPath) under the link's name. Recording it as a symlink entry
		// instead isn't safe either without restore-side support to restore
		// it as one, which is tracked separately.
		if info.Mode()&os.ModeSymlink != 0 {
			log.Printf("[sidecar] Skipping symlink (not preserved in backup): %s", relPath)
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
			if fallbackFiles[path] {
				stats.SQLiteFallback++
			}
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
	// SQLiteFallback counts SQLite files among SQLiteFiles that were copied
	// directly rather than via `sqlite3 .backup`, because sqlite3 was not
	// found on the system. These are not guaranteed-consistent snapshots.
	SQLiteFallback int
	TotalBytes     int64
}
