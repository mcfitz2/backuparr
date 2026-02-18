package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"backuparr/internal/backup"
	"backuparr/internal/config"
	"backuparr/internal/prowlarr"
	"backuparr/internal/radarr"
	"backuparr/internal/sidecar"
	"backuparr/internal/sonarr"
	"backuparr/internal/storage"
	"backuparr/internal/storage/local"
	s3backend "backuparr/internal/storage/s3"
	"backuparr/internal/truenas"
)

// preflightCheck inspects the loaded config and verifies that all required
// external tools are available before any work begins. This avoids partial
// failures mid-backup or mid-restore due to a missing CLI tool.
func preflightCheck(cfg config.BackuparrConfig) error {
	var needPgDump, needPsql bool

	for _, app := range cfg.AppConfigs {
		// If any app has an explicit postgres override, we'll need pg tools
		if app.Postgres != nil {
			needPgDump = true
			needPsql = true
		}
	}

	var missing []string

	if needPgDump {
		if _, err := exec.LookPath("pg_dump"); err != nil {
			missing = append(missing, "pg_dump (required for PostgreSQL backup)")
		}
	}
	if needPsql {
		if _, err := exec.LookPath("psql"); err != nil {
			missing = append(missing, "psql (required for PostgreSQL restore)")
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required tools:\n  - %s", strings.Join(missing, "\n  - "))
	}

	return nil
}

func createClient(cfg config.AppConfig) (backup.Client, error) {
	var pgOverride *backup.PostgresConfig
	if cfg.Postgres != nil {
		pgOverride = &backup.PostgresConfig{
			Host:     cfg.Postgres.Host,
			Port:     cfg.Postgres.Port,
			User:     cfg.Postgres.User,
			Password: cfg.Postgres.Password,
			MainDB:   cfg.Postgres.MainDB,
			LogDB:    cfg.Postgres.LogDB,
		}
	}

	switch cfg.AppType {
	case "sonarr":
		return sonarr.NewSonarrClient(cfg.Connection.URL, cfg.Connection.APIKey, cfg.Connection.Username, cfg.Connection.Password, pgOverride)
	case "radarr":
		return radarr.NewRadarrClient(cfg.Connection.URL, cfg.Connection.APIKey, cfg.Connection.Username, cfg.Connection.Password, pgOverride)
	case "prowlarr":
		return prowlarr.NewProwlarrClient(cfg.Connection.URL, cfg.Connection.APIKey, cfg.Connection.Username, cfg.Connection.Password)
	case "truenas":
		return truenas.NewClient(cfg.Connection.URL, cfg.Connection.APIKey), nil
	case "sidecar":
		name := cfg.Name
		if name == "" {
			name = "sidecar"
		}
		return sidecar.NewClient(cfg.Connection.URL, cfg.Connection.APIKey, name)
	default:
		return nil, fmt.Errorf("unsupported app type: %s", cfg.AppType)
	}
}

func createBackends(configs []config.StorageConfig) ([]storage.Backend, error) {
	if len(configs) == 0 {
		// Default to local storage in ./backups for backward compatibility
		return []storage.Backend{local.New("./backups")}, nil
	}

	var backends []storage.Backend
	for _, cfg := range configs {
		var b storage.Backend
		switch cfg.Type {
		case "local":
			path := cfg.Path
			if path == "" {
				path = "./backups"
			}
			b = local.New(path)
		case "s3":
			prefix := cfg.Prefix
			if prefix == "" {
				prefix = "backuparr"
			}
			s3cfg := s3backend.Config{
				Bucket:          cfg.Bucket,
				Prefix:          prefix,
				Region:          cfg.Region,
				Endpoint:        cfg.Endpoint,
				AccessKeyID:     cfg.AccessKeyID,
				SecretAccessKey: cfg.SecretAccessKey,
				StorageClass:    cfg.StorageClass,
			}
			var err error
			b, err = s3backend.New(context.Background(), s3cfg)
			if err != nil {
				return nil, fmt.Errorf("failed to create S3 backend: %w", err)
			}
		default:
			return nil, fmt.Errorf("unsupported storage type: %s", cfg.Type)
		}
		b.SetName(config.StorageConfigName(cfg))
		backends = append(backends, b)
	}
	return backends, nil
}

func toStorageRetention(r config.RetentionPolicy) storage.RetentionPolicy {
	return storage.RetentionPolicy{
		KeepLast:    r.KeepLast,
		KeepHourly:  r.KeepHourly,
		KeepDaily:   r.KeepDaily,
		KeepWeekly:  r.KeepWeekly,
		KeepMonthly: r.KeepMonthly,
		KeepYearly:  r.KeepYearly,
	}
}

func runBackup(ctx context.Context, app backup.Client, backends []storage.Backend, retention config.RetentionPolicy) error {
	log.Printf("[%s] Starting backup...", app.Name())

	result, reader, err := app.Backup(ctx)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	defer reader.Close()

	// Read backup into memory (needed for uploading to multiple backends)
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read backup data: %w", err)
	}

	log.Printf("[%s] Backup created: %s (%d bytes)", app.Name(), result.Name, len(data))

	// Generate consistent filename
	fileName := storage.FormatBackupName(app.Name(), time.Now())

	// Upload to each backend sequentially
	for _, backend := range backends {
		meta, err := backend.Upload(ctx, app.Name(), fileName, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			log.Printf("[%s] Failed to upload to %s: %v", app.Name(), backend.Name(), err)
			continue
		}
		log.Printf("[%s] Uploaded to %s: %s (%d bytes)", app.Name(), backend.Name(), meta.FileName, meta.Size)

		// Apply retention policy
		storageRetention := toStorageRetention(retention)
		deleted, err := storage.ApplyRetention(ctx, backend, app.Name(), storageRetention)
		if err != nil {
			log.Printf("[%s] Retention cleanup failed on %s: %v", app.Name(), backend.Name(), err)
		} else if deleted > 0 {
			log.Printf("[%s] Cleaned up %d old backup(s) from %s", app.Name(), deleted, backend.Name())
		}
	}

	return nil
}

