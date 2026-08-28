package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
)

// handleHealth reports sidecar status and capabilities.
// GET /api/v1/health
func handleHealth(cfg *config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			httpError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		resp := map[string]any{
			"status":     "ok",
			"backupPath": cfg.BackupPath,
			"restart": map[string]any{
				"docker":     cfg.DockerContainer != "",
				"kubernetes": cfg.KubePod != "",
			},
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// handleBackup creates a ZIP backup and streams it to the response.
// The archive is built to a temp file first so that a failure never leaves
// the client holding an empty or truncated ZIP behind a 200 OK: headers
// are only written once the archive is known to be complete.
// POST /api/v1/backup
func handleBackup(cfg *config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		log.Printf("[sidecar] Backup requested for %s", cfg.BackupPath)

		tmp, err := os.CreateTemp("", "sidecar-backup-*.zip")
		if err != nil {
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create temp file: %v", err))
			return
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()

		stats, err := createBackup(cfg.BackupPath, cfg.ExcludePatterns, tmp)
		if err != nil {
			log.Printf("[sidecar] Backup failed: %v", err)
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("backup failed: %v", err))
			return
		}

		info, err := tmp.Stat()
		if err != nil {
			log.Printf("[sidecar] Backup failed: %v", err)
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to stat backup: %v", err))
			return
		}

		sum := sha256.New()
		if _, err := io.Copy(sum, tmp); err != nil {
			log.Printf("[sidecar] Backup failed: %v", err)
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to checksum backup: %v", err))
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			log.Printf("[sidecar] Backup failed: %v", err)
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("failed to rewind backup: %v", err))
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="backup.zip"`)
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.Header().Set("X-Backup-Sha256", hex.EncodeToString(sum.Sum(nil)))
		w.WriteHeader(http.StatusOK)

		if _, err := io.Copy(w, tmp); err != nil {
			// Headers are already committed at this point; nothing left to do
			// but log — the client will see a short read and can retry.
			log.Printf("[sidecar] Backup failed while streaming response: %v", err)
			return
		}

		log.Printf("[sidecar] Backup complete: %d files (%d SQLite), %d bytes",
			stats.TotalFiles, stats.SQLiteFiles, stats.TotalBytes)
	}
}

// handleRestore accepts a ZIP upload and extracts it to the backup path.
// Optionally restarts the target container/pod after a successful restore.
// POST /api/v1/restore  (multipart/form-data with field "backup")
func handleRestore(cfg *config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		log.Printf("[sidecar] Restore requested for %s", cfg.BackupPath)

		// Limit upload to 2 GB
		r.Body = http.MaxBytesReader(w, r.Body, 2<<30)

		file, _, err := r.FormFile("backup")
		if err != nil {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("missing or invalid 'backup' form file: %v", err))
			return
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("failed to read upload: %v", err))
			return
		}

		stats, err := restoreFromZip(cfg.BackupPath, data)
		if err != nil {
			httpError(w, http.StatusInternalServerError, fmt.Sprintf("restore failed: %v", err))
			return
		}

		log.Printf("[sidecar] Restore complete: %d files, %d bytes", stats.FilesRestored, stats.BytesRestored)

		// Attempt restart if configured
		restart := tryRestart(cfg)
		if restart.Attempted {
			if restart.Success {
				log.Printf("[sidecar] App restarted via %s", restart.Method)
			} else {
				log.Printf("[sidecar] Warning: restart via %s failed: %s", restart.Method, restart.Error)
			}
		}

		resp := map[string]any{
			"success":       true,
			"filesRestored": stats.FilesRestored,
			"bytesRestored": stats.BytesRestored,
			"restart":       restart,
			"message":       restoreMessage(stats, restart),
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleRestart triggers a container/pod restart without restoring any data.
// POST /api/v1/restart
func handleRestart(cfg *config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		restart := tryRestart(cfg)
		if !restart.Attempted {
			httpError(w, http.StatusBadRequest, "no restart method configured (set DOCKER_CONTAINER or KUBE_POD)")
			return
		}

		status := http.StatusOK
		if !restart.Success {
			status = http.StatusInternalServerError
		}

		writeJSON(w, status, map[string]any{
			"success": restart.Success,
			"restart": restart,
		})
	}
}

// authMiddleware checks the X-Api-Key header if an API key is configured.
// An empty apiKey means the operator explicitly opted into ALLOW_NO_AUTH;
// loadConfig refuses to start otherwise.
func authMiddleware(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	if apiKey == "" {
		return next // ALLOW_NO_AUTH opt-in
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !constantTimeEqual(r.Header.Get("X-Api-Key"), apiKey) {
			httpError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, r)
	}
}

// constantTimeEqual compares two strings without leaking their length or
// content through timing. Hashing both sides to a fixed-length digest first
// means the ConstantTimeCompare call always runs on equal-length input.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// restoreMessage generates a human-readable summary of a restore operation.
func restoreMessage(stats *restoreStats, restart restartResult) string {
	msg := fmt.Sprintf("Restored %d files (%d bytes)", stats.FilesRestored, stats.BytesRestored)
	if restart.Attempted {
		if restart.Success {
			msg += fmt.Sprintf(", app restarted via %s", restart.Method)
		} else {
			msg += fmt.Sprintf(", but restart via %s failed: %s — please restart manually", restart.Method, restart.Error)
		}
	}
	return msg
}

// writeJSON serializes v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// httpError writes a JSON error response.
func httpError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"success": false,
		"error":   message,
	})
}
