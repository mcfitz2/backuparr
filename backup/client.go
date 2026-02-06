package backup

import (
	"context"
	"io"
	"time"
)

// BackupResult contains information about a completed backup operation
type BackupResult struct {
	Name      string
	Path      string
	Size      int64
	CreatedAt time.Time
}

// Client defines the high-level interface for any application that supports backup operations.
// Storage routing (local, S3, PBS, etc.) is handled by the orchestrator, not the client.
type Client interface {
	// Name returns the name/type of the application (e.g., "sonarr", "radarr")
	Name() string

	// Backup triggers a backup and returns the backup file content as a reader.
	// The caller is responsible for closing the reader.
	Backup(ctx context.Context) (*BackupResult, io.ReadCloser, error)

	// Restore restores the application from a backup file.
	// The reader should contain the backup file content.
	Restore(ctx context.Context, backup io.Reader) error
}
