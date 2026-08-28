package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"backuparr/internal/backup"
	"backuparr/internal/config"
	"backuparr/internal/storage"
)

func TestFindAppConfig(t *testing.T) {
	cfg := config.BackuparrConfig{
		AppConfigs: []config.AppConfig{
			{AppType: "sonarr", Connection: config.Connection{APIKey: "key1"}},
			{AppType: "radarr", Connection: config.Connection{APIKey: "key2"}},
		},
	}

	tests := []struct {
		name    string
		app     string
		wantKey string
		wantErr bool
	}{
		{"found sonarr", "sonarr", "key1", false},
		{"found radarr", "radarr", "key2", false},
		{"not found", "lidarr", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ac, err := findAppConfig(cfg, tt.app)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ac.Connection.APIKey != tt.wantKey {
				t.Errorf("got apiKey %q, want %q", ac.Connection.APIKey, tt.wantKey)
			}
		})
	}
}

func TestFindAppConfig_Empty(t *testing.T) {
	cfg := config.BackuparrConfig{}
	_, err := findAppConfig(cfg, "sonarr")
	if err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"zero", 0, "0 B"},
		{"bytes", 512, "512 B"},
		{"kilobytes", 1536, "1.5 KB"},
		{"megabytes", 52428800, "50.0 MB"},
		{"gigabytes", 1610612736, "1.5 GB"},
		{"exact 1KB", 1024, "1.0 KB"},
		{"exact 1MB", 1048576, "1.0 MB"},
		{"exact 1GB", 1073741824, "1.0 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSize(tt.bytes)
			if got != tt.want {
				t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func TestPreflightCheck_NoTools(t *testing.T) {
	// Config with no postgres — should always pass
	cfg := config.BackuparrConfig{
		AppConfigs: []config.AppConfig{
			{
				AppType: "sonarr",
				Storage: []config.StorageConfig{{Type: "local", Path: "./backups"}},
			},
		},
	}
	if err := preflightCheck(cfg); err != nil {
		t.Fatalf("expected no error for local-only config, got: %v", err)
	}
}

func TestPreflightCheck_PostgresTools(t *testing.T) {
	cfg := config.BackuparrConfig{
		AppConfigs: []config.AppConfig{
			{
				AppType:  "sonarr",
				Postgres: &config.PostgresOverride{Host: "db.local"},
				Storage:  []config.StorageConfig{{Type: "local"}},
			},
		},
	}
	err := preflightCheck(cfg)
	// On CI/dev machines pg_dump and psql may or may not be installed.
	// We just verify the function runs without panic and returns the right
	// kind of error if tools are missing.
	if err != nil {
		if !strings.Contains(err.Error(), "pg_dump") && !strings.Contains(err.Error(), "psql") {
			t.Errorf("expected postgres-related error, got: %v", err)
		}
	}
}

func TestPreflightCheck_AllMissing(t *testing.T) {
	cfg := config.BackuparrConfig{
		AppConfigs: []config.AppConfig{
			{
				AppType:  "sonarr",
				Postgres: &config.PostgresOverride{Host: "db.local"},
				Storage:  []config.StorageConfig{{Type: "local", Path: "./backups"}},
			},
		},
	}
	err := preflightCheck(cfg)
	if err != nil {
		// Should mention postgres tools if missing
		msg := err.Error()
		if !strings.Contains(msg, "pg_dump") && !strings.Contains(msg, "psql") {
			t.Errorf("expected postgres tool errors, got: %v", err)
		}
	}
}

func TestStorageConfigName(t *testing.T) {
	tests := []struct {
		cfg  config.StorageConfig
		want string
	}{
		{config.StorageConfig{Type: "local"}, "local"},
		{config.StorageConfig{Name: "nas", Type: "local"}, "nas"},
		{config.StorageConfig{Name: "offsite", Type: "s3"}, "offsite"},
		{config.StorageConfig{Type: "s3"}, "s3"},
	}
	for _, tt := range tests {
		got := config.StorageConfigName(tt.cfg)
		if got != tt.want {
			t.Errorf("config.StorageConfigName(%+v) = %q, want %q", tt.cfg, got, tt.want)
		}
	}
}

func TestFindBackend(t *testing.T) {
	appCfg := config.AppConfig{
		AppType: "sonarr",
		Storage: []config.StorageConfig{
			{Type: "local", Path: "./backups"},
			{Name: "nas", Type: "local", Path: "/mnt/nas"},
		},
	}

	tests := []struct {
		name        string
		backendName string
		wantName    string
		wantErr     bool
	}{
		{"find by type default", "local", "local", false},
		{"find by explicit name", "nas", "nas", false},
		{"not found", "s3", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := findBackend(appCfg, tt.backendName)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b.Name() != tt.wantName {
				t.Errorf("backend.Name() = %q, want %q", b.Name(), tt.wantName)
			}
		})
	}
}

func TestFindBackend_Ambiguous(t *testing.T) {
	appCfg := config.AppConfig{
		AppType: "sonarr",
		Storage: []config.StorageConfig{
			{Type: "local", Path: "./backups1"},
			{Type: "local", Path: "./backups2"},
		},
	}

	_, err := findBackend(appCfg, "local")
	if err == nil {
		t.Fatal("expected error for ambiguous backends")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("error = %q, want mention of multiple backends", err.Error())
	}
}

// --- fakes ---------------------------------------------------------------

// fakeClient is a backup.Client that returns canned data, or a canned error.
type fakeClient struct {
	name      string
	data      []byte
	backupErr error
}

func (c *fakeClient) Name() string { return c.name }

func (c *fakeClient) Backup(ctx context.Context) (*backup.BackupResult, io.ReadCloser, error) {
	if c.backupErr != nil {
		return nil, nil, c.backupErr
	}
	return &backup.BackupResult{
			Name:      c.name,
			Size:      int64(len(c.data)),
			CreatedAt: time.Now(),
		},
		io.NopCloser(bytes.NewReader(c.data)),
		nil
}

func (c *fakeClient) Restore(ctx context.Context, r io.Reader) error { return nil }

// fakeBackend is a storage.Backend with injectable failures. Uploads are kept
// in memory so List/Delete can exercise the retention path.
type fakeBackend struct {
	name      string
	uploadErr error
	listErr   error
	deleteErr error

	uploaded []storage.BackupMetadata
	deleted  []string
}

func (b *fakeBackend) Type() string        { return "fake" }
func (b *fakeBackend) Name() string        { return b.name }
func (b *fakeBackend) SetName(name string) { b.name = name }

func (b *fakeBackend) Upload(ctx context.Context, appName, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	if b.uploadErr != nil {
		return nil, b.uploadErr
	}
	meta := storage.BackupMetadata{
		Key:       appName + "/" + fileName,
		AppName:   appName,
		FileName:  fileName,
		Size:      size,
		CreatedAt: time.Now(),
	}
	b.uploaded = append(b.uploaded, meta)
	return &meta, nil
}

func (b *fakeBackend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	return nil, nil, errors.New("not implemented")
}

func (b *fakeBackend) List(ctx context.Context, appName string) ([]storage.BackupMetadata, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.uploaded, nil
}

func (b *fakeBackend) Delete(ctx context.Context, key string) error {
	if b.deleteErr != nil {
		return b.deleteErr
	}
	b.deleted = append(b.deleted, key)
	return nil
}

// --- runBackup -----------------------------------------------------------

func TestRunBackup_UploadFailures(t *testing.T) {
	uploadErr := errors.New("connection refused")

	tests := []struct {
		name        string
		backends    []storage.Backend
		wantErr     bool
		wantMention []string
	}{
		{
			name: "all backends succeed",
			backends: []storage.Backend{
				&fakeBackend{name: "local"},
				&fakeBackend{name: "s3"},
			},
			wantErr: false,
		},
		{
			name: "one of two backends fails",
			backends: []storage.Backend{
				&fakeBackend{name: "local"},
				&fakeBackend{name: "s3", uploadErr: uploadErr},
			},
			wantErr:     true,
			wantMention: []string{"s3", "1 of 2", "connection refused"},
		},
		{
			name: "all backends fail",
			backends: []storage.Backend{
				&fakeBackend{name: "local", uploadErr: uploadErr},
				&fakeBackend{name: "s3", uploadErr: uploadErr},
			},
			wantErr:     true,
			wantMention: []string{"local", "s3", "2 of 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{name: "sonarr", data: []byte("backup-data")}

			err := runBackup(context.Background(), client, tt.backends, config.RetentionPolicy{}, log.Default())

			if tt.wantErr && err == nil {
				t.Fatalf("runBackup() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("runBackup() = %v, want nil", err)
			}
			for _, want := range tt.wantMention {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want mention of %q", err.Error(), want)
				}
			}
		})
	}
}

func TestRunBackup_AppBackupFails(t *testing.T) {
	client := &fakeClient{name: "truenas", backupErr: errors.New("API key was rejected")}
	backend := &fakeBackend{name: "local"}

	err := runBackup(context.Background(), client, []storage.Backend{backend}, config.RetentionPolicy{}, log.Default())
	if err == nil {
		t.Fatal("runBackup() = nil, want error")
	}
	if !strings.Contains(err.Error(), "API key was rejected") {
		t.Errorf("error = %q, want the underlying cause", err.Error())
	}
	if len(backend.uploaded) != 0 {
		t.Errorf("uploaded %d objects, want 0 when the app backup failed", len(backend.uploaded))
	}
}

// Retention is housekeeping that runs after the backup is safely stored, so a
// failure there must not fail the backup.
func TestRunBackup_RetentionFailureDoesNotFail(t *testing.T) {
	client := &fakeClient{name: "sonarr", data: []byte("backup-data")}
	backend := &fakeBackend{name: "local", listErr: errors.New("list failed")}

	err := runBackup(context.Background(), client, []storage.Backend{backend},
		config.RetentionPolicy{KeepLast: 1}, log.Default())
	if err != nil {
		t.Fatalf("runBackup() = %v, want nil (retention failures are log-only)", err)
	}
	if len(backend.uploaded) != 1 {
		t.Errorf("uploaded %d objects, want 1", len(backend.uploaded))
	}
}

func TestAppDisplayName(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.AppConfig
		want string
	}{
		{"explicit name wins", config.AppConfig{Name: "sonarr-4k", AppType: "sonarr"}, "sonarr-4k"},
		{"falls back to app type", config.AppConfig{AppType: "radarr"}, "radarr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.AppConfigName(tt.cfg); got != tt.want {
				t.Errorf("AppConfigName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- runBackup temp-file spooling ------------------------------------------
//
// runBackup no longer buffers the whole backup as a []byte; it spools the
// source reader to a temp file (named "backuparr-*.zip") and re-opens that
// file once per backend. These tests check the observable consequences of
// that: the temp file is always cleaned up, and every backend still gets an
// independent, complete, byte-identical copy of the backup.

func tempBackupFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "backuparr-*.zip"))
	if err != nil {
		t.Fatalf("glob temp dir: %v", err)
	}
	return matches
}

func TestRunBackup_TempFileRemovedOnSuccess(t *testing.T) {
	before := tempBackupFiles(t)

	client := &fakeClient{name: "sonarr", data: []byte("backup-data")}
	backend := &fakeBackend{name: "local"}

	if err := runBackup(context.Background(), client, []storage.Backend{backend}, config.RetentionPolicy{}, log.Default()); err != nil {
		t.Fatalf("runBackup() = %v, want nil", err)
	}

	after := tempBackupFiles(t)
	if len(after) != len(before) {
		t.Errorf("temp backup file(s) leaked after success: before=%v after=%v", before, after)
	}
}

func TestRunBackup_TempFileRemovedOnUploadError(t *testing.T) {
	before := tempBackupFiles(t)

	client := &fakeClient{name: "sonarr", data: []byte("backup-data")}
	backend := &fakeBackend{name: "s3", uploadErr: errors.New("connection refused")}

	err := runBackup(context.Background(), client, []storage.Backend{backend}, config.RetentionPolicy{}, log.Default())
	if err == nil {
		t.Fatal("runBackup() = nil, want error")
	}

	after := tempBackupFiles(t)
	if len(after) != len(before) {
		t.Errorf("temp backup file(s) leaked after upload error: before=%v after=%v", before, after)
	}
}

func TestRunBackup_TempFileRemovedOnAppBackupError(t *testing.T) {
	before := tempBackupFiles(t)

	// Backup() fails before a temp file would even be created; verifies the
	// early-return path leaves nothing behind either.
	client := &fakeClient{name: "truenas", backupErr: errors.New("API key was rejected")}
	backend := &fakeBackend{name: "local"}

	err := runBackup(context.Background(), client, []storage.Backend{backend}, config.RetentionPolicy{}, log.Default())
	if err == nil {
		t.Fatal("runBackup() = nil, want error")
	}

	after := tempBackupFiles(t)
	if len(after) != len(before) {
		t.Errorf("temp backup file(s) leaked after app backup error: before=%v after=%v", before, after)
	}
}

// recordingBackend captures the exact bytes it receives via Upload, so tests
// can verify every backend gets a complete, independent, identical copy of
// the backup when a single source is fanned out to several destinations —
// the regression most likely to be introduced by removing the shared []byte.
type recordingBackend struct {
	name     string
	received []byte
	size     int64
}

func (b *recordingBackend) Type() string        { return "recording" }
func (b *recordingBackend) Name() string        { return b.name }
func (b *recordingBackend) SetName(name string) { b.name = name }

func (b *recordingBackend) Upload(ctx context.Context, appName, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	got, err := io.ReadAll(data)
	if err != nil {
		return nil, err
	}
	b.received = got
	b.size = size
	return &storage.BackupMetadata{Key: appName + "/" + fileName, AppName: appName, FileName: fileName, Size: size}, nil
}

func (b *recordingBackend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	return nil, nil, errors.New("not implemented")
}

func (b *recordingBackend) List(ctx context.Context, appName string) ([]storage.BackupMetadata, error) {
	return nil, nil
}

func (b *recordingBackend) Delete(ctx context.Context, key string) error { return nil }

func TestRunBackup_MultipleBackendsReceiveIdenticalData(t *testing.T) {
	// Large enough (~128KB) that a bug sharing one file offset/cursor across
	// backends, or truncating anything but the first backend, would surface.
	want := bytes.Repeat([]byte("backuparr-multi-backend-payload-"), 4096)
	client := &fakeClient{name: "sonarr", data: want}

	backends := []storage.Backend{
		&recordingBackend{name: "local"},
		&recordingBackend{name: "s3"},
		&recordingBackend{name: "nas"},
	}

	if err := runBackup(context.Background(), client, backends, config.RetentionPolicy{}, log.Default()); err != nil {
		t.Fatalf("runBackup() = %v, want nil", err)
	}

	for _, b := range backends {
		rb := b.(*recordingBackend)
		if rb.size != int64(len(want)) {
			t.Errorf("backend %s: size = %d, want %d", rb.name, rb.size, len(want))
		}
		if !bytes.Equal(rb.received, want) {
			t.Errorf("backend %s: received %d bytes not matching source (want %d bytes); data corrupted or truncated across backends", rb.name, len(rb.received), len(want))
		}
	}
}

// countingReader emits deterministic bytes without ever holding the full
// payload in memory, standing in for a large real backup stream.
type countingReader struct {
	remaining int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = byte(i)
	}
	r.remaining -= n
	return int(n), nil
}

