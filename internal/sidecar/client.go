// Package sidecar implements a backup.Client that communicates with the
// backuparr sidecar HTTP server. This allows backuparr to back up and restore
// applications that don't have their own built-in backup/restore API
// (e.g., nzbget, transmission, overseerr).
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"time"

	"backuparr/internal/backup"
)

// Client implements backup.Client by talking to a sidecar HTTP server.
type Client struct {
	baseURL      string
	apiKey       string
	appName      string
	httpClient   *http.Client
	backupClient *http.Client
	uploadClient *http.Client
}

// NewClient creates a new sidecar client.
func NewClient(baseURL, apiKey, appName string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("sidecar URL is required")
	}
	if appName == "" {
		return nil, fmt.Errorf("app name is required for sidecar client")
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		appName: appName,
		httpClient: &http.Client{
			Timeout:   2 * time.Minute,
			Transport: backup.NewRetryTransport(nil),
		},
		backupClient: &http.Client{
			Timeout:   5 * time.Minute,
			Transport: backup.NewRetryTransport(nil),
		},
		uploadClient: &http.Client{
			Timeout:   5 * time.Minute,
			Transport: backup.NewRetryTransport(nil),
		},
	}, nil
}

// Name returns the configured application name.
func (c *Client) Name() string {
	return c.appName
}

// Backup triggers a backup on the sidecar and returns the ZIP data.
func (c *Client) Backup(ctx context.Context) (*backup.BackupResult, io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/backup", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create backup request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.backupClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("backup request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, nil, fmt.Errorf("backup failed (HTTP %d): %s", resp.StatusCode, body)
	}

	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read backup response: %w", err)
	}

	result := &backup.BackupResult{
		Name:      fmt.Sprintf("%s-sidecar-backup", c.appName),
		Size:      int64(len(data)),
		CreatedAt: time.Now(),
	}

	log.Printf("[%s] Sidecar backup received: %d bytes", c.appName, len(data))
	return result, io.NopCloser(bytes.NewReader(data)), nil
}

// Restore uploads a backup ZIP to the sidecar for extraction.
func (c *Client) Restore(ctx context.Context, backupData io.Reader) error {
	data, err := io.ReadAll(backupData)
	if err != nil {
		return fmt.Errorf("failed to read backup data: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("backup", "backup.zip")
	if err != nil {
		return fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("failed to write form data: %w", err)
	}
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/restore", &body)
	if err != nil {
		return fmt.Errorf("failed to create restore request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.setHeaders(req)

	resp, err := c.uploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("restore upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("restore failed (HTTP %d): %s", resp.StatusCode, respBody)
	}

	var result struct {
		Success       bool   `json:"success"`
		FilesRestored int    `json:"filesRestored"`
		Message       string `json:"message"`
		Restart       struct {
			Attempted bool   `json:"attempted"`
			Success   bool   `json:"success"`
			Method    string `json:"method"`
			Error     string `json:"error"`
		} `json:"restart"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil {
		log.Printf("[%s] %s", c.appName, result.Message)
		if result.Restart.Attempted && !result.Restart.Success {
			log.Printf("[%s] Warning: restart failed — please restart the app manually", c.appName)
		}
	}

	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
}
