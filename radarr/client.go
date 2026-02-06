package radarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"backuparr/backup"
)

// Ensure RadarrClient implements backup.Client
var _ backup.Client = (*RadarrClient)(nil)

// RadarrClient wraps the generated radarr.Client with API key authentication
type RadarrClient struct {
	client     *Client
	baseURL    string
	apiKey     string
	username   string
	password   string
	httpClient *http.Client           // Shared HTTP client with cookie jar for session auth
	pgOverride *backup.PostgresConfig // Optional postgres config override
}

// NewRadarrClient creates a new Radarr API client with API key authentication
func NewRadarrClient(baseURL, apiKey, username, password string, pgOverride *backup.PostgresConfig) (*RadarrClient, error) {
	// Create a cookie jar for session management
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create cookie jar: %w", err)
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
	}

	client, err := NewClient(baseURL,
		WithHTTPClient(httpClient),
		WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("X-Api-Key", apiKey)
			req.Header.Set("Content-Type", "application/json")
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create radarr client: %w", err)
	}

	return &RadarrClient{
		client:     client,
		baseURL:    baseURL,
		apiKey:     apiKey,
		username:   username,
		password:   password,
		httpClient: httpClient,
		pgOverride: pgOverride,
	}, nil
}

// Name returns the application name
func (c *RadarrClient) Name() string {
	return "radarr"
}

// Backup triggers a backup and returns the backup file content
func (c *RadarrClient) Backup(ctx context.Context) (*backup.BackupResult, io.ReadCloser, error) {
	// Trigger the backup command and wait for completion
	if err := c.runBackupCommand(ctx); err != nil {
		return nil, nil, fmt.Errorf("backup command failed: %w", err)
	}

	// Get the latest backup file
	backups, err := c.getBackupFiles(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get backup files: %w", err)
	}

	if len(backups) == 0 {
		return nil, nil, fmt.Errorf("no backup files found after backup command")
	}

	// Get the most recent backup (first in the list)
	latest := backups[0]

	// Download the backup file into memory
	reader, err := c.downloadBackup(ctx, latest.Path, derefInt64(latest.Size))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to download backup: %w", err)
	}

	// Read the entire backup into memory for processing
	backupData, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read backup data: %w", err)
	}

	// Check if this instance uses PostgreSQL
	dbType, err := c.getDatabaseType(ctx)
	if err != nil {
		log.Printf("[radarr] Warning: could not determine database type: %v", err)
	}

	var finalBackupData []byte
	if dbType == "postgreSQL" {
		log.Printf("[radarr] PostgreSQL detected, extracting connection info and dumping databases...")

		// Parse Postgres config from the backup's config.xml
		pgConfig, err := backup.ParsePostgresConfigFromZip(backupData)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse postgres config: %w", err)
		}

		// Apply overrides from config if specified
		if pgConfig != nil && c.pgOverride != nil {
			log.Printf("[radarr] Applying postgres config overrides from config.yml")
			if c.pgOverride.Host != "" {
				pgConfig.Host = c.pgOverride.Host
			}
			if c.pgOverride.Port != "" {
				pgConfig.Port = c.pgOverride.Port
			}
			if c.pgOverride.User != "" {
				pgConfig.User = c.pgOverride.User
			}
			if c.pgOverride.Password != "" {
				pgConfig.Password = c.pgOverride.Password
			}
			if c.pgOverride.MainDB != "" {
				pgConfig.MainDB = c.pgOverride.MainDB
			}
			if c.pgOverride.LogDB != "" {
				pgConfig.LogDB = c.pgOverride.LogDB
			}
		} else if pgConfig == nil && c.pgOverride != nil {
			// Use override as the full config if no config.xml found
			pgConfig = c.pgOverride
		}

		if pgConfig != nil {
			log.Printf("[radarr] Using postgres host: %s:%s", pgConfig.Host, pgConfig.Port)

			// Dump all Postgres databases
			dumps, err := pgConfig.DumpAllDatabases()
			if err != nil {
				return nil, nil, fmt.Errorf("failed to dump postgres databases: %w", err)
			}

			log.Printf("[radarr] Dumped %d databases, creating enhanced backup...", len(dumps))

			// Create enhanced backup with pg_dump files
			finalBackupData, err = backup.CreateEnhancedBackup(backupData, dumps)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create enhanced backup: %w", err)
			}
		} else {
			finalBackupData = backupData
		}
	} else {
		finalBackupData = backupData
	}

	result := &backup.BackupResult{
		Name:      derefString(latest.Name),
		Path:      derefString(latest.Path),
		Size:      int64(len(finalBackupData)),
		CreatedAt: derefTime(latest.Time),
	}

	return result, io.NopCloser(bytes.NewReader(finalBackupData)), nil
}

