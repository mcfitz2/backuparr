# Remote Storage Design Document

## Overview

Backuparr currently supports backing up Sonarr/Radarr instances to the local filesystem. This document proposes adding remote storage backends so backups can be uploaded directly to S3-compatible object stores and Proxmox Backup Server (PBS). Network filesystems (CIFS/NFS) are out of scope — users mount them and use the existing local path.

## Goals

1. **Pluggable storage backends** — a single interface that S3, PBS, and future backends implement.
2. **Retention management** — each backend enforces the retention policy defined in config.
3. **Restore from remote** — pull a specific backup from remote storage and restore it.
4. **Multiple destinations** — a single app can back up to several destinations simultaneously.
5. **Streaming where possible** — avoid buffering entire backups in memory when the backend supports it.

## Non-Goals

- NFS/CIFS mount management (use OS-level mounts + local path).
- Scheduling (use cron, systemd timers, or a container scheduler).
- Encryption at the application layer (rely on transport encryption + server-side encryption).

---

## Architecture

### Storage Interface

A new `storage` package defines the contract that every backend must satisfy:

```
storage/
  storage.go        # Interface + types
  s3/
    s3.go           # S3 backend
  pbs/
    pbs.go          # Proxmox Backup Server backend
  local/
    local.go        # Local filesystem backend (refactored from main.go)
```

```go
package storage

import (
    "context"
    "io"
    "time"
)

// BackupMetadata describes a single backup file stored in a backend.
type BackupMetadata struct {
    Key       string    // Unique identifier within the backend (path, object key, snapshot ID)
    AppName   string    // "sonarr", "radarr", etc.
    FileName  string    // Original backup filename
    Size      int64
    CreatedAt time.Time
}

// Backend is the interface every remote storage provider implements.
type Backend interface {
    // Name returns a human-readable backend identifier (e.g. "s3", "pbs", "local").
    Name() string

    // Upload stores backup data and returns metadata for the stored object.
    Upload(ctx context.Context, appName string, fileName string, data io.Reader, size int64) (*BackupMetadata, error)

    // Download retrieves a backup by key. Caller must close the reader.
    Download(ctx context.Context, key string) (io.ReadCloser, *BackupMetadata, error)

    // List returns all backups for a given app, ordered newest-first.
    List(ctx context.Context, appName string) ([]BackupMetadata, error)

    // Delete removes a backup by key.
    Delete(ctx context.Context, key string) error
}
```

Retention is intentionally **not** part of the interface. A shared `storage.ApplyRetention()` helper calls `List` + `Delete` using the `RetentionPolicy` from config. This avoids duplicating retention logic in every backend.

```go
// storage/retention.go

// ApplyRetention lists existing backups and deletes those that exceed the policy.
func ApplyRetention(ctx context.Context, backend Backend, appName string, policy RetentionPolicy) (deleted int, err error)
```

### How Backup and Storage Interact

The flow stays the same — the `backup.Client` (Sonarr/Radarr) produces a backup zip (with embedded pg_dump if PostgreSQL). The **orchestrator** in `main.go` decides where to send it:

```
┌──────────────┐     BackupToLocal()     ┌──────────────┐
│ backup.Client│ ───────────────────────► │  []byte zip  │
│ (sonarr/     │                          │  in memory   │
│  radarr)     │                          └──────┬───────┘
└──────────────┘                                 │
                                                 │  for each destination:
                                    ┌────────────┼────────────┐
                                    ▼            ▼            ▼
                              ┌──────────┐ ┌──────────┐ ┌──────────┐
                              │  local/  │ │   s3/    │ │   pbs/   │
                              │ Backend  │ │ Backend  │ │ Backend  │
                              └──────────┘ └──────────┘ └──────────┘
                                    │            │            │
                              ApplyRetention  ApplyRetention  ApplyRetention
```

The `backup.Client` interface does **not** change. `BackupToRemote` / `RestoreFromRemote` are removed — the orchestrator handles routing. The simplified interface becomes:

```go
type Client interface {
    Name() string
    Backup(ctx context.Context) (*BackupResult, io.ReadCloser, error)
    Restore(ctx context.Context, backup io.Reader) error
}
```

---

## Backend: S3

### Library

Use the AWS SDK v2 (`github.com/aws/aws-sdk-go-v2`). It supports any S3-compatible API (AWS, MinIO, Backblaze B2, Wasabi, Cloudflare R2) via a custom endpoint.

### Object Layout

```
s3://<bucket>/<prefix>/<appName>/<filename>
```

Example:
```
s3://my-backups/backuparr/sonarr/sonarr_backup_v4.0.16.2944_2026.02.05_19.12.59.zip
s3://my-backups/backuparr/radarr/radarr_backup_v6.0.4.10291_2026.02.05_19.13.04.zip
```

### Implementation Notes

