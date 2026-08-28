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

// maxRestoreUncompressedBytes bounds the total uncompressed bytes a single
// restore may write, regardless of what the zip headers claim. The upload
// itself is already capped at 2 GiB compressed (see handleRestore); this
// cap guards against a highly compressible archive (zip bomb) expanding
// far beyond that and filling the volume. 20 GiB gives generous headroom
// for legitimate *arr configs (including large SQLite databases) while
// still bounding worst-case disk usage from a malicious or corrupt archive.
const maxRestoreUncompressedBytes int64 = 20 << 30

// restoreStagingPrefix names the temporary directory extraction is staged
// into before being swapped into backupPath. It is created as a child of
// backupPath (not a sibling of it) because backupPath is typically a
// container volume mount point: renaming a mount point itself fails on
// Linux (EBUSY), but ordinary renames of entries *inside* it work fine and
// stay on the same filesystem, which os.Rename requires.
const restoreStagingPrefix = ".sidecar-restore-staging-"

// restoreFromZip extracts a ZIP archive into backupPath, overwriting existing
// top-level entries. Directory structure is preserved. File permissions from
// the ZIP are restored, with setuid/setgid/sticky bits and any bits beyond
// the usual rwx stripped.
//
// Extraction is staged into a temporary directory inside backupPath and
// swapped into place only after the whole archive has been extracted
// successfully, so a failed or aborted restore (bad archive, size cap
// exceeded, disk full, etc.) leaves backupPath untouched.
func restoreFromZip(backupPath string, zipData []byte) (*restoreStats, error) {
	return restoreFromZipWithLimit(backupPath, zipData, maxRestoreUncompressedBytes)
}

// restoreFromZipWithLimit is restoreFromZip with the uncompressed-size cap
// as a parameter, so tests can exercise cap enforcement without extracting
// gigabytes of data. Production code always goes through restoreFromZip.
func restoreFromZipWithLimit(backupPath string, zipData []byte, maxUncompressedBytes int64) (*restoreStats, error) {
	backupPath = filepath.Clean(backupPath)

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}

	if err := os.MkdirAll(backupPath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create backup path %s: %w", backupPath, err)
	}

	stagingDir, err := os.MkdirTemp(backupPath, restoreStagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create restore staging directory: %w", err)
	}
	stagingIntact := true
	defer func() {
		if stagingIntact {
			os.RemoveAll(stagingDir)
		}
	}()

	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open restore staging root: %w", err)
	}
	defer root.Close()

	stats := &restoreStats{}
	var totalBytes int64

	for _, file := range reader.File {
		destPath := filepath.Join(stagingDir, file.Name)

		// Security: prevent zip slip (path traversal). os.Root below also
		// rejects escapes (including through symlinks), but this lexical
		// check is cheap, correct, and catches the common case up front.
		if !strings.HasPrefix(destPath, stagingDir+string(os.PathSeparator)) && destPath != stagingDir {
			log.Printf("[sidecar] Warning: skipping potentially unsafe path: %s", file.Name)
			continue
		}

		// Derive the root-relative name from the validated, joined destPath
		// rather than file.Name directly: file.Name may look absolute (e.g.
		// "/etc/passwd") or contain other oddities that filepath.Join already
		// resolved safely into destPath, but os.Root methods take names
		// relative to the root and must not be handed the raw entry name.
		relPath, err := filepath.Rel(stagingDir, destPath)
		if err != nil {
			log.Printf("[sidecar] Warning: skipping unresolvable path: %s", file.Name)
			continue
		}

		if file.FileInfo().IsDir() {
			mode := file.Mode().Perm() & 0o755
			if err := root.MkdirAll(relPath, mode); err != nil {
				return nil, fmt.Errorf("failed to create directory %s: %w", file.Name, err)
			}
			continue
		}

		if err := root.MkdirAll(filepath.Dir(relPath), 0o755); err != nil {
			return nil, fmt.Errorf("failed to create parent dir for %s: %w", file.Name, err)
		}

		n, err := extractFile(root, relPath, file, maxUncompressedBytes-totalBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to extract %s: %w", file.Name, err)
		}

		totalBytes += n
		stats.FilesRestored++
		stats.BytesRestored += n
	}

	if err := swapStagingIntoPlace(backupPath, stagingDir); err != nil {
		return nil, err
	}
	stagingIntact = false

	return stats, nil
}

// extractFile extracts a single file from the ZIP into root at relPath,
// masking archive-supplied setuid/setgid/sticky bits and capping the number
// of bytes copied at remaining. It returns the number of bytes written.
//
// The cap is enforced against bytes actually copied, not the zip header's
// (untrusted, possibly forged) UncompressedSize64.
func extractFile(root *os.Root, relPath string, file *zip.File, remaining int64) (int64, error) {
	rc, err := file.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	mode := file.Mode().Perm() & 0o644
	out, err := root.OpenFile(relPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	n, err := io.Copy(out, io.LimitReader(rc, remaining+1))
	if err != nil {
		return n, err
	}
	if n > remaining {
		return n, fmt.Errorf("archive exceeds uncompressed size cap (%d bytes remaining in budget)", remaining)
	}

	return n, nil
}

// swapStagingIntoPlace moves each top-level entry extracted into stagingDir
// (a child of backupPath) over the corresponding entry directly under
// backupPath, replacing whatever was there. Because stagingDir lives inside
// backupPath, both sides of each rename are on the same filesystem, so the
// rename is atomic even when backupPath itself is a mount point that cannot
// be renamed or replaced as a whole.
func swapStagingIntoPlace(backupPath, stagingDir string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return fmt.Errorf("failed to read restore staging directory: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		oldPath := filepath.Join(backupPath, name)
		newPath := filepath.Join(stagingDir, name)

		if err := os.RemoveAll(oldPath); err != nil {
			return fmt.Errorf("failed to remove existing %s before restore swap: %w", name, err)
		}
		if err := os.Rename(newPath, oldPath); err != nil {
			return fmt.Errorf("failed to move restored %s into place: %w", name, err)
		}
	}

	return os.RemoveAll(stagingDir)
}

// restoreStats holds metadata about a completed restore.
type restoreStats struct {
	FilesRestored int
	BytesRestored int64
}
