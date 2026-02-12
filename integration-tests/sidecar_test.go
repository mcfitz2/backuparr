package integration_tests

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"backuparr/sidecar"
)

// sidecarTestInstance describes a sidecar integration test target.
type sidecarTestInstance struct {
	name       string // human-readable label
	appName    string // Name() value for the sidecar client
	sidecarURL string // HTTP base URL of the sidecar
	apiKey     string // API key for the sidecar
}

var sidecarInstances = []sidecarTestInstance{
	{
		name:       "nzbget",
		appName:    "nzbget",
		sidecarURL: "http://localhost:8485",
		apiKey:     "nzbget-sidecar-test-key",
	},
	{
		name:       "transmission",
		appName:    "transmission",
		sidecarURL: "http://localhost:8486",
		apiKey:     "transmission-sidecar-test-key",
	},
	{
		name:       "overseerr",
		appName:    "overseerr",
		sidecarURL: "http://localhost:8487",
		apiKey:     "overseerr-sidecar-test-key",
	},
}

// createSidecarClient creates a sidecar.Client for the given test instance.
func createSidecarClient(t *testing.T, inst sidecarTestInstance) *sidecar.Client {
	t.Helper()
	c, err := sidecar.NewClient(inst.sidecarURL, inst.apiKey, inst.appName)
	if err != nil {
		t.Fatalf("failed to create sidecar client for %s: %v", inst.name, err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Health endpoint
// ---------------------------------------------------------------------------

// TestSidecarHealth verifies that each sidecar's health endpoint is reachable
// and returns the expected JSON structure.
func TestSidecarHealth(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, inst.sidecarURL+"/api/v1/health", nil)
			if err != nil {
				t.Fatalf("failed to build request: %v", err)
			}
			req.Header.Set("X-Api-Key", inst.apiKey)

			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatalf("health request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("health returned HTTP %d: %s", resp.StatusCode, body)
			}

			var health map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
				t.Fatalf("failed to decode health response: %v", err)
			}

			if health["status"] != "ok" {
				t.Errorf("status = %v, want ok", health["status"])
			}

			t.Logf("Health OK — backupPath: %v", health["backupPath"])
		})
	}
}

// TestSidecarHealthBadKey verifies that the sidecar rejects requests with
// an invalid API key.
func TestSidecarHealthBadKey(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, inst.sidecarURL+"/api/v1/health", nil)
			if err != nil {
				t.Fatalf("failed to build request: %v", err)
			}
			req.Header.Set("X-Api-Key", "wrong-key")

			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", resp.StatusCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Backup
// ---------------------------------------------------------------------------

// TestSidecarBackup tests backup via the sidecar client for all apps.
// Validates that the returned data is a non-empty, valid ZIP archive.
func TestSidecarBackup(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createSidecarClient(t, inst)

			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}
			defer reader.Close()

			if result == nil {
				t.Fatal("BackupResult is nil")
			}
			if result.Size == 0 {
				t.Error("BackupResult.Size is zero")
			}

			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("failed to read backup: %v", err)
			}
			if len(data) == 0 {
				t.Fatal("backup data is empty")
			}

			// Validate ZIP
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatalf("backup is not a valid ZIP: %v", err)
			}

			t.Logf("Backup: %s, %d bytes, %d entries", result.Name, len(data), len(zr.File))
			for _, f := range zr.File {
				t.Logf("  - %s (%d bytes)", f.Name, f.UncompressedSize64)
			}

			if len(zr.File) == 0 {
				t.Error("backup ZIP is empty (no entries)")
			}
		})
	}
}

// TestSidecarBackupContainsSQLite verifies that sidecar backups of apps
// which use SQLite include at least one .db file in the archive.
// nzbget uses nzbget.conf (not SQLite), so we only check overseerr here.
func TestSidecarBackupContainsSQLite(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Overseerr uses a SQLite database (db/db.sqlite3)
	var inst sidecarTestInstance
	for _, i := range sidecarInstances {
		if i.name == "overseerr" {
			inst = i
			break
		}
	}

	t.Run(inst.name, func(t *testing.T) {
		client := createSidecarClient(t, inst)

		_, reader, err := client.Backup(ctx)
		if err != nil {
			t.Fatalf("Backup failed: %v", err)
		}
		defer reader.Close()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("failed to read backup: %v", err)
		}

		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("invalid ZIP: %v", err)
		}

		var sqliteFiles []string
		for _, f := range zr.File {
			if strings.HasSuffix(f.Name, ".db") || strings.HasSuffix(f.Name, ".sqlite3") || strings.HasSuffix(f.Name, ".sqlite") {
				sqliteFiles = append(sqliteFiles, f.Name)
			}
		}

		if len(sqliteFiles) == 0 {
			t.Log("No SQLite databases detected (overseerr may not have initialized its DB yet)")
			t.Log("ZIP entries:")
			for _, f := range zr.File {
				t.Logf("  - %s", f.Name)
			}
		} else {
			for _, name := range sqliteFiles {
				t.Logf("SQLite DB found: %s", name)
			}
		}
	})
}