- **Upload**: `s3.PutObject` with `io.Reader` — streams directly, no temp file.
- **Download**: `s3.GetObject` returns a `ReadCloser`.
- **List**: `s3.ListObjectsV2` with prefix `<prefix>/<appName>/`, parse `LastModified` for retention.
- **Delete**: `s3.DeleteObject`.
- **Multipart**: The SDK handles multipart uploads automatically for large objects. No special code needed.
- **Authentication**: Standard AWS credential chain (env vars, `~/.aws/credentials`, IAM role, IRSA). No credentials in backuparr config unless the user wants explicit keys.

### Configuration

```yaml
storage:
  - type: s3
    bucket: "my-backups"
    prefix: "backuparr"          # optional, defaults to "backuparr"
    region: "us-east-1"
    endpoint: ""                 # optional, for MinIO/R2/etc.
    accessKeyId: ""              # optional, falls back to AWS credential chain
    secretAccessKey: ""          # optional
    storageClass: "STANDARD"     # optional, e.g. DEEP_ARCHIVE for cold storage
```

---

## Backend: Proxmox Backup Server

### Background

PBS stores backups as deduplicated, encrypted chunks with built-in GC and verification. It exposes an HTTP API and also has a CLI tool `proxmox-backup-client`. PBS is commonly used for VM/CT backups but also supports generic file-level archives via its "host" backup type.

### Approach

Use the **PBS REST API** directly over HTTPS:

1. **Authentication** — `POST /api2/json/access/ticket` with username/password (or API token). Returns a `PBSAuthCookie` + `CSRFPreventionToken`.
2. **Upload** — PBS uses a chunked upload protocol:
   - `POST /api2/json/admin/datastore/{store}/upload-backup-log` for metadata
   - The actual upload uses the `pxar` archive format or raw fixed-index/dynamic-index
   - For simplicity, use **`proxmox-backup-client backup`** CLI as the transport (similar to how we use `pg_dump`)
3. **List** — `GET /api2/json/admin/datastore/{store}/snapshots?backup-type=host&backup-id={appName}`
3. **Download** — `proxmox-backup-client restore` or the equivalent API calls
4. **Delete** — `DELETE /api2/json/admin/datastore/{store}/snapshots` with the snapshot path

### Why Use the CLI

PBS's upload protocol is complex — it chunks data, computes checksums, deduplicates against the datastore's chunk index, and writes catalog metadata. Reimplementing this in Go would be a large effort with no benefit. The `proxmox-backup-client` binary handles all of this and is available on any Proxmox host or can be installed standalone from the PBS repository.

This is the same pragmatic approach we use for `pg_dump`/`psql`.

### Implementation Notes

```go
// Backup: pipe the zip to proxmox-backup-client via stdin
// proxmox-backup-client backup - --backup-type host --backup-id sonarr \
//   --repository <user>@<server>:<datastore>

// Restore: get the archive back via stdout
// proxmox-backup-client restore <snapshot> - --repository <user>@<server>:<datastore>
```

- PBS deduplication means daily backups of similar data are extremely space-efficient.
- PBS handles retention natively via prune jobs, but we can also call the prune API to keep behavior consistent with other backends.
- PBS verification (`verify` endpoint) can be called after upload for integrity checking.

### Configuration

```yaml
storage:
  - type: pbs
    server: "pbs.example.com"
    port: 8007                   # default PBS port
    datastore: "backups"
    namespace: ""                # optional PBS namespace
    username: "backup@pbs"
    password: "secret"           # or tokenId + tokenSecret
    fingerprint: "AA:BB:CC:..."  # PBS server TLS fingerprint
    verify: true                 # optional, verify backup after upload
```

---

## Backend: Local Filesystem

Refactor the existing `backupToLocal()` in `main.go` into a proper `storage.Backend` implementation so all backends are treated uniformly.

### Configuration

```yaml
storage:
  - type: local
    path: "/mnt/backups"
```

The local backend stores files as:
```
/mnt/backups/<appName>/<filename>
```

---

## Updated Config Schema

```yaml
appConfigs:
  - appType: sonarr
    connection:
      apiKey: "..."
      url: "https://sonarr.example.com"
      username: "admin"
      password: "secret"
    retention:
      keepLast: 5
      keepDaily: 7
      keepWeekly: 4
      keepMonthly: 6
    postgres:
      host: "sonarr-db.example.com"
    storage:                       # NEW: list of destinations
      - type: local
        path: "/mnt/backups"
      - type: s3
        bucket: "my-backups"
        region: "us-east-1"
      - type: pbs
        server: "pbs.example.com"
        datastore: "app-backups"
        username: "backup@pbs"
        password: "secret"
        fingerprint: "AA:BB:CC:..."

  - appType: radarr
    connection:
      apiKey: "..."
      url: "https://radarr.example.com"
    retention:
      keepLast: 5
    storage:
      - type: s3
        bucket: "my-backups"
        region: "us-east-1"
```

