# backuparr

A CLI that backs up Sonarr/Radarr-family apps (and anything else fronted by
its sidecar) to one or more storage destinations, prunes old backups on a
retention policy, and restores from any of them.

## What it does

- Triggers an app's native backup mechanism (or, for the sidecar, snapshots
  its SQLite databases), optionally including a `pg_dump` of an external
  PostgreSQL database.
- Uploads the resulting archive to every configured storage destination for
  that app (local filesystem and/or S3-compatible object storage).
- Applies a restic/Borg-style retention policy (`keepLast`, `keepHourly`,
  `keepDaily`, `keepWeekly`, `keepMonthly`, `keepYearly`) per destination
  after each successful upload.
- Restores a specific app from a specific destination, by key or `--latest`.
- Serves a small web UI for browsing, triggering, and monitoring backups.

A failed upload to any configured destination fails that app's backup run
(other destinations and other apps still get a fair shot) — a destination in
config is a promise, not a best effort.

## Supported app types

| `appType`  | Backup method                                                    |
|------------|-------------------------------------------------------------------|
| `sonarr`   | Native backup API, with optional `pg_dump` for external Postgres |
| `radarr`   | Native backup API, with optional `pg_dump` for external Postgres |
| `prowlarr` | Native backup API, with optional `pg_dump` for external Postgres |
| `truenas`  | Config export via the TrueNAS Scale JSON-RPC/WebSocket API        |
| `sidecar`  | A companion container (`Dockerfile.sidecar`) run alongside apps with no backup API of their own (e.g. transmission, overseerr, nzbget); it snapshots SQLite databases and can restart the container after a restore |

Storage backends: `local` (filesystem path) and `s3` (any S3-compatible API —
AWS, MinIO, Backblaze B2, Wasabi, Cloudflare R2 — via a custom endpoint).

## Quick start

```sh
go build -o backuparr ./cmd/backuparr   # or: make build

cp config.yml.example config.yml
$EDITOR config.yml                      # add your app(s) and storage

./backuparr backup                      # back up every configured app
./backuparr list    --app sonarr --backend local
./backuparr restore --app sonarr --backend local --latest
./backuparr web --listen :8080          # browse/trigger backups in a UI
```

By default backuparr reads `/config/config.yml`; override with the
`BACKUPARR_CONFIG` environment variable. Running via Docker:

```sh
docker run -v /path/to/config.yml:/config/config.yml backuparr backup
```

If an app has no `storage` block, it defaults to a single `local` backend at
`./backups`.

## Configuration

See [`config.yml.example`](config.yml.example) for a complete, commented
reference covering every app type, storage backend, retention policy, and
the sidecar setup — it is the source of truth for the config schema and is
kept up to date; this README does not duplicate it.

For the storage/retention design (interfaces, retention bucket semantics,
S3 implementation notes, known limitations), see
[`docs/design-remote-storage.md`](docs/design-remote-storage.md).

## Running the tests

```sh
make test-unit          # unit tests, no external services required
go test ./...            # equivalent, without gotestfmt output

make test-integration    # spins up Docker containers, runs full backup/restore suite
make test-quick          # integration tests, skipping the slower validation runs
make test-s3             # S3 backend tests against a local MinIO container
```

`test-integration`, `test-quick`, and `test-sidecar` require Docker and set
`INTEGRATION_TEST=1`; `test-s3` requires Docker and sets `S3_TEST=1`.
