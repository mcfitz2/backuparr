package main

import (
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

// config holds the sidecar runtime configuration, loaded from environment variables.
type config struct {
	BackupPath      string   // BACKUP_PATH (required) — directory to back up / restore to
	ExcludePatterns []string // EXCLUDE_PATTERNS — comma-separated glob patterns to exclude
	APIPort         string   // API_PORT (default "8484") — HTTP listen port
	APIKey          string   // API_KEY — optional shared secret for X-Api-Key auth

	// Docker restart (optional)
	DockerContainer string // DOCKER_CONTAINER — container name/ID to restart
	DockerHost      string // DOCKER_HOST (default "/var/run/docker.sock") — Docker socket path

	// Kubernetes restart (optional)
	KubePod       string // KUBE_POD — pod name to delete (controller recreates)
	KubeNamespace string // KUBE_NAMESPACE — K8s namespace (auto-detected from SA if empty)
}

// loadConfig reads configuration from environment variables.
func loadConfig() (*config, error) {
	cfg := &config{
		BackupPath:      os.Getenv("BACKUP_PATH"),
		APIPort:         os.Getenv("API_PORT"),
		APIKey:          os.Getenv("API_KEY"),
		DockerContainer: os.Getenv("DOCKER_CONTAINER"),
		DockerHost:      os.Getenv("DOCKER_HOST"),
		KubePod:         os.Getenv("KUBE_POD"),
		KubeNamespace:   os.Getenv("KUBE_NAMESPACE"),
	}

	// Parse exclude patterns
	if v := os.Getenv("EXCLUDE_PATTERNS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.ExcludePatterns = append(cfg.ExcludePatterns, p)
			}
		}
	}

	// Defaults
	if cfg.APIPort == "" {
		cfg.APIPort = "8484"
	}
	if cfg.DockerHost == "" && cfg.DockerContainer != "" {
		cfg.DockerHost = "/var/run/docker.sock"
	}

	// Validation
	if cfg.BackupPath == "" {
		return nil, fmt.Errorf("BACKUP_PATH is required")
	}
	info, err := os.Stat(cfg.BackupPath)
	if err != nil {
		return nil, fmt.Errorf("BACKUP_PATH %q: %w", cfg.BackupPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("BACKUP_PATH %q is not a directory", cfg.BackupPath)
	}

	if cfg.DockerContainer != "" && cfg.KubePod != "" {
		return nil, fmt.Errorf("DOCKER_CONTAINER and KUBE_POD are mutually exclusive")
	}

	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[sidecar] Configuration error: %v", err)
	}

	log.Printf("[sidecar] Starting backup sidecar")
	log.Printf("[sidecar]   Backup path:  %s", cfg.BackupPath)
	if len(cfg.ExcludePatterns) > 0 {
		log.Printf("[sidecar]   Excludes:     %s", strings.Join(cfg.ExcludePatterns, ", "))
	}
	if cfg.APIKey != "" {
		log.Printf("[sidecar]   API key:      configured")
	}
	if cfg.DockerContainer != "" {
		log.Printf("[sidecar]   Docker:       container=%s socket=%s", cfg.DockerContainer, cfg.DockerHost)
	}
	if cfg.KubePod != "" {
		ns := cfg.KubeNamespace
		if ns == "" {
			ns = "(auto-detect)"
		}
		log.Printf("[sidecar]   Kubernetes:   pod=%s namespace=%s", cfg.KubePod, ns)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", authMiddleware(cfg.APIKey, handleHealth(cfg)))
	mux.HandleFunc("/api/v1/backup", authMiddleware(cfg.APIKey, handleBackup(cfg)))
	mux.HandleFunc("/api/v1/restore", authMiddleware(cfg.APIKey, handleRestore(cfg)))
	mux.HandleFunc("/api/v1/restart", authMiddleware(cfg.APIKey, handleRestart(cfg)))

	addr := ":" + cfg.APIPort
	log.Printf("[sidecar] Listening on %s", addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[sidecar] Server error: %v", err)
	}
}

// newCertPool creates a certificate pool from PEM-encoded certificate data.
func newCertPool(pemData []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}
	return pool, nil
}
