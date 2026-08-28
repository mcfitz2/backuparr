package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

func TestParse(t *testing.T) {
	tests := []struct {
		name        string
		contents    string
		wantErr     bool
		wantErrText string
		wantApps    int
	}{
		{
			name: "valid config",
			contents: `appConfigs:
  - appType: sonarr
    name: sonarr-4k
    connection:
      url: http://localhost:8989
      apiKey: abc123
    storage:
      - type: local
        path: ./backups
    retention:
      keepLast: 3
`,
			wantApps: 1,
		},
		{
			// The silent-success path from issue #7: a misspelled top-level key
			// used to yield an empty config and a clean exit.
			name: "misspelled top-level key is rejected",
			contents: `appconfigs:
  - appType: sonarr
`,
			wantErr:     true,
			wantErrText: "appconfigs",
		},
		{
			name: "unknown nested key is rejected",
			contents: `appConfigs:
  - appType: sonarr
    conection:
      url: http://localhost:8989
`,
			wantErr:     true,
			wantErrText: "conection",
		},
		{
			name:     "empty file parses to no targets",
			contents: "",
			wantApps: 0,
		},
		{
			// Issue #9: an omitted retention block decodes to an all-zero
			// policy, which deletes every existing backup on the next run.
			name: "missing retention block is rejected",
			contents: `appConfigs:
  - appType: sonarr
    connection:
      url: http://localhost:8989
      apiKey: abc123
    storage:
      - type: local
        path: ./backups
`,
			wantErr:     true,
			wantErrText: "sonarr",
		},
		{
			name: "all-zero retention block is rejected",
			contents: `appConfigs:
  - appType: radarr
    name: radarr-main
    connection:
      url: http://localhost:7878
      apiKey: abc123
    storage:
      - type: local
        path: ./backups
    retention:
      keepLast: 0
      keepDaily: 0
`,
			wantErr:     true,
			wantErrText: "radarr-main",
		},
		{
			name:     "explicitly empty app list",
			contents: "appConfigs: []\n",
			wantApps: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(writeConfig(t, tt.contents))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse() = nil error, want error")
				}
				if !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("error = %q, want mention of %q", err.Error(), tt.wantErrText)
				}
				return
			}

			if err != nil {
				t.Fatalf("Parse() = %v, want nil", err)
			}
			if len(cfg.AppConfigs) != tt.wantApps {
				t.Errorf("got %d app configs, want %d", len(cfg.AppConfigs), tt.wantApps)
			}
		})
	}
}

func TestParse_MissingFile(t *testing.T) {
	_, err := Parse(filepath.Join(t.TempDir(), "nope.yml"))
	if err == nil {
		t.Fatal("Parse() = nil error, want error for a missing file")
	}
}

// The repo's own configs must keep parsing under strict decoding.
func TestParse_RepoConfigs(t *testing.T) {
	for _, path := range []string{"../../config.yml", "../../integration-tests/config.test.yml"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := os.Stat(path); err != nil {
				t.Skipf("%s not present", path)
			}
			if _, err := Parse(path); err != nil {
				t.Errorf("Parse(%s) = %v, want nil", path, err)
			}
		})
	}
}
