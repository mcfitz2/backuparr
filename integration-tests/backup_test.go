package integration_tests

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"backuparr/backup"
	"backuparr/radarr"
	"backuparr/sonarr"
)

// Test configuration for each instance
type testInstance struct {
	name          string
	containerName string
	appType       string
	url           string
	pgOverride    *backup.PostgresConfig
	isPostgres    bool
}

var testInstances = []testInstance{
	{
		name:          "sonarr-sqlite",
		containerName: "sonarr-sqlite",
		appType:       "sonarr",
		url:           "http://localhost:8989",
		pgOverride:    nil,
		isPostgres:    false,
	},
	{
		name:          "sonarr-postgres",
		containerName: "sonarr-postgres",
		appType:       "sonarr",
		url:           "http://localhost:8990",
		pgOverride: &backup.PostgresConfig{
			Host: "localhost",
			Port: "5433",
		},
		isPostgres: true,
	},
	{
		name:          "radarr-sqlite",
		containerName: "radarr-sqlite",
		appType:       "radarr",
		url:           "http://localhost:7878",
		pgOverride:    nil,
		isPostgres:    false,
	},
	{
		name:          "radarr-postgres",
		containerName: "radarr-postgres",
		appType:       "radarr",
		url:           "http://localhost:7879",
		pgOverride: &backup.PostgresConfig{
			Host: "localhost",
			Port: "5434",
		},
		isPostgres: true,
	},
}

// configXML is used to parse the API key from container config
type configXML struct {
	XMLName xml.Name `xml:"Config"`
	ApiKey  string   `xml:"ApiKey"`
}

// getAPIKeyFromContainer reads the API key from a running container's config.xml
func getAPIKeyFromContainer(containerName string) (string, error) {
	cmd := exec.Command("docker", "exec", containerName, "cat", "/config/config.xml")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to read config from container %s: %w", containerName, err)
	}

	var config configXML
	if err := xml.Unmarshal(output, &config); err != nil {
		return "", fmt.Errorf("failed to parse config.xml: %w", err)
	}

	return config.ApiKey, nil
}

// createClient creates a backup client for the given test instance
func createClient(t *testing.T, inst testInstance) backup.Client {
	// Get the actual API key from the running container
	apiKey, err := getAPIKeyFromContainer(inst.containerName)
	if err != nil {
		t.Fatalf("failed to get API key from container %s: %v", inst.containerName, err)
	}
	t.Logf("Using API key from container %s: %s", inst.containerName, apiKey)

	var client backup.Client

	switch inst.appType {
	case "sonarr":
		client, err = sonarr.NewSonarrClient(inst.url, apiKey, "", "", inst.pgOverride)
	case "radarr":
		client, err = radarr.NewRadarrClient(inst.url, apiKey, "", "", inst.pgOverride)
	default:
		t.Fatalf("unknown app type: %s", inst.appType)
	}

	if err != nil {
		t.Fatalf("failed to create client for %s: %v", inst.name, err)
	}

	return client
}

// TestBackup tests that backup works for all instances
func TestBackup(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range testInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			// Run backup
			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}
			defer reader.Close()

			// Validate result
			if result == nil {
				t.Fatal("BackupResult is nil")
			}

			if result.Name == "" {
				t.Error("BackupResult.Name is empty")
			}

			if result.Path == "" {
				t.Error("BackupResult.Path is empty")
			}

			t.Logf("Backup created: %s (size: %d bytes)", result.Name, result.Size)

			// Read backup content
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("failed to read backup content: %v", err)
			}

			if len(content) == 0 {
				t.Fatal("backup content is empty")
			}

			t.Logf("Downloaded %d bytes", len(content))

			// Validate it's a valid ZIP file
			zipReader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
			if err != nil {
				t.Fatalf("backup is not a valid ZIP file: %v", err)
			}

			// Check for expected files in the backup
			files := make(map[string]bool)
			for _, f := range zipReader.File {
				files[f.Name] = true
				t.Logf("  - %s (%d bytes)", f.Name, f.UncompressedSize64)
			}

			// config.xml should always be present
			if !files["config.xml"] {
				t.Error("backup does not contain config.xml")
			}
		})
	}
}