// streamingClient is a backup.Client whose Backup() returns a reader that
// generates its payload lazily instead of handing back a pre-built []byte.
type streamingClient struct {
	name  string
	total int64
}

func (c *streamingClient) Name() string { return c.name }

func (c *streamingClient) Backup(ctx context.Context) (*backup.BackupResult, io.ReadCloser, error) {
	return &backup.BackupResult{Name: c.name, Size: c.total, CreatedAt: time.Now()},
		io.NopCloser(&countingReader{remaining: c.total}), nil
}

func (c *streamingClient) Restore(ctx context.Context, r io.Reader) error { return nil }

// hashingBackend consumes the upload stream via io.Copy to io.Discard instead
// of retaining it, so the test itself doesn't need to hold the payload in
// memory either.
type hashingBackend struct {
	name string
	n    int64
}

func (b *hashingBackend) Type() string        { return "hashing" }
func (b *hashingBackend) Name() string        { return b.name }
func (b *hashingBackend) SetName(name string) { b.name = name }

func (b *hashingBackend) Upload(ctx context.Context, appName, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	n, err := io.Copy(io.Discard, data)
	if err != nil {
		return nil, err
	}
	b.n = n
	return &storage.BackupMetadata{Key: appName + "/" + fileName, AppName: appName, FileName: fileName, Size: size}, nil
}

