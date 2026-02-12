package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// BackuparrConfig is the top-level configuration.
type BackuparrConfig struct {
	AppConfigs []AppConfig `yaml:"appConfigs"`
}

// AppConfig configures a single application to back up.
type AppConfig struct {
	AppType    string            `yaml:"appType"`
	Name       string            `yaml:"name,omitempty"` // optional display name; defaults to appType
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
	Name string `yaml:"name,omitempty"` // optional display name; defaults to type
	Type string `yaml:"type"`           // "local", "s3"

	// Local backend
	Path string `yaml:"path,omitempty"`

	// S3 backend
	Bucket          string `yaml:"bucket,omitempty"`
	Prefix          string `yaml:"prefix,omitempty"`
	Region          string `yaml:"region,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"`
	AccessKeyID     string `yaml:"accessKeyId,omitempty"`
	SecretAccessKey string `yaml:"secretAccessKey,omitempty"`
	StorageClass    string `yaml:"storageClass,omitempty"`
}

// Parse reads and parses the config file at the given path.
func Parse(path string) (BackuparrConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BackuparrConfig{}, fmt.Errorf("error reading config file: %w", err)
	}
	var cfg BackuparrConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return BackuparrConfig{}, fmt.Errorf("error parsing config: %w", err)
	}
	return cfg, nil
}

// Path resolves the config file path from (in order of priority):
// 1. BACKUPARR_CONFIG environment variable
// 2. /config/config.yml (Docker default)
// 3. ./config.yml (local development fallback)
func Path() string {
	if v := os.Getenv("BACKUPARR_CONFIG"); v != "" {
		return v
	}
	// Docker default location
	if _, err := os.Stat("/config/config.yml"); err == nil {
		return "/config/config.yml"
	}
	// Local development fallback
	return "config.yml"
}

// StorageConfigName returns the effective name for a storage config entry.
// If a custom name is set it takes precedence; otherwise the type is used.
func StorageConfigName(sc StorageConfig) string {
	if sc.Name != "" {
		return sc.Name
	}
	return sc.Type
}