// TestBackupContainsPostgresDumps verifies that PostgreSQL instances include database dumps
func TestBackupContainsPostgresDumps(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range testInstances {
		if !inst.isPostgres {
			continue
		}

		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			// Run backup
			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}
			defer reader.Close()

			t.Logf("Backup created: %s", result.Name)

			// Read backup content
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("failed to read backup content: %v", err)
			}

			// Parse ZIP
			zipReader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
			if err != nil {
				t.Fatalf("backup is not a valid ZIP file: %v", err)
			}

			// Look for postgres dump files
			var hasPgDumps bool
			var dumpFiles []string
			for _, f := range zipReader.File {
				if strings.HasPrefix(f.Name, "postgres/") && strings.HasSuffix(f.Name, ".sql") {
					hasPgDumps = true
					dumpFiles = append(dumpFiles, f.Name)
					t.Logf("  Found postgres dump: %s (%d bytes)", f.Name, f.UncompressedSize64)
				}
			}

			if !hasPgDumps {
				t.Error("PostgreSQL backup does not contain postgres/*.sql dump files")
			}

			// Should have at least main and log database dumps
			if len(dumpFiles) < 2 {
				t.Errorf("Expected at least 2 postgres dump files, got %d", len(dumpFiles))
			}
		})
	}
}

// TestBackupSQLiteNoPgDumps verifies that SQLite instances do NOT include postgres dumps
func TestBackupSQLiteNoPgDumps(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range testInstances {
		if inst.isPostgres {
			continue
		}

		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			// Run backup
			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}
			defer reader.Close()

			t.Logf("Backup created: %s", result.Name)

			// Read backup content
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("failed to read backup content: %v", err)
			}

			// Parse ZIP
			zipReader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
			if err != nil {
				t.Fatalf("backup is not a valid ZIP file: %v", err)
			}

			// Should have SQLite database files
			var hasSQLiteDB bool
			for _, f := range zipReader.File {
				if strings.HasSuffix(f.Name, ".db") {
					hasSQLiteDB = true
					t.Logf("  Found SQLite DB: %s (%d bytes)", f.Name, f.UncompressedSize64)
				}
				// Should NOT have postgres directory
				if strings.HasPrefix(f.Name, "postgres/") {
					t.Errorf("SQLite backup should not contain postgres dumps: %s", f.Name)
				}
			}

			if !hasSQLiteDB {
				t.Error("SQLite backup does not contain .db files")
			}
		})
	}
}

// TestBackupConfigXMLParseable verifies that config.xml in backups is valid XML
func TestBackupConfigXMLParseable(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range testInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			// Run backup
			_, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}
			defer reader.Close()

			// Read backup content
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("failed to read backup content: %v", err)
			}

			// Try to parse PostgresConfig from zip (this validates config.xml is parseable)
			pgConfig, err := backup.ParsePostgresConfigFromZip(content)
			if err != nil {
				t.Fatalf("failed to parse config.xml from backup: %v", err)
			}

			if inst.isPostgres {
				if pgConfig == nil {
					t.Error("PostgreSQL instance config.xml should contain postgres configuration")
				} else {
					t.Logf("Postgres config: host=%s port=%s user=%s mainDB=%s logDB=%s",
						pgConfig.Host, pgConfig.Port, pgConfig.User, pgConfig.MainDB, pgConfig.LogDB)
				}
			} else {
				if pgConfig != nil {
					t.Error("SQLite instance config.xml should not contain postgres configuration")
				}
			}
		})
	}
}

// TestClientName verifies the Name() method returns correct values
func TestClientName(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	for _, inst := range testInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			name := client.Name()
			if name != inst.appType {
				t.Errorf("expected Name() to return %q, got %q", inst.appType, name)
			}
		})
	}
}

// TestRestore tests the restore functionality
// This test creates a backup, then restores it
func TestRestore(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Only test SQLite instances since they restart faster and we don't need postgres complexity
	sqliteInstances := []testInstance{}
	for _, inst := range testInstances {
		if !inst.isPostgres {
			sqliteInstances = append(sqliteInstances, inst)
		}
	}

	for _, inst := range sqliteInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			// First, create a backup
			t.Logf("Creating backup...")
			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}

			// Read all backup data into memory
			backupData, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatalf("Failed to read backup data: %v", err)
			}

			t.Logf("Backup created: %s (size: %d bytes)", result.Name, len(backupData))

			// Now restore from the backup
			t.Logf("Restoring from backup...")
			err = client.Restore(ctx, bytes.NewReader(backupData))
			if err != nil {
				t.Fatalf("Restore failed: %v", err)
			}

			t.Logf("Restore completed successfully")

			// Wait for the app to restart and become healthy again
			t.Logf("Waiting for app to restart...")
			time.Sleep(10 * time.Second)

			// Verify the app is back online by trying to create a new client and calling Name()
			// We need to wait a bit more and retry since the app is restarting
			maxRetries := 12
			var lastErr error
			for i := 0; i < maxRetries; i++ {
				newClient := createClient(t, inst)
				if newClient.Name() == inst.appType {
					t.Logf("App is back online after restore")
					lastErr = nil
					break
				}
				lastErr = fmt.Errorf("app not responding correctly after restore")
				time.Sleep(5 * time.Second)
			}

			if lastErr != nil {
				t.Fatalf("App failed to come back online after restore: %v", lastErr)
			}
		})
	}
}

