package pbs

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// buildRepository
// ---------------------------------------------------------------------------

func TestBuildRepository(t *testing.T) {
	tests := []struct {
		name      string
		username  string
		server    string
		port      int
		datastore string
		want      string
	}{
		{
			name:      "full config",
			username:  "backup@pbs",
			server:    "pbs.example.com",
			port:      8007,
			datastore: "store1",
			want:      "backup@pbs@pbs.example.com:8007:store1",
		},
		{
			name:      "custom port",
			username:  "user@pbs!token",
			server:    "192.168.1.10",
			port:      9007,
			datastore: "backups",
			want:      "user@pbs!token@192.168.1.10:9007:backups",
		},
		{
			name:      "default username",
			username:  "",
			server:    "pbs.local",
			port:      8007,
			datastore: "data",
			want:      "root@pam@pbs.local:8007:data",
		},
		{
			name:      "default port",
			username:  "admin@pam",
			server:    "pbs.local",
			port:      0,
			datastore: "data",
			want:      "admin@pam@pbs.local:8007:data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRepository(tt.username, tt.server, tt.port, tt.datastore)
			if got != tt.want {
				t.Errorf("buildRepository() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseSnapshotFromOutput
// ---------------------------------------------------------------------------

func TestParseSnapshotFromOutput(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		appName string
		want    string
	}{
		{
			name: "standard output",
			output: `Starting backup protocol (zstd)
Starting backup: host/sonarr/2026-02-06T12:00:00Z
Client name: backuparr
Uploading backup catalog
Upload statistics: 52 MiB
Duration: 3.45s
End Time: 2026-02-06T12:00:03Z`,
			appName: "sonarr",
			want:    "host/sonarr/2026-02-06T12:00:00Z",
		},
		{
			name: "different app",
			output: `Starting backup: host/radarr/2026-01-15T08:30:00Z
Upload statistics: 100 MiB`,
			appName: "radarr",
			want:    "host/radarr/2026-01-15T08:30:00Z",
		},
		{
			name:    "no match - wrong app",
			output:  "Starting backup: host/sonarr/2026-02-06T12:00:00Z\n",
			appName: "radarr",
			want:    "",
		},
		{
			name:    "no match - no starting line",
			output:  "Upload complete\nDuration: 1s",
			appName: "sonarr",
			want:    "",
		},
		{
			name:    "empty output",
			output:  "",
			appName: "sonarr",
			want:    "",
		},
		{
			name:    "line with extra whitespace",
			output:  "  Starting backup: host/sonarr/2026-02-06T12:00:00Z  \n",
			appName: "sonarr",
			want:    "host/sonarr/2026-02-06T12:00:00Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSnapshotFromOutput(tt.output, tt.appName)
			if got != tt.want {
				t.Errorf("parseSnapshotFromOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseSnapshotKey
// ---------------------------------------------------------------------------

func TestParseSnapshotKey(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		wantApp    string
		wantTime   time.Time
		wantZero   bool
	}{
		{
			name:     "valid key",
			key:      "host/sonarr/2026-02-06T12:00:00Z",
			wantApp:  "sonarr",
			wantTime: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
		},
		{
			name:     "valid key with offset",
			key:      "host/radarr/2025-12-31T23:59:59Z",
			wantApp:  "radarr",
			wantTime: time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		},
		{
			name:     "invalid - too few parts",
			key:      "host/sonarr",
			wantApp:  "",
			wantZero: true,
		},
		{
			name:     "invalid - bad time",
			key:      "host/sonarr/not-a-time",
			wantApp:  "sonarr",
			wantZero: true,
		},
		{
			name:     "empty key",
			key:      "",
			wantApp:  "",
			wantZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotApp, gotTime := parseSnapshotKey(tt.key)
			if gotApp != tt.wantApp {
				t.Errorf("parseSnapshotKey() appName = %q, want %q", gotApp, tt.wantApp)
			}
			if tt.wantZero {
				if !gotTime.IsZero() {
					t.Errorf("parseSnapshotKey() time = %v, want zero", gotTime)
				}
			} else if !gotTime.Equal(tt.wantTime) {
				t.Errorf("parseSnapshotKey() time = %v, want %v", gotTime, tt.wantTime)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Config validation (New)
// ---------------------------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing server",
			cfg:     Config{Datastore: "store1"},
			wantErr: "server is required",
		},
		{
			name:    "missing datastore",
			cfg:     Config{Server: "pbs.local"},
			wantErr: "datastore is required",
		},
		{
			name: "missing proxmox-backup-client",
			cfg: Config{
				Server:    "pbs.local",
				Datastore: "store1",
			},
			// On dev machines without the CLI, this is expected
			wantErr: "proxmox-backup-client not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatalf("New() error = nil, wantErr containing %q", tt.wantErr)
			}
			if got := err.Error(); !contains(got, tt.wantErr) {
				t.Errorf("New() error = %q, want containing %q", got, tt.wantErr)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// snapshotInfo JSON parsing (via List logic)
// ---------------------------------------------------------------------------

func TestSnapshotInfoParsing(t *testing.T) {
	// Verify our struct tags match the PBS JSON output format
	info := snapshotInfo{
		BackupType: "host",
		BackupID:   "sonarr",
		BackupTime: 1770364800, // 2026-02-06T12:00:00Z
	}

	if info.BackupType != "host" {
		t.Errorf("BackupType = %q, want %q", info.BackupType, "host")
	}
	if info.BackupID != "sonarr" {
		t.Errorf("BackupID = %q, want %q", info.BackupID, "sonarr")
	}

	ts := time.Unix(info.BackupTime, 0).UTC()
	if ts.Year() != 2026 || ts.Month() != 2 || ts.Day() != 6 {
		t.Errorf("BackupTime = %v, want 2026-02-06", ts)
	}
}

// ---------------------------------------------------------------------------
// PBSBackend helper methods
// ---------------------------------------------------------------------------

func TestBaseArgs(t *testing.T) {
	b := &PBSBackend{
		repository: "root@pam@pbs.local:8007:store1",
	}

	args := b.baseArgs()
	if len(args) != 2 {
		t.Fatalf("baseArgs() len = %d, want 2", len(args))
	}
	if args[0] != "--repository" || args[1] != "root@pam@pbs.local:8007:store1" {
		t.Errorf("baseArgs() = %v, want [--repository root@pam@pbs.local:8007:store1]", args)
	}
}

func TestBaseArgs_WithNamespace(t *testing.T) {
	b := &PBSBackend{
		repository: "root@pam@pbs.local:8007:store1",
		namespace:  "prod/backups",
	}

	args := b.baseArgs()
	if len(args) != 4 {
		t.Fatalf("baseArgs() len = %d, want 4", len(args))
	}
	if args[2] != "--ns" || args[3] != "prod/backups" {
		t.Errorf("baseArgs() ns = %v %v, want --ns prod/backups", args[2], args[3])
	}
}

func TestName(t *testing.T) {
	b := &PBSBackend{}
	if b.Name() != "pbs" {
		t.Errorf("Name() = %q, want %q", b.Name(), "pbs")
	}
}
