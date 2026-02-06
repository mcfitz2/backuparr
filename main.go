package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"backuparr/backup"
	"backuparr/radarr"
	"backuparr/sonarr"
	"backuparr/storage"
	"backuparr/storage/local"
	pbsbackend "backuparr/storage/pbs"
	s3backend "backuparr/storage/s3"
)

// BackuparrConfig is the top-level configuration.
type BackuparrConfig struct {
	AppConfigs []AppConfig `yaml:"appConfigs"`
}

// AppConfig configures a single application to back up.
type AppConfig struct {
	AppType    string            `yaml:"appType"`
	Connection Connection        `yaml:"connection"`
	Retention  RetentionPolicy   `yaml:"retention"`
	Postgres   *PostgresOverride `yaml:"postgres,omitempty"`
	Storage    []StorageConfig   `yaml:"storage,omitempty"`
}

type RetentionPolicy struct {
	KeepLast    int `yaml:"keepLast"`
	KeepHourly  int `yaml:"keepHourly"`
	KeepDaily   int `yaml:"keepDaily"`
	KeepWeekly  int `yaml:"keepWeekly"`
	KeepMonthly int `yaml:"keepMonthly"`
	KeepYearly  int `yaml:"keepYearly"`
}

type Connection struct {
	APIKey   string `yaml:"apiKey"`
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// PostgresOverride allows manually specifying Postgres connection details.
// When specified, these override the auto-detected values from config.xml.
type PostgresOverride struct {
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	MainDB   string `yaml:"mainDb"`
	LogDB    string `yaml:"logDb"`
}

// StorageConfig defines a storage backend destination.
type StorageConfig struct {
	Type string `yaml:"type"` // "local", "s3", "pbs"

	// Local backend
	Path string `yaml:"path,omitempty"`

	// S3 backend (future)
	Bucket         string `yaml:"bucket,omitempty"`
	Prefix         string `yaml:"prefix,omitempty"`
	Region         string `yaml:"region,omitempty"`
	Endpoint       string `yaml:"endpoint,omitempty"`
	AccessKeyID    string `yaml:"accessKeyId,omitempty"`
	SecretAccessKey string `yaml:"secretAccessKey,omitempty"`
	StorageClass   string `yaml:"storageClass,omitempty"`

	// PBS backend (future)
	Server      string `yaml:"server,omitempty"`
	Port        int    `yaml:"port,omitempty"`
	Datastore   string `yaml:"datastore,omitempty"`
	Namespace   string `yaml:"namespace,omitempty"`
	Username    string `yaml:"username,omitempty"`
	Password    string `yaml:"password,omitempty"`
	Fingerprint string `yaml:"fingerprint,omitempty"`
	Verify      bool   `yaml:"verify,omitempty"`
}

func parseConfig(path string) (BackuparrConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BackuparrConfig{}, fmt.Errorf("error reading config file: %w", err)
	}
	var config BackuparrConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return BackuparrConfig{}, fmt.Errorf("error parsing config: %w", err)
	}
	return config, nil
}

func createClient(cfg AppConfig) (backup.Client, error) {
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
	default:
		return nil, fmt.Errorf("unsupported app type: %s", cfg.AppType)
	}
}

func createBackends(configs []StorageConfig) ([]storage.Backend, error) {
	if len(configs) == 0 {
		// Default to local storage in ./backups for backward compatibility
		return []storage.Backend{local.New("./backups")}, nil
	}

	var backends []storage.Backend
	for _, cfg := range configs {
		switch cfg.Type {
		case "local":
			path := cfg.Path
			if path == "" {
				path = "./backups"
			}
			backends = append(backends, local.New(path))
		case "s3":
			prefix := cfg.Prefix
			if prefix == "" {
				prefix = "backuparr"
			}
			s3cfg := s3backend.Config{
				Bucket:         cfg.Bucket,
				Prefix:         prefix,
				Region:         cfg.Region,
				Endpoint:       cfg.Endpoint,
				AccessKeyID:    cfg.AccessKeyID,
				SecretAccessKey: cfg.SecretAccessKey,
				StorageClass:   cfg.StorageClass,
			}
			backend, err := s3backend.New(context.Background(), s3cfg)
			if err != nil {
				return nil, fmt.Errorf("failed to create S3 backend: %w", err)
			}
			backends = append(backends, backend)
		case "pbs":
			pbsCfg := pbsbackend.Config{
				Server:      cfg.Server,
				Port:        cfg.Port,
				Datastore:   cfg.Datastore,
				Namespace:   cfg.Namespace,
				Username:    cfg.Username,
				Password:    cfg.Password,
				Fingerprint: cfg.Fingerprint,
			}
			backend, err := pbsbackend.New(pbsCfg)
			if err != nil {
				return nil, fmt.Errorf("failed to create PBS backend: %w", err)
			}
			backends = append(backends, backend)
		default:
			return nil, fmt.Errorf("unsupported storage type: %s", cfg.Type)
		}
	}
	return backends, nil
}

func toStorageRetention(r RetentionPolicy) storage.RetentionPolicy {
	return storage.RetentionPolicy{
		KeepLast:    r.KeepLast,
		KeepHourly:  r.KeepHourly,
		KeepDaily:   r.KeepDaily,
		KeepWeekly:  r.KeepWeekly,
		KeepMonthly: r.KeepMonthly,
		KeepYearly:  r.KeepYearly,
	}
}

func runBackup(ctx context.Context, app backup.Client, backends []storage.Backend, retention RetentionPolicy) error {
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
	ctx := context.Background()

	config, err := parseConfig("config.yml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	for _, appCfg := range config.AppConfigs {
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
