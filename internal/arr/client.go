// Package arr holds the logic shared by the sonarr, radarr and prowlarr
// clients: triggering and polling a backup command, selecting and
// downloading the newest backup file, session authentication, and restore
// upload. Each of those packages differs only in its generated API client
// (distinct types and versioned operation names), so they each supply a
// thin Operations adapter and get everything else from Client.
package arr

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
	"sort"
	"strings"
	"time"

	"backuparr/internal/backup"
)

// BackupFile is the neutral decode target for a backup-list entry. The JSON
// tags mirror the generated BackupResource type in each package, which are
// identical across sonarr, radarr and prowlarr.
type BackupFile struct {
	Name *string    `json:"name"`
	Path *string    `json:"path"`
	Size *int64     `json:"size,omitempty"`
	Time *time.Time `json:"time,omitempty"`
}

// commandStatus is the neutral decode target for command POST/GET
// responses; mirrors the generated CommandResource JSON shape.
type commandStatus struct {
	Id      *int32  `json:"id,omitempty"`
	Status  *string `json:"status,omitempty"`
	Message *string `json:"message"`
}

// Config holds everything a Client needs beyond the Operations adapter.
type Config struct {
	// AppName is used both as the backup.Client Name() and as the "[name]"
	// log line prefix.
	AppName string
	// APIVersion is the API path segment ("v3" or "v1") used to build the
	// restore-upload URL, which is issued as a raw HTTP request rather than
	// through Operations because the generated upload operation doesn't
	// accept a body.
	APIVersion string
	BaseURL    string
	APIKey     string
	Username   string
	Password   string
	// HTTPClient must share a cookie jar with the generated client so forms
	// session auth (set up by loginWithForms) is visible to it.
	HTTPClient *http.Client
	// PgOverride is optional; when nil Postgres handling behaves exactly as
	// if no override were configured. Callers whose backend has no
	// Postgres support (prowlarr) simply never set it and never implement
	// DatabaseTyper.
	PgOverride *backup.PostgresConfig
	Ops        Operations
}

// Client implements backup.Client using the shared *arr backup/restore flow
// against whatever Operations adapter it's given.
type Client struct {
	appName    string
	apiVersion string
	baseURL    string
	apiKey     string
	username   string
	password   string
	httpClient *http.Client
	pgOverride *backup.PostgresConfig
	ops        Operations
}

// NewClient builds a shared Client from Config.
func NewClient(cfg Config) *Client {
	return &Client{
		appName:    cfg.AppName,
		apiVersion: cfg.APIVersion,
		baseURL:    cfg.BaseURL,
		apiKey:     cfg.APIKey,
		username:   cfg.Username,
		password:   cfg.Password,
		httpClient: cfg.HTTPClient,
		pgOverride: cfg.PgOverride,
		ops:        cfg.Ops,
	}
}

// NewSessionHTTPClient builds the cookie-jar-backed, retry-wrapped HTTP
// client every *arr constructor needs: the jar carries forms-auth session
// cookies across the generated client and the manual download/upload/login
// requests, and the retry transport applies uniformly to all of them.
func NewSessionHTTPClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create cookie jar: %w", err)
	}

	return &http.Client{
		Timeout:   2 * time.Minute,
		Jar:       jar,
		Transport: backup.NewRetryTransport(nil),
	}, nil
}

// Name returns the application name.
func (c *Client) Name() string {
	return c.appName
}

// APIKey, Username, Password, and BaseURL expose the credential fields for
// callers (namely tests) that need to verify constructor wiring without
// duplicating those fields on the wrapping per-package client.
func (c *Client) APIKey() string   { return c.apiKey }
func (c *Client) Username() string { return c.username }
func (c *Client) Password() string { return c.password }
func (c *Client) BaseURL() string  { return c.baseURL }

