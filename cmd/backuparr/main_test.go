package main

import (
	"strings"
	"testing"

	"backuparr/internal/config"
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
