// Package truenas implements a backup client for TrueNAS Scale using the
// JSON-RPC 2.0 over WebSocket API.
//
// Backup flow:
//  1. Open a WebSocket to ws(s)://<host>/api/current
//  2. Authenticate with auth.login_with_api_key
//  3. Call core.download("config.save", opts, filename)
//  4. Receive [jobID, downloadURL]
//  5. HTTP POST to <baseURL><downloadURL> to fetch the config backup
//
// Restore flow:
//  1. HTTP POST multipart/form-data to <baseURL>/_upload/
//     - field "data": JSON {"method": "config.upload", "params": []}
//     - field "file": the backup tar/db file
//  2. Response contains {"job_id": N}
//  3. Monitor job via WebSocket core.job_wait(jobID)
//  4. TrueNAS reboots automatically after successful upload
//
// The config.save method produces either:
//   - A raw SQLite database file (when no options are set)
//   - A tar archive containing the database + secrets (when secretseed
//     or root_authorized_keys is true)
//
// Reference: https://api.truenas.com/v26.04/jsonrpc.html
package truenas

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"backuparr/backup"
)

// Verify Client satisfies the backup.Client interface at compile time.
var _ backup.Client = (*Client)(nil)

// Client implements backup.Client for TrueNAS Scale systems.
type Client struct {
	baseURL string // e.g. "http://192.168.1.136"
	apiKey  string // TrueNAS API key (created in UI under Credentials > API Keys)
}

// NewClient creates a TrueNAS backup client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
	}
}

// Name returns the application identifier used for storage paths and logging.
func (c *Client) Name() string { return "truenas" }

// Backup triggers a full TrueNAS configuration backup.
//
// The backup includes the system database, password secret seed, and root
// SSH authorized_keys. The result is a tar archive streamed directly from
// the TrueNAS server.
func (c *Client) Backup(ctx context.Context) (*backup.BackupResult, io.ReadCloser, error) {
	ws, err := c.dialWebSocket(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("websocket connect: %w", err)
	}
	defer ws.close()

	// Authenticate with API key
	var authed bool
	if err := ws.call("auth.login_with_api_key", &authed, c.apiKey); err != nil {
		return nil, nil, fmt.Errorf("auth: %w", err)
	}
	if !authed {
		return nil, nil, fmt.Errorf("authentication failed: API key was rejected")
	}
	log.Printf("[truenas] Authenticated via API key")

	// Trigger config.save through core.download.
	// Options: include secretseed and root SSH keys for a complete backup.
	// With any option set, TrueNAS produces a tar archive instead of a raw .db file.
	saveOpts := map[string]bool{
		"secretseed":           true,
		"root_authorized_keys": true,
	}

	var dlResult [2]json.RawMessage
	err = ws.call("core.download", &dlResult,
		"config.save",        // method: the job to run
		[]any{saveOpts},      // args: passed to config.save as its parameters
		"truenas-config.tar", // filename: suggested Content-Disposition name
		false,                // buffered: false = stream immediately (60s download window)
	)
	if err != nil {
		return nil, nil, fmt.Errorf("core.download: %w", err)
	}

	var jobID int64
	var dlPath string
	if err := json.Unmarshal(dlResult[0], &jobID); err != nil {
		return nil, nil, fmt.Errorf("parse job ID: %w", err)
	}
	if err := json.Unmarshal(dlResult[1], &dlPath); err != nil {
		return nil, nil, fmt.Errorf("parse download path: %w", err)
	}

	log.Printf("[truenas] Config save job %d started, downloading from %s", jobID, dlPath)

	// Fetch the backup file over HTTP. The download URL contains an embedded
	// auth_token so no additional authentication is needed.
	body, size, err := c.httpDownload(ctx, c.baseURL+dlPath)
	if err != nil {
		return nil, nil, fmt.Errorf("download: %w", err)
	}

	return &backup.BackupResult{
		Name:      "truenas-config.tar",
		Size:      size,
		CreatedAt: time.Now(),
	}, body, nil
}

// Restore uploads a configuration backup to TrueNAS via the /_upload endpoint
// and waits for the config.upload job to complete.
//
// After a successful restore TrueNAS will reboot automatically (with a ~10s
// delay). The caller should expect the connection to drop shortly after this
// method returns.
func (c *Client) Restore(ctx context.Context, backup io.Reader) error {
	// Step 1: Upload the file via multipart POST to /_upload/
	jobID, err := c.httpUpload(ctx, backup)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	log.Printf("[truenas] Config upload job %d started, waiting for completion...", jobID)

	// Step 2: Wait for the job to finish via WebSocket
	ws, err := c.dialWebSocket(ctx)
	if err != nil {
		return fmt.Errorf("websocket connect: %w", err)
	}
	defer ws.close()

	var authed bool
	if err := ws.call("auth.login_with_api_key", &authed, c.apiKey); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if !authed {
		return fmt.Errorf("authentication failed: API key was rejected")
	}

	// Poll core.get_jobs until the upload job reaches a terminal state.
	// We can't rely on core.job_wait because it is itself a job method and
	// returns immediately via JSON-RPC before the target job finishes.
	if err := c.waitForJob(ws, jobID); err != nil {
		return fmt.Errorf("config.upload job %d failed: %w", jobID, err)
	}

	log.Printf("[truenas] Config restored successfully (job %d). TrueNAS will reboot shortly.", jobID)
	return nil
}

// jobInfo represents a single job entry from core.get_jobs.
type jobInfo struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	Progress struct {
		Percent     float64 `json:"percent"`
		Description string  `json:"description"`
	} `json:"progress"`
}