**Key decisions:**
- Storage is **per-app**, not global. Different apps may need different buckets or datastores.
- Retention is still **per-app** and applied uniformly across all backends for that app.
- If `storage` is omitted, default to local storage in `./backups` for backward compatibility.

---

## Retention Strategy

The `RetentionPolicy` fields map to time-based buckets:

| Field         | Meaning                                          |
|---------------|--------------------------------------------------|
| `keepLast`    | Always keep the N most recent backups            |
| `keepHourly`  | Keep one backup per hour for the last N hours    |
| `keepDaily`   | Keep one backup per day for the last N days      |
| `keepWeekly`  | Keep one backup per week for the last N weeks    |
| `keepMonthly` | Keep one backup per month for the last N months  |
| `keepYearly`  | Keep one backup per year for the last N years    |

The algorithm (modeled after restic/PBS/Borg):
1. List all backups for the app, sorted by `CreatedAt` descending.
2. Mark backups to keep based on each bucket (a backup can satisfy multiple buckets).
3. Delete unmarked backups.

For PBS, we can optionally delegate to PBS's built-in prune API since it implements the same algorithm natively.

---

## Orchestrator Changes (main.go)

```go
func runBackup(ctx context.Context, app backup.Client, backends []storage.Backend, retention RetentionPolicy) error {
    // 1. Create backup
    result, reader, err := app.Backup(ctx)
    if err != nil {
        return fmt.Errorf("backup failed: %w", err)
    }
    defer reader.Close()

    // 2. Buffer the backup data (needed for multiple backends)
    data, err := io.ReadAll(reader)
    if err != nil {
        return fmt.Errorf("failed to read backup: %w", err)
    }

    // 3. Upload to each backend
    for _, backend := range backends {
        _, err := backend.Upload(ctx, app.Name(), result.Name, bytes.NewReader(data), int64(len(data)))
        if err != nil {
            log.Printf("[%s] Failed to upload to %s: %v", app.Name(), backend.Name(), err)
            continue
        }
        log.Printf("[%s] Uploaded to %s", app.Name(), backend.Name())

        // 4. Apply retention
        deleted, err := storage.ApplyRetention(ctx, backend, app.Name(), retention)
        if err != nil {
            log.Printf("[%s] Retention cleanup failed on %s: %v", app.Name(), backend.Name(), err)
        } else if deleted > 0 {
            log.Printf("[%s] Cleaned up %d old backups from %s", app.Name(), deleted, backend.Name())
        }
    }

    return nil
}
```

---

## Restore Flow

Restore pulls from a **single** backend (user specifies which):

```
backuparr restore --app sonarr --backend s3 --backup <key>
backuparr restore --app sonarr --backend s3 --latest
```

Implementation:
1. `backend.Download(ctx, key)` → `io.ReadCloser`
2. `app.Restore(ctx, reader)` → existing restore logic

For interactive use, `backuparr restore --app sonarr --backend s3` with no `--backup` flag could list available backups and prompt.

---

## Implementation Plan

### Phase 1: Storage Interface + Local Backend
- Create `storage/` package with interface
- Refactor existing local backup into `storage/local`
- Implement `ApplyRetention()`
- Update `main.go` orchestrator to use backends
- Update config schema to support `storage:` block
- Tests for retention logic

### Phase 2: S3 Backend
- Implement `storage/s3` using AWS SDK v2
- Add `go.mod` dependency
- Integration test with MinIO container
- Support custom endpoints for S3-compatible stores

### Phase 3: PBS Backend
- Implement `storage/pbs` using `proxmox-backup-client` CLI
- Detect CLI availability at startup
- Integration test with PBS container (if feasible) or mock
- Support API token auth as alternative to password

### Phase 4: CLI for Restore
- Add `restore` subcommand
- List backups from a backend
- Select and restore

---

## Decisions

1. **Global vs per-app storage** — Per-app configs only. Each app defines its own storage destinations inline. Users who want the same destination for multiple apps duplicate the config block.

2. **Backup naming** — Backuparr uses a consistent naming scheme, ignoring the original filenames from Sonarr/Radarr:
   ```
   <appName>_<timestamp>.zip
   ```
   Example: `sonarr_2026-02-06T120000Z.zip`, `radarr_2026-02-06T120500Z.zip`. Timestamps use UTC in a filesystem-safe format (`YYYY-MM-DDTHHmmSSZ`). This ensures predictable sorting, deduplication-friendly names, and no reliance on upstream naming conventions.

3. **Concurrency** — Uploads are sequential for now (one backend at a time, one app at a time). The orchestrator loop is structured so that swapping in `errgroup` or goroutine fan-out later is a minimal change — each `backend.Upload()` call is independent with no shared state.

4. **Notifications** — Out of scope. May be revisited as a separate feature.

5. **PBS namespace support** — Supported from the start. The `namespace` field in PBS config maps to the `--ns` flag on `proxmox-backup-client`. Defaults to the root namespace when omitted.
