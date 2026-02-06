package pbs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"backuparr/storage"
)

// Ensure PBSBackend implements storage.Backend at compile time.
var _ storage.Backend = (*PBSBackend)(nil)

// Config holds the configuration for a Proxmox Backup Server backend.
type Config struct {
	Server      string // PBS server hostname or IP
	Port        int    // default 8007
	Datastore   string // PBS datastore name
	Namespace   string // optional PBS namespace
	Username    string // e.g. "backup@pbs" or "user@pbs!token"
	Password    string // password or API token secret
	Fingerprint string // PBS server TLS fingerprint (AA:BB:CC:...)
}

// PBSBackend stores backups on a Proxmox Backup Server using the
// proxmox-backup-client CLI tool. This is the same pragmatic approach
// used for pg_dump/psql -- the CLI handles the complex chunked upload
// protocol, deduplication, and encryption.
type PBSBackend struct {
	repository  string
	namespace   string
	password    string
	fingerprint string
}

// New creates a new PBS storage backend from the given config.
// Returns an error if proxmox-backup-client is not found in PATH.
func New(cfg Config) (*PBSBackend, error) {
	if cfg.Server == "" {
		return nil, fmt.Errorf("pbs: server is required")
	}
	if cfg.Datastore == "" {
		return nil, fmt.Errorf("pbs: datastore is required")
	}

	// Verify proxmox-backup-client is available
	if _, err := exec.LookPath("proxmox-backup-client"); err != nil {
		return nil, fmt.Errorf("pbs: proxmox-backup-client not found in PATH: %w", err)
	}

	username := cfg.Username
	if username == "" {
		username = "root@pam"
	}

	port := cfg.Port
	if port == 0 {
		port = 8007
	}

	// Build repository string: username@server:port:datastore
	repository := buildRepository(username, cfg.Server, port, cfg.Datastore)

	return &PBSBackend{
		repository:  repository,
		namespace:   cfg.Namespace,
		password:    cfg.Password,
		fingerprint: cfg.Fingerprint,
	}, nil
}

func (b *PBSBackend) Name() string {
	return "pbs"
}

// cmdEnv returns the environment variables for proxmox-backup-client commands.
// PBS_PASSWORD and PBS_FINGERPRINT are set to avoid interactive prompts.
func (b *PBSBackend) cmdEnv() []string {
	env := os.Environ()
	if b.password != "" {
		env = append(env, "PBS_PASSWORD="+b.password)
	}
	if b.fingerprint != "" {
		env = append(env, "PBS_FINGERPRINT="+b.fingerprint)
	}
	return env
}

// baseArgs returns the common CLI arguments shared by all commands.
func (b *PBSBackend) baseArgs() []string {
	args := []string{"--repository", b.repository}
	if b.namespace != "" {
		args = append(args, "--ns", b.namespace)
	}
	return args
}

