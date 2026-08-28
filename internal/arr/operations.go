package arr

import (
	"context"
	"net/http"
)

// Operations is the minimal set of generated-client calls the shared Client
// needs. sonarr, radarr and prowlarr each satisfy this with a thin adapter
// over their own generated code, since the generated request/response types
// (and operation names, e.g. GetApiV3SystemBackup vs GetApiV1SystemBackup)
// are distinct per package.
type Operations interface {
	PostCommand(ctx context.Context, name string) (*http.Response, error)
	GetCommand(ctx context.Context, id int32) (*http.Response, error)
	GetConfigHost(ctx context.Context) (*http.Response, error)
	GetSystemBackup(ctx context.Context) (*http.Response, error)
	PostSystemRestart(ctx context.Context) (*http.Response, error)
}

// DatabaseTyper is implemented by Operations that can report a database
// type. Only sonarr and radarr support it; prowlarr's adapter simply
// doesn't implement this interface, so the Postgres branch in Backup is
// skipped for it entirely.
type DatabaseTyper interface {
	GetSystemStatus(ctx context.Context) (*http.Response, error)
}