// Backup triggers a backup and returns the backup file content.
func (c *Client) Backup(ctx context.Context) (*backup.BackupResult, io.ReadCloser, error) {
	// Snapshot the newest known backup before triggering. The trigger+poll
	// flow can report completion before the new backup file actually shows
	// up in the list, so we need something to compare against afterward.
	existingBackups, err := c.getBackupFiles(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get backup files: %w", err)
	}
	hasPreviousBackup := len(existingBackups) > 0
	var previousNewest time.Time
	if hasPreviousBackup {
		sortBackupsByTimeDesc(existingBackups)
		previousNewest = derefTime(existingBackups[0].Time)
	}

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

	// Sort newest-first rather than trusting the API's ordering.
	sortBackupsByTimeDesc(backups)
	latest := backups[0]

	// If there was already at least one backup before we triggered, the
	// selected backup must be strictly newer than it. Otherwise we'd upload
	// and timestamp a stale (pre-existing) backup as if it were fresh.
	if hasPreviousBackup && !derefTime(latest.Time).After(previousNewest) {
		return nil, nil, fmt.Errorf("%s: no fresh backup found after triggering backup: latest backup time %s is not after previous newest %s", c.appName, derefTime(latest.Time), previousNewest)
	}

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

	// Postgres detection is a capability, not every backend has it: only
	// clients whose Operations adapter implements DatabaseTyper (sonarr,
	// radarr) even attempt the lookup.
	finalBackupData := backupData
	if dt, ok := c.ops.(DatabaseTyper); ok {
		dbType, err := c.getDatabaseType(ctx, dt)
		if err != nil {
			log.Printf("[%s] Warning: could not determine database type: %v", c.appName, err)
		}

		if dbType == "postgreSQL" {
			finalBackupData, err = c.applyPostgres(backupData)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	result := &backup.BackupResult{
		Name:      derefString(latest.Name),
		Path:      derefString(latest.Path),
		Size:      int64(len(finalBackupData)),
		CreatedAt: derefTime(latest.Time),
	}

	return result, io.NopCloser(bytes.NewReader(finalBackupData)), nil
}

// applyPostgres parses the Postgres connection info out of the backup's
// config.xml (merging in any configured override), dumps all databases, and
// returns the backup with the dumps folded in. It returns backupData
// unmodified if there's no usable Postgres config at all.
func (c *Client) applyPostgres(backupData []byte) ([]byte, error) {
	log.Printf("[%s] PostgreSQL detected, extracting connection info and dumping databases...", c.appName)

	pgConfig, err := backup.ParsePostgresConfigFromZip(backupData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse postgres config: %w", err)
	}

	if pgConfig != nil && c.pgOverride != nil {
		log.Printf("[%s] Applying postgres config overrides from config.yml", c.appName)
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

	if pgConfig == nil {
		return backupData, nil
	}

	log.Printf("[%s] Using postgres host: %s:%s", c.appName, pgConfig.Host, pgConfig.Port)

	dumps, err := pgConfig.DumpAllDatabases()
	if err != nil {
		return nil, fmt.Errorf("failed to dump postgres databases: %w", err)
	}

	log.Printf("[%s] Dumped %d databases, creating enhanced backup...", c.appName, len(dumps))

	enhanced, err := backup.CreateEnhancedBackup(backupData, dumps)
	if err != nil {
		return nil, fmt.Errorf("failed to create enhanced backup: %w", err)
	}

	return enhanced, nil
}

// Restore restores the application from a backup file.
func (c *Client) Restore(ctx context.Context, backupData io.Reader) error {
	log.Printf("[%s] Reading backup data...", c.appName)

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
		log.Printf("[%s] PostgreSQL backup detected with %d database dumps", c.appName, len(pgDumps))

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
			log.Printf("[%s] Applying postgres config overrides, using host: %s:%s", c.appName, pgConfig.Host, pgConfig.Port)
		}

		// Restore PostgreSQL databases
		log.Printf("[%s] Restoring PostgreSQL databases...", c.appName)
		for filename, data := range pgDumps {
			log.Printf("[%s] Restoring %s (%d bytes)...", c.appName, filename, len(data))
		}

		if err := pgConfig.RestoreAllDatabases(pgDumps); err != nil {
			return fmt.Errorf("failed to restore postgres databases: %w", err)
		}
		log.Printf("[%s] PostgreSQL databases restored successfully", c.appName)
	} else {
		log.Printf("[%s] No PostgreSQL dumps found in backup (SQLite-only backup)", c.appName)
	}

	// Now upload the backup to the API (handles config.xml)
	log.Printf("[%s] Uploading backup for restore...", c.appName)

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

	// Build request URL. This isn't routed through Operations because the
	// generated upload operation for this endpoint takes no body.
	reqURL := fmt.Sprintf("%s/api/%s/system/backup/restore/upload", strings.TrimSuffix(c.baseURL, "/"), c.apiVersion)

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, &buf)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Use a client with longer timeout for uploads but share the cookie jar
	uploadClient := &http.Client{
		Timeout:   5 * time.Minute,
		Jar:       c.httpClient.Jar,
		Transport: backup.NewRetryTransport(nil),
	}

	// Send request
	resp, err := uploadClient.Do(req)
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
		log.Printf("[%s] Warning: failed to parse restore response: %v", c.appName, err)
	}

	log.Printf("[%s] Backup uploaded successfully. Restart required: %v", c.appName, result.RestartRequired)

	if result.RestartRequired {
		log.Printf("[%s] Triggering application restart...", c.appName)
		if err := c.restart(ctx); err != nil {
			return fmt.Errorf("failed to restart after restore: %w", err)
		}
		log.Printf("[%s] Restart triggered successfully", c.appName)
	}

	return nil
}

// Internal methods

