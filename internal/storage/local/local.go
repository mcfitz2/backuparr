package local

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"backuparr/internal/storage"
)

// Ensure LocalBackend implements storage.Backend at compile time.
var _ storage.Backend = (*LocalBackend)(nil)

// LocalBackend stores backups on a local filesystem path.
type LocalBackend struct {
	basePath string
	name     string
}

// New creates a new local storage backend rooted at basePath.
func New(basePath string) *LocalBackend {
	return &LocalBackend{basePath: basePath}
}

func (b *LocalBackend) Type() string { return "local" }

func (b *LocalBackend) Name() string {
	if b.name != "" {
		return b.name
	}
	return b.Type()
}

func (b *LocalBackend) SetName(name string) { b.name = name }

// resolveKey converts a caller-supplied key into a path relative to
// basePath, rejecting anything that resolves outside of it (absolute paths,
// "../" traversal). Combined with os.Root in the methods below, this also
// blocks symlinks within basePath that point outside of it.
func (b *LocalBackend) resolveKey(key string) (string, error) {
	rel, err := filepath.Rel(b.basePath, key)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid backup key %q", key)
	}
	return rel, nil
}

// statNoSymlink stats rel within root and rejects it outright if it is a
// symlink. A legitimate backup key always names a regular file created by
// Upload, so this also covers symlinks that point outside basePath: Remove
// unlinks the symlink itself rather than its target, so os.Root's own
// escape checks don't apply to it the way they do for Open/Stat.
func statNoSymlink(root *os.Root, rel string) (os.FileInfo, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to operate on symlink %q", rel)
	}
	return info, nil
}

// Upload writes backup data to <basePath>/<appName>/<fileName>.
func (b *LocalBackend) Upload(ctx context.Context, appName string, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	if err := os.MkdirAll(b.basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", b.basePath, err)
	}

	root, err := os.OpenRoot(b.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", b.basePath, err)
	}
	defer root.Close()

	dir := filepath.Join(b.basePath, appName)
	if err := root.MkdirAll(appName, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	relPath := filepath.Join(appName, fileName)
	path := filepath.Join(b.basePath, relPath)
	file, err := root.Create(relPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create file %s: %w", path, err)
	}
	defer file.Close()

	written, err := io.Copy(file, data)
	if err != nil {
		// Clean up partial file
		root.Remove(relPath)
		return nil, fmt.Errorf("failed to write backup: %w", err)
	}

	return &storage.BackupMetadata{
		Key:      path,
		AppName:  appName,
		FileName: fileName,
		Size:     written,
	}, nil
}

// Download opens a backup file by its key (full path). The key must resolve
// inside basePath.
func (b *LocalBackend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	rel, err := b.resolveKey(key)
	if err != nil {
		return nil, nil, err
	}

	root, err := os.OpenRoot(b.basePath)
	if err != nil {
		return nil, nil, fmt.Errorf("backup not found: %w", err)
	}
	defer root.Close()

	info, err := statNoSymlink(root, rel)
	if err != nil {
		return nil, nil, fmt.Errorf("backup not found: %w", err)
	}

	file, err := root.Open(rel)
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
	root, err := os.OpenRoot(b.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open %s: %w", b.basePath, err)
	}
	defer root.Close()

	dir := filepath.Join(b.basePath, appName)
	entries, err := fs.ReadDir(root.FS(), appName)
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

// Delete removes a backup file by its key (full path). The key must resolve
// inside basePath.
func (b *LocalBackend) Delete(ctx context.Context, key string) error {
	rel, err := b.resolveKey(key)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(b.basePath)
	if err != nil {
		return fmt.Errorf("failed to delete backup %s: %w", key, err)
	}
	defer root.Close()

	if _, err := statNoSymlink(root, rel); err != nil {
		return fmt.Errorf("failed to delete backup %s: %w", key, err)
	}

	if err := root.Remove(rel); err != nil {
		return fmt.Errorf("failed to delete backup %s: %w", key, err)
	}
	return nil
}
