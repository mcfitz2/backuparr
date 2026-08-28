package prowlarr

import (
	"context"
	"fmt"
	"net/http"

	"backuparr/internal/arr"
	"backuparr/internal/backup"
)

// Ensure ProwlarrClient implements backup.Client
var _ backup.Client = (*ProwlarrClient)(nil)

// ProwlarrClient wraps the generated prowlarr.Client with API key authentication.
// All backup/restore logic lives in arr.Client; this type only adapts the
// generated operations it needs. Prowlarr has no Postgres support, so its
// adapter deliberately doesn't implement arr.DatabaseTyper.
type ProwlarrClient struct {
	*arr.Client
}

// opsAdapter satisfies arr.Operations against the generated prowlarr client.
type opsAdapter struct {
	client *Client
}

func (a *opsAdapter) PostCommand(ctx context.Context, name string) (*http.Response, error) {
	return a.client.PostApiV1Command(ctx, CommandResource{Name: &name})
}

func (a *opsAdapter) GetCommand(ctx context.Context, id int32) (*http.Response, error) {
	return a.client.GetApiV1CommandId(ctx, id)
}

func (a *opsAdapter) GetConfigHost(ctx context.Context) (*http.Response, error) {
	return a.client.GetApiV1ConfigHost(ctx)
}

func (a *opsAdapter) GetSystemBackup(ctx context.Context) (*http.Response, error) {
	return a.client.GetApiV1SystemBackup(ctx)
}

func (a *opsAdapter) PostSystemRestart(ctx context.Context) (*http.Response, error) {
	return a.client.PostApiV1SystemRestart(ctx)
}

// NewProwlarrClient creates a new Prowlarr API client with API key authentication
func NewProwlarrClient(baseURL, apiKey, username, password string) (*ProwlarrClient, error) {
	httpClient, err := arr.NewSessionHTTPClient()
	if err != nil {
		return nil, err
	}

	genClient, err := NewClient(baseURL,
		WithHTTPClient(httpClient),
		WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("X-Api-Key", apiKey)
			req.Header.Set("Content-Type", "application/json")
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create prowlarr client: %w", err)
	}

	return &ProwlarrClient{
		Client: arr.NewClient(arr.Config{
			AppName:    "prowlarr",
			APIVersion: "v1",
			BaseURL:    baseURL,
			APIKey:     apiKey,
			Username:   username,
			Password:   password,
			HTTPClient: httpClient,
			Ops:        &opsAdapter{client: genClient},
		}),
	}, nil
}
