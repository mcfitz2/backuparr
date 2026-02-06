package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"backuparr/storage"
)

// Ensure LocalBackend implements storage.Backend at compile time.
var _ storage.Backend = (*LocalBackend)(nil)

// LocalBackend stores backups on a local filesystem path.
type LocalBackend struct {
	basePath string
}

// New creates a new local storage backend rooted at basePath.
func New(basePath string) *LocalBackend {
	return &LocalBackend{basePath: basePath}
}

func (b *LocalBackend) Name() string {
	return "local"
}

// Upload writes backup data to <basePath>/<appName>/<fileName>.
func (b *LocalBackend) Upload(ctx context.Context, appName string, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	dir := filepath.Join(b.basePath, appName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, fileName)
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create file %s: %w", path, err)
	}
	defer file.Close()

	written, err := io.Copy(file, data)
	if err != nil {
		// Clean up partial file
		os.Remove(path)
		return nil, fmt.Errorf("failed to write backup: %w", err)
	}

	return &storage.BackupMetadata{
		Key:      path,
		AppName:  appName,
		FileName: fileName,
		Size:     written,
	}, nil
}

// Download opens a backup file by its key (full path).
func (b *LocalBackend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	info, err := os.Stat(key)
	if err != nil {
		return nil, nil, fmt.Errorf("backup not found: %w", err)
	}

	file, err := os.Open(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open backup: %w", err)
	}

	meta := &storage.BackupMetadata{
		Key:       key,
		AppName:   filepath.Base(filepath.Dir(key)),
		FileName:  filepath.Base(key),
		Size:      info.Size(),
		CreatedAt: info.ModTime(),
	}

	return file, meta, nil
}

// List returns all backup files for an app, sorted newest-first by modification time.
func (b *LocalBackend) List(ctx context.Context, appName string) ([]storage.BackupMetadata, error) {
	dir := filepath.Join(b.basePath, appName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list directory %s: %w", dir, err)
	}

	var backups []storage.BackupMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, storage.BackupMetadata{
			Key:       filepath.Join(dir, entry.Name()),
			AppName:   appName,
			FileName:  entry.Name(),
			Size:      info.Size(),
			CreatedAt: info.ModTime(),
		})
	}

	// Sort newest-first
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// Delete removes a backup file by its key (full path).
func (b *LocalBackend) Delete(ctx context.Context, key string) error {
	if err := os.Remove(key); err != nil {
		return fmt.Errorf("failed to delete backup %s: %w", key, err)
	}
	return nil
}
