package sonarr

import (
	"context"
	"fmt"
	"net/http"

	"backuparr/internal/arr"
	"backuparr/internal/backup"
)

// Ensure SonarrClient implements backup.Client
var _ backup.Client = (*SonarrClient)(nil)

// SonarrClient wraps the generated sonarr.Client with API key authentication.
// All backup/restore logic lives in arr.Client; this type only adapts the
// generated operations it needs.
type SonarrClient struct {
	*arr.Client
}

// opsAdapter satisfies arr.Operations (and arr.DatabaseTyper) against the
// generated sonarr client.
type opsAdapter struct {
	client *Client
}

func (a *opsAdapter) PostCommand(ctx context.Context, name string) (*http.Response, error) {
	return a.client.PostApiV3Command(ctx, CommandResource{Name: &name})
}

func (a *opsAdapter) GetCommand(ctx context.Context, id int32) (*http.Response, error) {
	return a.client.GetApiV3CommandId(ctx, id)
}

func (a *opsAdapter) GetConfigHost(ctx context.Context) (*http.Response, error) {
	return a.client.GetApiV3ConfigHost(ctx)
}

func (a *opsAdapter) GetSystemBackup(ctx context.Context) (*http.Response, error) {
	return a.client.GetApiV3SystemBackup(ctx)
}

func (a *opsAdapter) PostSystemRestart(ctx context.Context) (*http.Response, error) {
	return a.client.PostApiV3SystemRestart(ctx)
}

func (a *opsAdapter) GetSystemStatus(ctx context.Context) (*http.Response, error) {
	return a.client.GetApiV3SystemStatus(ctx)
}

// NewSonarrClient creates a new Sonarr API client with API key authentication
func NewSonarrClient(baseURL, apiKey, username, password string, pgOverride *backup.PostgresConfig) (*SonarrClient, error) {
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
		return nil, fmt.Errorf("failed to create sonarr client: %w", err)
	}

	return &SonarrClient{
		Client: arr.NewClient(arr.Config{
			AppName:    "sonarr",
			APIVersion: "v3",
			BaseURL:    baseURL,
			APIKey:     apiKey,
			Username:   username,
			Password:   password,
			HTTPClient: httpClient,
			PgOverride: pgOverride,
			Ops:        &opsAdapter{client: genClient},
		}),
	}, nil
}
