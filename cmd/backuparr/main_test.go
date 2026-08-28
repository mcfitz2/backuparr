package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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

			err := runBackup(context.Background(), client, tt.backends, config.RetentionPolicy{})

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

	err := runBackup(context.Background(), client, []storage.Backend{backend}, config.RetentionPolicy{})
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
		config.RetentionPolicy{KeepLast: 1})
	if err != nil {
		t.Fatalf("runBackup() = %v, want nil (retention failures are log-only)", err)
	}
	if len(backend.uploaded) != 1 {
		t.Errorf("uploaded %d objects, want 1", len(backend.uploaded))
	}
}
