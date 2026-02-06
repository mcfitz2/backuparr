package main

import (
	"strings"
	"testing"
)

func TestFindAppConfig(t *testing.T) {
	config := BackuparrConfig{
		AppConfigs: []AppConfig{
			{AppType: "sonarr", Connection: Connection{APIKey: "key1"}},
			{AppType: "radarr", Connection: Connection{APIKey: "key2"}},
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
			cfg, err := findAppConfig(config, tt.app)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Connection.APIKey != tt.wantKey {
				t.Errorf("got apiKey %q, want %q", cfg.Connection.APIKey, tt.wantKey)
			}
		})
	}
}

func TestFindAppConfig_Empty(t *testing.T) {
	config := BackuparrConfig{}
	_, err := findAppConfig(config, "sonarr")
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
	// Config with no postgres and no PBS — should always pass
	config := BackuparrConfig{
		AppConfigs: []AppConfig{
			{
				AppType: "sonarr",
				Storage: []StorageConfig{{Type: "local", Path: "./backups"}},
			},
		},
	}
	if err := preflightCheck(config); err != nil {
		t.Fatalf("expected no error for local-only config, got: %v", err)
	}
}

func TestPreflightCheck_PostgresTools(t *testing.T) {
	config := BackuparrConfig{
		AppConfigs: []AppConfig{
			{
				AppType:  "sonarr",
				Postgres: &PostgresOverride{Host: "db.local"},
				Storage:  []StorageConfig{{Type: "local"}},
			},
		},
	}
	err := preflightCheck(config)
	// On CI/dev machines pg_dump and psql may or may not be installed.
	// We just verify the function runs without panic and returns the right
	// kind of error if tools are missing.
	if err != nil {
		if !strings.Contains(err.Error(), "pg_dump") && !strings.Contains(err.Error(), "psql") {
			t.Errorf("expected postgres-related error, got: %v", err)
		}
	}
}

func TestPreflightCheck_PBSTool(t *testing.T) {
	config := BackuparrConfig{
		AppConfigs: []AppConfig{
			{
				AppType: "sonarr",
				Storage: []StorageConfig{{Type: "pbs", Server: "pbs.local", Datastore: "store"}},
			},
		},
	}
	err := preflightCheck(config)
	// proxmox-backup-client is unlikely to be installed in dev/CI
	if err != nil {
		if !strings.Contains(err.Error(), "proxmox-backup-client") {
			t.Errorf("expected proxmox-backup-client error, got: %v", err)
		}
	}
}

func TestPreflightCheck_AllMissing(t *testing.T) {
	config := BackuparrConfig{
		AppConfigs: []AppConfig{
			{
				AppType:  "sonarr",
				Postgres: &PostgresOverride{Host: "db.local"},
				Storage:  []StorageConfig{{Type: "pbs", Server: "pbs.local", Datastore: "store"}},
			},
		},
	}
	err := preflightCheck(config)
	if err != nil {
		// Should mention all missing tools
		msg := err.Error()
		// At minimum proxmox-backup-client will be missing
		if !strings.Contains(msg, "proxmox-backup-client") {
			t.Errorf("expected proxmox-backup-client in error, got: %v", err)
		}
	}
}