// TestRestorePostgres tests the restore functionality for PostgreSQL instances
// This test creates a backup, then restores it, including the PostgreSQL database dumps
func TestRestorePostgres(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Only test PostgreSQL instances
	pgInstances := []testInstance{}
	for _, inst := range testInstances {
		if inst.isPostgres {
			pgInstances = append(pgInstances, inst)
		}
	}

	for _, inst := range pgInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createClient(t, inst)

			// First, create a backup
			t.Logf("Creating backup...")
			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}

			// Read all backup data into memory
			backupData, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatalf("Failed to read backup data: %v", err)
			}

			t.Logf("Backup created: %s (size: %d bytes)", result.Name, len(backupData))

			// Verify backup contains postgres dumps
			zipReader, err := zip.NewReader(bytes.NewReader(backupData), int64(len(backupData)))
			if err != nil {
				t.Fatalf("Failed to read backup as zip: %v", err)
			}

			var pgDumpCount int
			for _, f := range zipReader.File {
				if strings.HasPrefix(f.Name, "postgres/") && strings.HasSuffix(f.Name, ".sql") {
					pgDumpCount++
					t.Logf("  Found postgres dump: %s (%d bytes)", f.Name, f.UncompressedSize64)
				}
			}

			if pgDumpCount == 0 {
				t.Fatal("Backup should contain postgres dumps for PostgreSQL instance")
			}
			t.Logf("Backup contains %d PostgreSQL dump files", pgDumpCount)

			// Now restore from the backup
			t.Logf("Restoring from backup (including PostgreSQL databases)...")
			err = client.Restore(ctx, bytes.NewReader(backupData))
			if err != nil {
				t.Fatalf("Restore failed: %v", err)
			}

			t.Logf("Restore completed successfully")

			// Wait for the app to restart and become healthy again
			t.Logf("Waiting for app to restart...")
			time.Sleep(15 * time.Second)

			// Verify the app is back online by trying to create a new client and calling Name()
			// We need to wait a bit more and retry since the app is restarting
			maxRetries := 18 // PostgreSQL restores take longer
			var lastErr error
			for i := 0; i < maxRetries; i++ {
				newClient := createClient(t, inst)
				if newClient.Name() == inst.appType {
					t.Logf("App is back online after restore")
					lastErr = nil
					break
				}
				lastErr = fmt.Errorf("app not responding correctly after restore")
				time.Sleep(5 * time.Second)
			}

			if lastErr != nil {
				t.Fatalf("App failed to come back online after restore: %v", lastErr)
			}

			// Create another backup to verify data integrity
			t.Logf("Verifying restore by creating a new backup...")
			result2, reader2, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Post-restore backup failed: %v", err)
			}
			defer reader2.Close()

			backupData2, err := io.ReadAll(reader2)
			if err != nil {
				t.Fatalf("Failed to read post-restore backup: %v", err)
			}

			t.Logf("Post-restore backup created: %s (size: %d bytes)", result2.Name, len(backupData2))

			// Verify post-restore backup also has postgres dumps
			zipReader2, err := zip.NewReader(bytes.NewReader(backupData2), int64(len(backupData2)))
			if err != nil {
				t.Fatalf("Failed to read post-restore backup as zip: %v", err)
			}

			var pgDumpCount2 int
			for _, f := range zipReader2.File {
				if strings.HasPrefix(f.Name, "postgres/") && strings.HasSuffix(f.Name, ".sql") {
					pgDumpCount2++
				}
			}

			if pgDumpCount2 == 0 {
				t.Error("Post-restore backup should contain postgres dumps")
			}
			t.Logf("Post-restore backup contains %d PostgreSQL dump files (verified)", pgDumpCount2)
		})
	}
}