func (c *Client) runBackupCommand(ctx context.Context) error {
	resp, err := c.ops.PostCommand(ctx, "Backup")
	if err != nil {
		return fmt.Errorf("failed to send command: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var cmdResp commandStatus
	if err := json.NewDecoder(resp.Body).Decode(&cmdResp); err != nil {
		return fmt.Errorf("failed to decode command response: %w", err)
	}

	if cmdResp.Id == nil {
		return fmt.Errorf("command response has no ID")
	}

	// Poll for command completion
	return c.waitForCommand(ctx, *cmdResp.Id)
}

func (c *Client) waitForCommand(ctx context.Context, commandID int32) error {
	for {
		resp, err := c.ops.GetCommand(ctx, commandID)
		if err != nil {
			return fmt.Errorf("failed to get command status: %w", err)
		}

		var cmdResp commandStatus
		if err := json.NewDecoder(resp.Body).Decode(&cmdResp); err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to decode command status: %w", err)
		}
		resp.Body.Close()

		if cmdResp.Status != nil {
			log.Printf("[%s] Command status: %s", c.appName, *cmdResp.Status)

			switch *cmdResp.Status {
			case "completed":
				return nil
			case "failed":
				msg := ""
				if cmdResp.Message != nil {
					msg = *cmdResp.Message
				}
				return fmt.Errorf("command failed: %s", msg)
			case "cancelled":
				return fmt.Errorf("command was cancelled")
			case "aborted":
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

func (c *Client) getBackupFiles(ctx context.Context) ([]BackupFile, error) {
	resp, err := c.ops.GetSystemBackup(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get backups: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var backups []BackupFile
	if err := json.NewDecoder(resp.Body).Decode(&backups); err != nil {
		return nil, fmt.Errorf("failed to decode backups response: %w", err)
	}

	return backups, nil
}

func (c *Client) getDatabaseType(ctx context.Context, dt DatabaseTyper) (string, error) {
	resp, err := dt.GetSystemStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get system status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var status struct {
		DatabaseType *string `json:"databaseType,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("failed to decode system status: %w", err)
	}

	if status.DatabaseType == nil {
		return "sqLite", nil // Default to SQLite
	}

	return *status.DatabaseType, nil
}

func (c *Client) downloadBackup(ctx context.Context, backupPath *string, expectedSize int64) (io.ReadCloser, error) {
	if backupPath == nil || *backupPath == "" {
		return nil, fmt.Errorf("backup path is empty")
	}

	// Get the authentication method from config
	authMethod, err := c.getAuthMethod(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth method: %w", err)
	}

	log.Printf("[%s] Authentication method: %s", c.appName, authMethod)

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
		log.Printf("[%s] Unknown auth method: %s, proceeding without session auth", c.appName, authMethod)
	}

	// Download the backup using the session
	downloadURL := fmt.Sprintf("%s%s", c.baseURL, *backupPath)
	log.Printf("[%s] Downloading backup from: %s", c.appName, downloadURL)

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
		Timeout:   5 * time.Minute,
		Jar:       c.httpClient.Jar,
		Transport: backup.NewRetryTransport(nil),
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
		log.Printf("[%s] Content length mismatch: got %d, expected %d (continuing anyway)", c.appName, resp.ContentLength, expectedSize)
	}

	return resp.Body, nil
}

func (c *Client) getAuthMethod(ctx context.Context) (string, error) {
	resp, err := c.ops.GetConfigHost(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get host config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var config struct {
		AuthenticationMethod *string `json:"authenticationMethod,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return "", fmt.Errorf("failed to decode host config: %w", err)
	}

	if config.AuthenticationMethod == nil {
		return "none", nil
	}

	return *config.AuthenticationMethod, nil
}

func (c *Client) loginWithForms(ctx context.Context) error {
	loginURL := fmt.Sprintf("%s/login", c.baseURL)

	formData := url.Values{}
	formData.Set("username", c.username)
	formData.Set("password", c.password)

	req, err := http.NewRequestWithContext(ctx, "POST", loginURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Use a client that doesn't follow redirects so we can inspect the response
	noRedirectClient := &http.Client{
		Timeout:   2 * time.Minute,
		Jar:       c.httpClient.Jar,
		Transport: backup.NewRetryTransport(nil),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("login failed: invalid credentials")
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Verify the auth cookie was actually set — the *arr apps return 200/302
	// even on failed login, but only set the auth cookie on success
	parsedURL, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("failed to parse base URL: %w", err)
	}

	var hasAuthCookie bool
	for _, cookie := range c.httpClient.Jar.Cookies(parsedURL) {
		if strings.HasSuffix(cookie.Name, "Auth") {
			hasAuthCookie = true
			break
		}
	}

	if !hasAuthCookie {
		return fmt.Errorf("login failed: no auth cookie received (check username/password)")
	}

	log.Printf("[%s] Forms login successful", c.appName)
	return nil
}

func (c *Client) restart(ctx context.Context) error {
	resp, err := c.ops.PostSystemRestart(ctx)
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

// sortBackupsByTimeDesc orders backups newest-first by Time, rather than
// trusting the order the API returns. A nil Time is treated as the oldest
// possible value, so such entries sort last and are never picked as newest.
func sortBackupsByTimeDesc(backups []BackupFile) {
	sort.SliceStable(backups, func(i, j int) bool {
		return derefTime(backups[i].Time).After(derefTime(backups[j].Time))
	})
}