func main() {
	if len(os.Args) < 2 {
		// Default to backup when no subcommand
		printUsage()
		return
	}

	switch os.Args[1] {
	case "backup":
		runBackupAll()
	case "restore":
		runRestoreCLI()
	case "list":
		runListCLI()
	case "web", "serve":
		runWebUI()
	case "help", "--help", "-h":
		printUsage()
	default:
		printUsage()
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: backuparr [command]

Commands:
  backup                  Run backups for all configured apps (default)
  restore                 Restore an app from a storage backend
  list                    List available backups from a storage backend
	web                     Start web UI for listing/deleting backups
  help                    Show this help message

Restore flags:
  --app <name>            App to restore (e.g. sonarr, radarr, prowlarr, truenas) [required]
  --backend <name>        Storage backend name (defaults to type, e.g. local, s3) [required]
  --backup <key>          Specific backup key to restore
  --latest                Restore the most recent backup

List flags:
  --app <name>            App to list backups for (e.g. sonarr, radarr, prowlarr, truenas) [required]
  --backend <name>        Storage backend name (defaults to type, e.g. local, s3) [required]

Environment:
  BACKUPARR_CONFIG        Path to config file (default: /config/config.yml)

Examples:
  backuparr                                           # Run backups
  backuparr backup                                    # Run backups (explicit)
  backuparr list --app sonarr --backend local         # List sonarr backups
  backuparr restore --app sonarr --backend s3 --latest
  backuparr restore --app radarr --backend nas --latest  # Named backend
  backuparr restore --app sonarr --backend local --backup "sonarr/sonarr_2026-02-06T120000Z.zip"
	backuparr web --listen :8080                      # Start web UI

Docker:
  docker run -v /path/to/config.yml:/config/config.yml backuparr backup
`)
}

func runBackupAll() {
	ctx := context.Background()

	cfg, err := config.Parse(config.Path())
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if err := preflightCheck(cfg); err != nil {
		log.Fatalf("Preflight check failed: %v", err)
	}

	for _, appCfg := range cfg.AppConfigs {
		client, err := createClient(appCfg)
		if err != nil {
			log.Printf("Failed to create client for %s: %v", appCfg.AppType, err)
			continue
		}

		backends, err := createBackends(appCfg.Storage)
		if err != nil {
			log.Printf("[%s] Failed to create storage backends: %v", appCfg.AppType, err)
			continue
		}

		if err := runBackup(ctx, client, backends, appCfg.Retention); err != nil {
			log.Printf("[%s] Backup failed: %v", appCfg.AppType, err)
		}
	}
}

func runRestoreCLI() {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	appName := fs.String("app", "", "App to restore (e.g. sonarr, radarr, prowlarr)")
	backendName := fs.String("backend", "", "Storage backend name (e.g. local, s3, nas)")
	backupKey := fs.String("backup", "", "Specific backup key to restore")
	latest := fs.Bool("latest", false, "Restore the most recent backup")
	fs.Parse(os.Args[2:])

	if *appName == "" || *backendName == "" {
		fmt.Fprintln(os.Stderr, "Error: --app and --backend are required")
		fs.Usage()
		os.Exit(1)
	}
	if *backupKey == "" && !*latest {
		fmt.Fprintln(os.Stderr, "Error: either --backup <key> or --latest is required")
		fs.Usage()
		os.Exit(1)
	}

	ctx := context.Background()

	cfg, err := config.Parse(config.Path())
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if err := preflightCheck(cfg); err != nil {
		log.Fatalf("Preflight check failed: %v", err)
	}

	// Find the app config
	appCfg, err := findAppConfig(cfg, *appName)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Create the app client (needed for Restore)
	client, err := createClient(appCfg)
	if err != nil {
		log.Fatalf("Failed to create client for %s: %v", *appName, err)
	}

	// Find and create the specific backend
	backend, err := findBackend(appCfg, *backendName)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Resolve which backup to restore
	key := *backupKey
	if *latest {
		backups, err := backend.List(ctx, *appName)
		if err != nil {
			log.Fatalf("Failed to list backups: %v", err)
		}
		if len(backups) == 0 {
			log.Fatalf("No backups found for %s on %s", *appName, *backendName)
		}
		key = backups[0].Key
		log.Printf("Selected latest backup: %s (created %s)", key, backups[0].CreatedAt.Format(time.RFC3339))
	}

	// Download the backup
	log.Printf("Downloading backup %s from %s...", key, backend.Name())
	reader, meta, err := backend.Download(ctx, key)
	if err != nil {
		log.Fatalf("Failed to download backup: %v", err)
	}
	defer reader.Close()

	log.Printf("Downloaded: %s (%d bytes, created %s)", meta.FileName, meta.Size, meta.CreatedAt.Format(time.RFC3339))

	// Restore
	log.Printf("Restoring %s...", *appName)
	if err := client.Restore(ctx, reader); err != nil {
		log.Fatalf("Restore failed: %v", err)
	}

	log.Printf("Restore complete for %s", *appName)
}

func runListCLI() {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	appName := fs.String("app", "", "App to list backups for (e.g. sonarr, radarr)")
	backendName := fs.String("backend", "", "Storage backend name (e.g. local, s3, nas)")
	fs.Parse(os.Args[2:])

	if *appName == "" || *backendName == "" {
		fmt.Fprintln(os.Stderr, "Error: --app and --backend are required")
		fs.Usage()
		os.Exit(1)
	}

	ctx := context.Background()

	cfg, err := config.Parse(config.Path())
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Find the app config
	appCfg, err := findAppConfig(cfg, *appName)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Find and create the specific backend
	backend, err := findBackend(appCfg, *backendName)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// List backups
	backups, err := backend.List(ctx, *appName)
	if err != nil {
		log.Fatalf("Failed to list backups: %v", err)
	}

	if len(backups) == 0 {
		fmt.Printf("No backups found for %s on %s\n", *appName, *backendName)
		return
	}

	// Print as a formatted table
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "KEY\tFILENAME\tSIZE\tCREATED\n")
	for _, b := range backups {
		sizeStr := formatSize(b.Size)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.Key, b.FileName, sizeStr, b.CreatedAt.Format(time.RFC3339))
	}
	w.Flush()
}

// findAppConfig looks up the AppConfig for the given app name.
// It matches against the Name field first, then falls back to AppType.
func findAppConfig(cfg config.BackuparrConfig, appName string) (config.AppConfig, error) {
	for _, ac := range cfg.AppConfigs {
		effectiveName := ac.Name
		if effectiveName == "" {
			effectiveName = ac.AppType
		}
		if effectiveName == appName {
			return ac, nil
		}
	}
	var names []string
	for _, ac := range cfg.AppConfigs {
		if ac.Name != "" {
			names = append(names, ac.Name)
		} else {
			names = append(names, ac.AppType)
		}
	}
	return config.AppConfig{}, fmt.Errorf("app %q not found in config (available: %v)", appName, names)
}

// findBackend creates a single storage backend matching the given name from
// the app's config. Returns an error if no match is found or if multiple
// backends share the same effective name (ambiguous).
func findBackend(appCfg config.AppConfig, backendName string) (storage.Backend, error) {
	var matches []config.StorageConfig
	for _, sc := range appCfg.Storage {
		if config.StorageConfigName(sc) == backendName {
			matches = append(matches, sc)
		}
	}

	switch len(matches) {
	case 0:
		var names []string
		for _, sc := range appCfg.Storage {
			names = append(names, config.StorageConfigName(sc))
		}
		return nil, fmt.Errorf("backend %q not configured for %s (available: %v)", backendName, appCfg.AppType, names)
	case 1:
		backends, err := createBackends(matches)
		if err != nil {
			return nil, err
		}
		return backends[0], nil
	default:
		return nil, fmt.Errorf("multiple backends match %q for %s; assign unique names in config", backendName, appCfg.AppType)
	}
}

// formatSize returns a human-readable size string.
func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
