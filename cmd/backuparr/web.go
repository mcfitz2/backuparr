package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"

	"backuparr/internal/config"
)

//go:embed webui/*
var webUIFS embed.FS

type webServer struct {
	cfg config.BackuparrConfig
}

type appOption struct {
	Name     string   `json:"name"`
	AppType  string   `json:"appType"`
	Backends []string `json:"backends"`
}

type appsResponse struct {
	Apps []appOption `json:"apps"`
}

func runWebUI() {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "HTTP listen address")
	fs.Parse(os.Args[2:])

	cfg, err := config.Parse(config.Path())
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	s := &webServer{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", s.handleApps)
	mux.HandleFunc("/api/backups", s.handleBackups)

	staticFS, err := fsSub(webUIFS, "webui")
	if err != nil {
		log.Fatalf("Failed to initialize web UI assets: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	log.Printf("Backuparr web UI listening on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("Web server failed: %v", err)
	}
}

func fsSub(fsys embed.FS, dir string) (fs.FS, error) {
	return fs.Sub(fsys, dir)
}

func (s *webServer) handleApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	apps := make([]appOption, 0, len(s.cfg.AppConfigs))
	for _, ac := range s.cfg.AppConfigs {
		name := ac.Name
		if name == "" {
			name = ac.AppType
		}

		seen := map[string]struct{}{}
		backends := make([]string, 0, len(ac.Storage))
		for _, sc := range ac.Storage {
			bn := config.StorageConfigName(sc)
			if _, ok := seen[bn]; ok {
				continue
			}
			seen[bn] = struct{}{}
			backends = append(backends, bn)
		}
		sort.Strings(backends)

		apps = append(apps, appOption{
			Name:     name,
			AppType:  ac.AppType,
			Backends: backends,
		})
	}

	sort.Slice(apps, func(i, j int) bool {
		return apps[i].Name < apps[j].Name
	})

	writeJSON(w, http.StatusOK, appsResponse{Apps: apps})
}

func (s *webServer) handleBackups(w http.ResponseWriter, r *http.Request) {
	appName := r.URL.Query().Get("app")
	backendName := r.URL.Query().Get("backend")
	if appName == "" || backendName == "" {
		writeError(w, http.StatusBadRequest, "query params app and backend are required")
		return
	}

	appCfg, err := findAppConfig(s.cfg, appName)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	backend, err := findBackend(appCfg, backendName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := context.Background()

	switch r.Method {
	case http.MethodGet:
		backups, err := backend.List(ctx, appName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list backups")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
	case http.MethodDelete:
		key := r.URL.Query().Get("key")
		if key == "" {
			writeError(w, http.StatusBadRequest, "query param key is required")
			return
		}
		if err := backend.Delete(ctx, key); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete backup")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
