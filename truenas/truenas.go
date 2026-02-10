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
// The config.save method produces either:
//   - A raw SQLite database file (when no options are set)
//   - A tar archive containing the database + secrets (when secretseed
//     or root_authorized_keys is true)
//
// Reference: https://api.truenas.com/v26.04/jsonrpc.html
package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// Restore is not yet supported for TrueNAS.
//
// TrueNAS configuration restore requires the config.upload WebSocket method
// which uses a different multipart upload flow. For now, use the TrueNAS
// web UI (System > General > Manage Configuration > Upload Config) to
// restore backups.
func (c *Client) Restore(_ context.Context, _ io.Reader) error {
	return fmt.Errorf("truenas restore is not yet implemented; use the TrueNAS web UI to restore config backups")
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
