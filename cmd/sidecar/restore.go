package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// restoreFromZip extracts a ZIP archive into backupPath, overwriting existing files.
// Directory structure is preserved. File permissions from the ZIP are restored.
func restoreFromZip(backupPath string, zipData []byte) (*restoreStats, error) {
	backupPath = filepath.Clean(backupPath)

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}

	stats := &restoreStats{}

	for _, file := range reader.File {
		destPath := filepath.Join(backupPath, file.Name)

		// Security: prevent zip slip (path traversal)
		if !strings.HasPrefix(filepath.Clean(destPath), backupPath+string(os.PathSeparator)) && filepath.Clean(destPath) != backupPath {
			log.Printf("[sidecar] Warning: skipping potentially unsafe path: %s", file.Name)
			continue
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, file.Mode()); err != nil {
				return nil, fmt.Errorf("failed to create directory %s: %w", file.Name, err)
			}
			continue
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return nil, fmt.Errorf("failed to create parent dir for %s: %w", file.Name, err)
		}

		if err := extractFile(file, destPath); err != nil {
			return nil, fmt.Errorf("failed to extract %s: %w", file.Name, err)
		}

		stats.FilesRestored++
		stats.BytesRestored += int64(file.UncompressedSize64)
	}

	return stats, nil
}

// extractFile extracts a single file from the ZIP to destPath.
func extractFile(file *zip.File, destPath string) error {
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		return err
	}

	return nil
}

// restoreStats holds metadata about a completed restore.
type restoreStats struct {
	FilesRestored int
	BytesRestored int64
}