// Upload stores backup data as a PBS snapshot containing a single blob archive.
//
// The zip data is written to a temp file, then uploaded via:
//
//	proxmox-backup-client backup backup.img:<tempfile> --backup-id <appName> --repository <repo>
//
// This creates a snapshot at host/<appName>/<timestamp> on the PBS server.
func (b *PBSBackend) Upload(ctx context.Context, appName string, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	// Write data to a temp file -- proxmox-backup-client needs a file path
	tmpFile, err := os.CreateTemp("", "backuparr-pbs-*.zip")
	if err != nil {
		return nil, fmt.Errorf("pbs: failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	written, err := io.Copy(tmpFile, data)
	if err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("pbs: failed to write temp file: %w", err)
	}
	tmpFile.Close()

	// proxmox-backup-client backup backup.img:<path> --backup-id <appName>
	args := []string{"backup", fmt.Sprintf("backup.img:%s", tmpPath), "--backup-id", appName}
	args = append(args, b.baseArgs()...)

	cmd := exec.CommandContext(ctx, "proxmox-backup-client", args...)
	cmd.Env = b.cmdEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pbs: backup failed: %w\nstderr: %s", err, stderr.String())
	}

	// Parse the snapshot ID from the output.
	// Output typically contains: "Starting backup: host/<appName>/<timestamp>"
	combinedOutput := stdout.String() + "\n" + stderr.String()
	snapshotID := parseSnapshotFromOutput(combinedOutput, appName)
	if snapshotID == "" {
		// Fallback: construct from current time
		snapshotID = fmt.Sprintf("host/%s/%s", appName, time.Now().UTC().Format(time.RFC3339))
	}

	log.Printf("[pbs] Backup uploaded: %s (%d bytes)", snapshotID, written)

	return &storage.BackupMetadata{
		Key:       snapshotID,
		AppName:   appName,
		FileName:  fileName,
		Size:      written,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Download retrieves a backup blob from a PBS snapshot.
// The caller must close the returned reader.
//
// The snapshot is restored to a temp file via:
//
//	proxmox-backup-client restore <snapshot> backup.img <tempfile> --repository <repo>
func (b *PBSBackend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	tmpFile, err := os.CreateTemp("", "backuparr-pbs-restore-*.zip")
	if err != nil {
		return nil, nil, fmt.Errorf("pbs: failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	// proxmox-backup-client restore <snapshot> backup.img <target>
	args := []string{"restore", key, "backup.img", tmpPath}
	args = append(args, b.baseArgs()...)

	cmd := exec.CommandContext(ctx, "proxmox-backup-client", args...)
	cmd.Env = b.cmdEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		os.Remove(tmpPath)
		return nil, nil, fmt.Errorf("pbs: restore failed: %w\nstderr: %s", err, stderr.String())
	}

	file, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, nil, fmt.Errorf("pbs: failed to open restored file: %w", err)
	}

	info, _ := file.Stat()
	var fileSize int64
	if info != nil {
		fileSize = info.Size()
	}

	appName, backupTime := parseSnapshotKey(key)

	meta := &storage.BackupMetadata{
		Key:       key,
		AppName:   appName,
		FileName:  storage.FormatBackupName(appName, backupTime),
		Size:      fileSize,
		CreatedAt: backupTime,
	}

	// Wrap in a reader that cleans up the temp file on Close
	return &tempFileReader{file: file, path: tmpPath}, meta, nil
}

// List returns all snapshots for the given app, sorted newest-first.
//
// Uses: proxmox-backup-client snapshot list --output-format json --repository <repo>
// Then filters by backup-type=host and backup-id=appName.
func (b *PBSBackend) List(ctx context.Context, appName string) ([]storage.BackupMetadata, error) {
	args := []string{"snapshot", "list", "--output-format", "json"}
	args = append(args, b.baseArgs()...)

	cmd := exec.CommandContext(ctx, "proxmox-backup-client", args...)
	cmd.Env = b.cmdEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pbs: failed to list snapshots: %w\nstderr: %s", err, stderr.String())
	}

	var snapshots []snapshotInfo
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("pbs: failed to parse snapshot list: %w\noutput: %s", err, string(output))
	}

	var backups []storage.BackupMetadata
	for _, snap := range snapshots {
		if snap.BackupType != "host" || snap.BackupID != appName {
			continue
		}

		ts := time.Unix(snap.BackupTime, 0).UTC()
		snapshotPath := fmt.Sprintf("host/%s/%s", snap.BackupID, ts.Format(time.RFC3339))

		meta := storage.BackupMetadata{
			Key:       snapshotPath,
			AppName:   snap.BackupID,
			FileName:  storage.FormatBackupName(snap.BackupID, ts),
			CreatedAt: ts,
		}
		if snap.Size != nil {
			meta.Size = *snap.Size
		}
		backups = append(backups, meta)
	}

	// Sort newest-first
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// Delete removes a PBS snapshot.
//
// Uses: proxmox-backup-client snapshot forget <snapshot> --repository <repo>
func (b *PBSBackend) Delete(ctx context.Context, key string) error {
	args := []string{"snapshot", "forget", key}
	args = append(args, b.baseArgs()...)

	cmd := exec.CommandContext(ctx, "proxmox-backup-client", args...)
	cmd.Env = b.cmdEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pbs: failed to forget snapshot %s: %w\nstderr: %s", key, err, stderr.String())
	}

	return nil
}

// snapshotInfo represents a PBS snapshot in the JSON output of "snapshot list".
type snapshotInfo struct {
	BackupType string `json:"backup-type"`
	BackupID   string `json:"backup-id"`
	BackupTime int64  `json:"backup-time"`
	Size       *int64 `json:"size"`
}

// tempFileReader wraps an os.File and removes the temp file on Close.
type tempFileReader struct {
	file *os.File
	path string
}

func (r *tempFileReader) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *tempFileReader) Close() error {
	err := r.file.Close()
	os.Remove(r.path)
	return err
}

// parseSnapshotFromOutput extracts the snapshot ID from proxmox-backup-client output.
// Looks for "Starting backup: host/<appName>/<timestamp>" pattern.
func parseSnapshotFromOutput(output, appName string) string {
	prefix := "Starting backup: "
	target := "host/" + appName + "/"
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, prefix); idx >= 0 {
			snapshot := strings.TrimSpace(line[idx+len(prefix):])
			if strings.HasPrefix(snapshot, target) {
				return snapshot
			}
		}
	}
	return ""
}

// parseSnapshotKey extracts appName and timestamp from a PBS snapshot key.
// Expected format: host/<appName>/<RFC3339 timestamp>
func parseSnapshotKey(key string) (appName string, backupTime time.Time) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 {
		return "", time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, parts[2])
	return parts[1], t
}

// buildRepository constructs the PBS repository string from config fields.
func buildRepository(username, server string, port int, datastore string) string {
	if username == "" {
		username = "root@pam"
	}
	if port == 0 {
		port = 8007
	}
	return fmt.Sprintf("%s@%s:%d:%s", username, server, port, datastore)
}