// Restore restores the application from a backup file
func (c *RadarrClient) Restore(ctx context.Context, backupData io.Reader) error {
	log.Printf("[%s] Reading backup data...", c.Name())

	// Read all backup data into memory so we can analyze it
	zipData, err := io.ReadAll(backupData)
	if err != nil {
		return fmt.Errorf("failed to read backup data: %w", err)
	}

	// Check if backup contains PostgreSQL dumps
	pgDumps, err := backup.ExtractPostgresDumpsFromZip(zipData)
	if err != nil {
		return fmt.Errorf("failed to extract postgres dumps: %w", err)
	}

	if len(pgDumps) > 0 {
		log.Printf("[%s] PostgreSQL backup detected with %d database dumps", c.Name(), len(pgDumps))

		// Parse postgres config from the backup's config.xml
		pgConfig, err := backup.ParsePostgresConfigFromZip(zipData)
		if err != nil {
			return fmt.Errorf("failed to parse postgres config from backup: %w", err)
		}

		if pgConfig == nil {
			return fmt.Errorf("backup contains postgres dumps but config.xml has no postgres settings")
		}

		// Apply postgres config overrides if specified
		if c.pgOverride != nil {
			if c.pgOverride.Host != "" {
				pgConfig.Host = c.pgOverride.Host
			}
			if c.pgOverride.Port != "" {
				pgConfig.Port = c.pgOverride.Port
			}
			if c.pgOverride.User != "" {
				pgConfig.User = c.pgOverride.User
			}
			if c.pgOverride.Password != "" {
				pgConfig.Password = c.pgOverride.Password
			}
			log.Printf("[%s] Applying postgres config overrides, using host: %s:%s", c.Name(), pgConfig.Host, pgConfig.Port)
		}

		// Restore PostgreSQL databases
		log.Printf("[%s] Restoring PostgreSQL databases...", c.Name())
		for filename, data := range pgDumps {
			log.Printf("[%s] Restoring %s (%d bytes)...", c.Name(), filename, len(data))
		}

		if err := pgConfig.RestoreAllDatabases(pgDumps); err != nil {
			return fmt.Errorf("failed to restore postgres databases: %w", err)
		}
		log.Printf("[%s] PostgreSQL databases restored successfully", c.Name())
	}

	// Now upload the backup to the API (handles config.xml)
	log.Printf("[%s] Uploading backup for restore...", c.Name())

	// Create multipart form data
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Create form file field named "restore"
	part, err := writer.CreateFormFile("restore", "backup.zip")
	if err != nil {
		return fmt.Errorf("failed to create form file: %w", err)
	}

	// Write backup data to form field
	if _, err := part.Write(zipData); err != nil {
		return fmt.Errorf("failed to write backup data: %w", err)
	}

	// Close the multipart writer to finalize the form
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Build request URL
	reqURL := fmt.Sprintf("%s/api/v3/system/backup/restore/upload", strings.TrimSuffix(c.baseURL, "/"))

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, &buf)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload backup: %w", err)
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("restore upload failed: %d - %s", resp.StatusCode, string(body))
	}

	// Parse response
	var result struct {
		RestartRequired bool `json:"RestartRequired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[%s] Warning: failed to parse restore response: %v", c.Name(), err)
	}

	log.Printf("[%s] Backup uploaded successfully. Restart required: %v", c.Name(), result.RestartRequired)

	if result.RestartRequired {
		log.Printf("[%s] Triggering application restart...", c.Name())
		if err := c.restart(ctx); err != nil {
			return fmt.Errorf("failed to restart after restore: %w", err)
		}
		log.Printf("[%s] Restart triggered successfully", c.Name())
	}

	return nil
}

// Internal methods

func (c *RadarrClient) runBackupCommand(ctx context.Context) error {
	cmdName := "Backup"
	cmdBody := CommandResource{
		Name: &cmdName,
	}

	resp, err := c.client.PostApiV3Command(ctx, cmdBody)
	if err != nil {
		return fmt.Errorf("failed to send command: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var cmdResp CommandResource
	if err := json.NewDecoder(resp.Body).Decode(&cmdResp); err != nil {
		return fmt.Errorf("failed to decode command response: %w", err)
	}

	if cmdResp.Id == nil {
		return fmt.Errorf("command response has no ID")
	}

	// Poll for command completion
	return c.waitForCommand(ctx, *cmdResp.Id)
}

func (c *RadarrClient) waitForCommand(ctx context.Context, commandID int32) error {
	for {
		resp, err := c.client.GetApiV3CommandId(ctx, commandID)
		if err != nil {
			return fmt.Errorf("failed to get command status: %w", err)
		}

		var cmdResp CommandResource
		if err := json.NewDecoder(resp.Body).Decode(&cmdResp); err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to decode command status: %w", err)
		}
		resp.Body.Close()

		if cmdResp.Status != nil {
			log.Printf("[radarr] Command status: %s", *cmdResp.Status)

			switch *cmdResp.Status {
			case CommandStatusCompleted:
				return nil
			case CommandStatusFailed:
				msg := ""
				if cmdResp.Message != nil {
					msg = *cmdResp.Message
				}
				return fmt.Errorf("command failed: %s", msg)
			case CommandStatusCancelled:
				return fmt.Errorf("command was cancelled")
			case CommandStatusAborted:
				return fmt.Errorf("command was aborted")
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			// Continue polling
		}
	}
}

func (c *RadarrClient) getBackupFiles(ctx context.Context) ([]BackupResource, error) {
	resp, err := c.client.GetApiV3SystemBackup(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get backups: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var backups []BackupResource
	if err := json.NewDecoder(resp.Body).Decode(&backups); err != nil {
		return nil, fmt.Errorf("failed to decode backups response: %w", err)
	}

	return backups, nil
}

func (c *RadarrClient) getDatabaseType(ctx context.Context) (string, error) {
	resp, err := c.client.GetApiV3SystemStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get system status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var status SystemResource
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("failed to decode system status: %w", err)
	}

	if status.DatabaseType == nil {
		return "sqLite", nil // Default to SQLite
	}

	return string(*status.DatabaseType), nil
}

func (c *RadarrClient) downloadBackup(ctx context.Context, backupPath *string, expectedSize int64) (io.ReadCloser, error) {
	if backupPath == nil || *backupPath == "" {
		return nil, fmt.Errorf("backup path is empty")
	}

	// Get the authentication method from config
	authMethod, err := c.getAuthMethod(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth method: %w", err)
	}

	log.Printf("[radarr] Authentication method: %s", authMethod)

	// Handle authentication based on method
	switch strings.ToLower(authMethod) {
	case "forms":
		if err := c.loginWithForms(ctx); err != nil {
			return nil, fmt.Errorf("forms login failed: %w", err)
		}
	case "basic":
		// Basic auth will be handled in the request
	case "none", "external":
		// No authentication needed or handled externally
	default:
		log.Printf("[radarr] Unknown auth method: %s, proceeding without session auth", authMethod)
	}

	// Download the backup using the session
	downloadURL := fmt.Sprintf("%s%s", c.baseURL, *backupPath)
	log.Printf("[radarr] Downloading backup from: %s", downloadURL)

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add API key header as backup
	req.Header.Set("X-Api-Key", c.apiKey)

	// For basic auth, add the credentials
	if strings.ToLower(authMethod) == "basic" && c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	// Use a client with longer timeout for downloads but share the cookie jar
	downloadClient := &http.Client{
		Timeout: 5 * time.Minute,
		Jar:     c.httpClient.Jar,
	}

	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download backup: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("download error: %d - %s", resp.StatusCode, string(body))
	}

	// Verify content type is a zip file
	contentType := resp.Header.Get("Content-Type")
	validTypes := []string{"application/zip", "application/octet-stream", "application/x-zip-compressed", "application/x-zip"}
	isValidType := contentType == ""
	for _, t := range validTypes {
		if contentType == t {
			isValidType = true
			break
		}
	}
	if !isValidType {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected content type: %s (expected application/zip)", contentType)
	}

	// Verify content length matches expected size
	if resp.ContentLength > 0 && expectedSize > 0 && resp.ContentLength != expectedSize {
		log.Printf("[radarr] Content length mismatch: got %d, expected %d (continuing anyway)", resp.ContentLength, expectedSize)
	}

	return resp.Body, nil
}

func (c *RadarrClient) getAuthMethod(ctx context.Context) (string, error) {
	resp, err := c.client.GetApiV3ConfigHost(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get host config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var config HostConfigResource
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return "", fmt.Errorf("failed to decode host config: %w", err)
	}

	if config.AuthenticationMethod == nil {
		return "none", nil
	}

	return string(*config.AuthenticationMethod), nil
}

func (c *RadarrClient) loginWithForms(ctx context.Context) error {
	loginURL := fmt.Sprintf("%s/login", c.baseURL)

	formData := url.Values{}
	formData.Set("username", c.username)
	formData.Set("password", c.password)

	req, err := http.NewRequestWithContext(ctx, "POST", loginURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	// A successful login typically returns 200 or redirects (302)
	if resp.StatusCode == 401 {
		return fmt.Errorf("login failed: invalid credentials")
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("[radarr] Forms login successful")
	return nil
}

func (c *RadarrClient) restart(ctx context.Context) error {
	resp, err := c.client.PostApiV3SystemRestart(ctx)
	if err != nil {
		return fmt.Errorf("failed to send restart command: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("restart command failed: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

// Helper functions
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
