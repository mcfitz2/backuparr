package storage

import (
	"context"
	"io"
	"time"
)

// BackupMetadata describes a single backup file stored in a backend.
type BackupMetadata struct {
	// Key is the unique identifier within the backend (path, object key, snapshot ID).
	Key string
	// AppName is the application that produced the backup (e.g. "sonarr", "radarr").
	AppName string
	// FileName is the consistent backup filename (e.g. "sonarr_2026-02-06T120000Z.zip").
	FileName string
	// Size is the backup size in bytes.
	Size int64
	// CreatedAt is when the backup was created.
	CreatedAt time.Time
}

// Backend is the interface every storage provider implements.
type Backend interface {
	// Type returns the backend type identifier (e.g. "s3", "local").
	Type() string
	// Name returns a display name for this backend instance. Defaults to Type()
	// but can be overridden in config to distinguish multiple backends of the
	// same type (e.g. two local backends named "nas" and "usb").
	Name() string
	// SetName overrides the display name returned by Name().
	SetName(name string)
	// Upload stores backup data and returns metadata for the stored object.
	Upload(ctx context.Context, appName string, fileName string, data io.Reader, size int64) (*BackupMetadata, error)
	// Download retrieves a backup by key. Caller must close the reader.
	Download(ctx context.Context, key string) (io.ReadCloser, *BackupMetadata, error)
	// List returns all backups for a given app, ordered newest-first.
	List(ctx context.Context, appName string) ([]BackupMetadata, error)
	// Delete removes a backup by key.
	Delete(ctx context.Context, key string) error
}

// FormatBackupName creates a consistent backup filename from app name and timestamp.
// Format: <appName>_<YYYY-MM-DDTHHMMSSZ>.zip
func FormatBackupName(appName string, t time.Time) string {
	return appName + "_" + t.UTC().Format("2006-01-02T150405Z") + ".zip"
}