func (b *hashingBackend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	return nil, nil, errors.New("not implemented")
}

func (b *hashingBackend) List(ctx context.Context, appName string) ([]storage.BackupMetadata, error) {
	return nil, nil
}

func (b *hashingBackend) Delete(ctx context.Context, key string) error { return nil }

// TestRunBackup_StreamsWithoutFullBuffering runs a payload much larger than
// would be comfortable to hold as a single []byte (as the old
// io.ReadAll-based implementation did) through runBackup, and checks the
// exact byte count survives end to end.
//
// NOTE: this test does not measure process memory and cannot by itself prove
// peak RSS stayed flat for a 256MiB backup — that guarantee comes from
// runBackup spooling via io.Copy (fixed-size internal buffer) to a temp file
// and re-opening it per backend, rather than io.ReadAll into one []byte. This
// test only proves the streaming path is still functionally correct at a
// size where "just buffer it" would be a real cost, not that memory is bounded.
func TestRunBackup_StreamsWithoutFullBuffering(t *testing.T) {
	const total = 256 * 1024 * 1024 // 256MiB
	client := &streamingClient{name: "sonarr", total: total}
	backend := &hashingBackend{name: "local"}

	if err := runBackup(context.Background(), client, []storage.Backend{backend}, config.RetentionPolicy{}, log.Default()); err != nil {
		t.Fatalf("runBackup() = %v, want nil", err)
	}
	if backend.n != total {
		t.Errorf("backend received %d bytes, want %d", backend.n, total)
	}
}