// waitForJob polls core.get_jobs until the given job reaches a terminal state
// (SUCCESS, FAILED, or ABORTED). It logs progress updates along the way.
func (c *Client) waitForJob(ws *wsClient, jobID int64) error {
	var lastPct float64

	for {
		// core.get_jobs accepts query-filters; [["id", "=", jobID]] returns just our job.
		var jobs []jobInfo
		if err := ws.call("core.get_jobs", &jobs, [][]any{{"id", "=", jobID}}); err != nil {
			return fmt.Errorf("query job %d: %w", jobID, err)
		}

		if len(jobs) == 0 {
			return fmt.Errorf("job %d not found", jobID)
		}

		job := jobs[0]
		if job.Progress.Percent != lastPct {
			log.Printf("[truenas] Job %d: %.0f%% – %s", jobID, job.Progress.Percent, job.Progress.Description)
			lastPct = job.Progress.Percent
		}

		switch job.State {
		case "SUCCESS":
			return nil
		case "FAILED":
			return fmt.Errorf("%s", job.Error)
		case "ABORTED":
			return fmt.Errorf("job was aborted")
		}

		// Poll every 2 seconds.
		time.Sleep(2 * time.Second)
	}
}

// wsURL returns the WebSocket URL derived from the base HTTP URL.
// http://host  -> ws://host/api/current
// https://host -> wss://host/api/current
func (c *Client) wsURL() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		// Fallback: just replace the scheme prefix
		s := strings.Replace(c.baseURL, "https://", "wss://", 1)
		s = strings.Replace(s, "http://", "ws://", 1)
		return s + "/api/current"
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/api/current"
	return u.String()
}

// --- WebSocket JSON-RPC 2.0 client ---

// wsClient wraps a gorilla/websocket connection with JSON-RPC request/response
// handling and automatic request ID generation.
type wsClient struct {
	conn   *websocket.Conn
	nextID atomic.Int64
}

// jsonRPCRequest is the outgoing JSON-RPC 2.0 request format.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// wsMessage can represent both a JSON-RPC response (has ID + result/error)
// and a notification (has method + params, no ID). We use *int64 for the ID
// to distinguish "ID absent" (notification) from "ID present" (response).
type wsMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (c *Client) dialWebSocket(ctx context.Context) (*wsClient, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	// Support self-signed certs for HTTPS TrueNAS instances
	u, _ := url.Parse(c.baseURL)
	if u != nil && u.Scheme == "https" {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // home lab servers often use self-signed certs
	}

	wsURL := c.wsURL()
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}

	return &wsClient{conn: conn}, nil
}

func (ws *wsClient) close() error {
	return ws.conn.Close()
}

// call sends a JSON-RPC 2.0 request and waits for the matching response,
// silently discarding any interleaved notifications (e.g. collection_update
// events) that TrueNAS may send on the same connection.
func (ws *wsClient) call(method string, result any, params ...any) error {
	id := ws.nextID.Add(1)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	if err := ws.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}

	// Give the server up to 2 minutes to respond (config.save can take a
	// few seconds on large installations).
	ws.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	defer ws.conn.SetReadDeadline(time.Time{}) // clear deadline

	for {
		var msg wsMessage
		if err := ws.conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read %s: %w", method, err)
		}

		// Notifications have no ID - skip them.
		if msg.ID == nil {
			continue
		}
		// Not our response - skip (shouldn't happen with single-caller, but safe).
		if *msg.ID != id {
			continue
		}

		if msg.Error != nil {
			return fmt.Errorf("RPC error %d: %s", msg.Error.Code, msg.Error.Message)
		}

		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	}
}

// httpDownload fetches a file from the TrueNAS download endpoint.
// The URL already contains the auth_token query parameter.
func (c *Client) httpDownload(ctx context.Context, downloadURL string) (io.ReadCloser, int64, error) {
	transport := &http.Transport{}

	// Match the WebSocket TLS policy for the download request.
	u, _ := url.Parse(downloadURL)
	if u != nil && u.Scheme == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	httpClient := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: transport,
	}

	// TrueNAS download endpoints accept POST with an empty body.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, downloadURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	return resp.Body, resp.ContentLength, nil
}

// uploadResponse is the JSON body returned by the /_upload/ endpoint.
type uploadResponse struct {
	JobID int64 `json:"job_id"`
}

// httpUpload posts a config backup file to the TrueNAS /_upload/ endpoint
// as multipart/form-data and returns the resulting job ID.
//
// The multipart body contains two fields:
//   - "data":  JSON object {"method": "config.upload", "params": []}
//   - "file":  the backup file content
func (c *Client) httpUpload(ctx context.Context, file io.Reader) (int64, error) {
	// Build multipart body. We buffer it so we can set Content-Length and
	// Content-Type on the request (TrueNAS rejects chunked uploads).
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// "data" must be the first field per the TrueNAS docs.
	if err := w.WriteField("data", `{"method": "config.upload", "params": []}`); err != nil {
		return 0, fmt.Errorf("write data field: %w", err)
	}

	filePart, err := w.CreateFormFile("file", "truenas-config.tar")
	if err != nil {
		return 0, fmt.Errorf("create file part: %w", err)
	}
	if _, err := io.Copy(filePart, file); err != nil {
		return 0, fmt.Errorf("copy file data: %w", err)
	}

	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("close multipart writer: %w", err)
	}

	uploadURL := c.baseURL + "/_upload/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	transport := &http.Transport{}
	u, _ := url.Parse(uploadURL)
	if u != nil && u.Scheme == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	httpClient := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: transport,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}

	return result.JobID, nil
}