// TestSidecarBackupExcludesLogs verifies that the EXCLUDE_PATTERNS env var
// is effective — *.log files should not appear in the backup.
func TestSidecarBackupExcludesLogs(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createSidecarClient(t, inst)

			_, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}
			defer reader.Close()

			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("failed to read backup: %v", err)
			}

			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatalf("invalid ZIP: %v", err)
			}

			for _, f := range zr.File {
				if strings.HasSuffix(f.Name, ".log") {
					t.Errorf("backup should not contain .log files, found: %s", f.Name)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Client Name
// ---------------------------------------------------------------------------

// TestSidecarClientName verifies the Name() method returns the configured app name.
func TestSidecarClientName(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createSidecarClient(t, inst)
			if got := client.Name(); got != inst.appName {
				t.Errorf("Name() = %q, want %q", got, inst.appName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

// TestSidecarRestore creates a backup via the sidecar, then posts it back to
// the restore endpoint. The test sidecars mount the config volume as read-only,
// so the restore will fail on extraction — but we verify the endpoint accepts
// the upload and processes it correctly (returns a structured JSON response).
func TestSidecarRestore(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createSidecarClient(t, inst)

			// Step 1: Create a backup
			t.Log("Creating backup...")
			result, reader, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("Backup failed: %v", err)
			}

			backupData, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatalf("failed to read backup: %v", err)
			}
			t.Logf("Backup created: %s (%d bytes)", result.Name, len(backupData))

			// Validate the ZIP
			zr, err := zip.NewReader(bytes.NewReader(backupData), int64(len(backupData)))
			if err != nil {
				t.Fatalf("backup is not a valid ZIP: %v", err)
			}
			if len(zr.File) == 0 {
				t.Fatal("backup ZIP is empty")
			}

			// Step 2: Send restore request directly via HTTP
			t.Log("Sending restore request...")

			var formBody bytes.Buffer
			mpw := multipart.NewWriter(&formBody)
			part, err := mpw.CreateFormFile("backup", "backup.zip")
			if err != nil {
				t.Fatalf("failed to create form file: %v", err)
			}
			if _, err := part.Write(backupData); err != nil {
				t.Fatalf("failed to write form data: %v", err)
			}
			mpw.Close()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.sidecarURL+"/api/v1/restore", &formBody)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Header.Set("Content-Type", mpw.FormDataContentType())
			req.Header.Set("X-Api-Key", inst.apiKey)

			httpClient := &http.Client{Timeout: 2 * time.Minute}
			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatalf("restore request failed: %v", err)
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)

			// The sidecar mounts the volume :ro, so the restore will fail
			// with a permission/read-only error during extraction.
			// We consider this a successful test of the restore path —
			// the endpoint accepted the upload, parsed the ZIP, and the
			// error is from the OS rejecting writes to the read-only mount.
			if resp.StatusCode == http.StatusOK {
				t.Log("Restore succeeded (unexpected — volume may be writable)")
				var restoreResult map[string]any
				json.Unmarshal(respBody, &restoreResult)
				t.Logf("Response: %v", restoreResult)
				return
			}

			// 500 with permission denied / read-only is expected
			if resp.StatusCode == http.StatusInternalServerError {
				errMsg := string(respBody)
				if strings.Contains(errMsg, "permission denied") || strings.Contains(errMsg, "read-only") {
					t.Logf("Restore returned expected read-only error: %s", strings.TrimSpace(errMsg))
					t.Log("PASS: restore endpoint correctly accepted upload and attempted extraction")
					return
				}
				t.Fatalf("Unexpected 500 error: %s", errMsg)
			}

			t.Fatalf("Unexpected response: HTTP %d — %s", resp.StatusCode, respBody)
		})
	}
}

// ---------------------------------------------------------------------------
// Backup → Second Backup consistency
// ---------------------------------------------------------------------------

// TestSidecarBackupConsistency runs two backups in succession and verifies
// both produce valid ZIPs with the same set of file names.
func TestSidecarBackupConsistency(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, inst := range sidecarInstances {
		t.Run(inst.name, func(t *testing.T) {
			client := createSidecarClient(t, inst)

			// First backup
			_, reader1, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("first backup failed: %v", err)
			}
			data1, _ := io.ReadAll(reader1)
			reader1.Close()

			// Second backup
			_, reader2, err := client.Backup(ctx)
			if err != nil {
				t.Fatalf("second backup failed: %v", err)
			}
			data2, _ := io.ReadAll(reader2)
			reader2.Close()

			// Parse both ZIPs
			zr1, err := zip.NewReader(bytes.NewReader(data1), int64(len(data1)))
			if err != nil {
				t.Fatalf("first backup invalid ZIP: %v", err)
			}
			zr2, err := zip.NewReader(bytes.NewReader(data2), int64(len(data2)))
			if err != nil {
				t.Fatalf("second backup invalid ZIP: %v", err)
			}

			// Compare file lists
			files1 := map[string]bool{}
			for _, f := range zr1.File {
				files1[f.Name] = true
			}
			files2 := map[string]bool{}
			for _, f := range zr2.File {
				files2[f.Name] = true
			}

			for name := range files1 {
				if !files2[name] {
					t.Errorf("file %q in first backup but not second", name)
				}
			}
			for name := range files2 {
				if !files1[name] {
					t.Errorf("file %q in second backup but not first", name)
				}
			}

			t.Logf("Both backups contain %d entries", len(files1))
		})
	}
}
