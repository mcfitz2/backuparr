package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"backuparr/internal/config"
	"backuparr/internal/storage"
	"github.com/gorilla/websocket"
)

//go:embed webui/*
var webUIFS embed.FS

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var logCaptureMu sync.Mutex

type jobLogWriter struct {
	server *webServer
	jobID  string
}

func (w jobLogWriter) Write(p []byte) (int, error) {
	lines := strings.Split(string(p), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		w.server.appendJobRawLog(w.jobID, line)
	}
	return len(p), nil
}

type webServer struct {
	cfg  config.BackuparrConfig
	mu   sync.RWMutex
	jobs map[string]*backupJob
}

type appOption struct {
	Name      string          `json:"name"`
	AppType   string          `json:"appType"`
	Backends  []string        `json:"backends"`
	Retention retentionInfo   `json:"retention"`
}

type retentionInfo struct {
	KeepLast    int `json:"keepLast"`
	KeepHourly  int `json:"keepHourly"`
	KeepDaily   int `json:"keepDaily"`
	KeepWeekly  int `json:"keepWeekly"`
	KeepMonthly int `json:"keepMonthly"`
	KeepYearly  int `json:"keepYearly"`
}

type appsResponse struct {
	Apps []appOption `json:"apps"`
}

type triggerBackupRequest struct {
	App string `json:"app,omitempty"`
	All bool   `json:"all,omitempty"`
}

type triggerBackupResult struct {
	App    string `json:"app"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Status string `json:"status"`
}

type triggerBackupResponse struct {
	JobID     string                `json:"jobId,omitempty"`
	Running   bool                  `json:"running"`
	Success   *bool                 `json:"success,omitempty"`
	Status    string                `json:"status"`
	Results   []triggerBackupResult `json:"results,omitempty"`
	Logs      []string              `json:"logs,omitempty"`
	StartedAt time.Time             `json:"startedAt"`
	EndedAt   *time.Time            `json:"endedAt,omitempty"`
}

type backupJob struct {
	ID        string
	StartedAt time.Time
	EndedAt   *time.Time
	Running   bool
	Success   *bool
	Request   triggerBackupRequest
	Results   []triggerBackupResult
	Logs      []string
}

func runWebUI() {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "HTTP listen address")
	configPath := fs.String("config", "", "Path to config file (overrides BACKUPARR_CONFIG)")
	fs.Parse(os.Args[2:])

	path := config.Path()
	if *configPath != "" {
		path = *configPath
	}

	cfg, err := config.Parse(path)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	s := &webServer{cfg: cfg, jobs: map[string]*backupJob{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", s.handleApps)
	mux.HandleFunc("/api/backups", s.handleBackups)
	mux.HandleFunc("/api/backup", s.handleTriggerBackup)
	mux.HandleFunc("/api/backup/ws", s.handleBackupWS)

	staticFS, err := fsSub(webUIFS, "webui")
	if err != nil {
		log.Fatalf("Failed to initialize web UI assets: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	log.Printf("Backuparr web UI listening on %s (config: %s)", *listen, path)
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
			Retention: retentionInfo{
				KeepLast:    ac.Retention.KeepLast,
				KeepHourly:  ac.Retention.KeepHourly,
				KeepDaily:   ac.Retention.KeepDaily,
				KeepWeekly:  ac.Retention.KeepWeekly,
				KeepMonthly: ac.Retention.KeepMonthly,
				KeepYearly:  ac.Retention.KeepYearly,
			},
		})
	}

	sort.Slice(apps, func(i, j int) bool {
		return apps[i].Name < apps[j].Name
	})

	writeJSON(w, http.StatusOK, appsResponse{Apps: apps})
}

func (s *webServer) handleTriggerBackup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req triggerBackupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		targetApp := ""
		if !req.All {
			targetApp = req.App
			if targetApp == "" {
				writeError(w, http.StatusBadRequest, "set all=true or provide app")
				return
			}

			_, err := findAppConfig(s.cfg, targetApp)
			if err != nil {
				writeError(w, http.StatusNotFound, "app not found")
				return
			}
		}

		job := s.startBackupJob(req)
		writeJSON(w, http.StatusAccepted, s.toJobResponse(job))
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "query param id is required")
			return
		}

		job, ok := s.getJob(id)
		if !ok {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}

		writeJSON(w, http.StatusOK, s.toJobResponse(job))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *webServer) handleBackupWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "query param id is required")
		return
	}

	_, ok := s.getJob(id)
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	writeSnapshot := func() bool {
		job, exists := s.getJob(id)
		if !exists {
			_ = conn.WriteJSON(map[string]string{"error": "job not found"})
			return false
		}

		resp := s.toJobResponse(job)
		if err := conn.WriteJSON(resp); err != nil {
			return false
		}

		if !resp.Running {
			return false
		}
		return true
	}

	if cont := writeSnapshot(); !cont {
		return
	}

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
			if cont := writeSnapshot(); !cont {
				return
			}
		}
	}
}

func (s *webServer) startBackupJob(req triggerBackupRequest) *backupJob {
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	job := &backupJob{
		ID:        id,
		StartedAt: time.Now().UTC(),
		Running:   true,
		Request:   req,
		Results:   []triggerBackupResult{},
		Logs:      []string{"Backup job started"},
	}

	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()

	go s.executeBackupJob(id)
	return s.snapshotJob(job)
}

func (s *webServer) executeBackupJob(id string) {
	if err := preflightCheck(s.cfg); err != nil {
		s.finishJob(id, false, []triggerBackupResult{}, []string{fmt.Sprintf("Preflight failed: %v", err)})
		return
	}

	logCaptureMu.Lock()
	baseLogWriter := log.Writer()
	log.SetOutput(io.MultiWriter(baseLogWriter, jobLogWriter{server: s, jobID: id}))
	defer func() {
		log.SetOutput(baseLogWriter)
		logCaptureMu.Unlock()
	}()

	targetApp := ""
	s.mu.RLock()
	if j, ok := s.jobs[id]; ok {
		if !j.Request.All {
			targetApp = j.Request.App
		}
	}
	s.mu.RUnlock()

	ctx := context.Background()
	results := make([]triggerBackupResult, 0, len(s.cfg.AppConfigs))
	jobLogs := make([]string, 0, 32)

	for _, appCfg := range s.cfg.AppConfigs {
		name := appCfg.Name
		if name == "" {
			name = appCfg.AppType
		}
		if targetApp != "" && name != targetApp {
			continue
		}

		s.appendJobLog(id, fmt.Sprintf("[%s] Starting backup", name))

		client, err := createClient(appCfg)
		if err != nil {
			msg := "failed to create app client"
			results = append(results, triggerBackupResult{App: name, OK: false, Status: "failed", Error: msg})
			s.appendJobLog(id, fmt.Sprintf("[%s] %s", name, msg))
			continue
		}

		backends, err := createBackends(appCfg.Storage)
		if err != nil {
			msg := "failed to create storage backends"
			results = append(results, triggerBackupResult{App: name, OK: false, Status: "failed", Error: msg})
			s.appendJobLog(id, fmt.Sprintf("[%s] %s", name, msg))
			continue
		}

		if err := runBackup(ctx, client, backends, appCfg.Retention); err != nil {
			results = append(results, triggerBackupResult{App: name, OK: false, Status: "failed", Error: err.Error()})
			s.appendJobLog(id, fmt.Sprintf("[%s] failed: %v", name, err))
			continue
		}

		results = append(results, triggerBackupResult{App: name, OK: true, Status: "ok"})
		s.appendJobLog(id, fmt.Sprintf("[%s] backup completed", name))
	}

	success := true
	for _, r := range results {
		if !r.OK {
			success = false
			break
		}
	}

	if success {
		jobLogs = append(jobLogs, "Backup job completed successfully")
	} else {
		jobLogs = append(jobLogs, "Backup job completed with failures")
	}

	s.finishJob(id, success, results, jobLogs)
}

func (s *webServer) appendJobLog(id, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		j.Logs = append(j.Logs, fmt.Sprintf("%s %s", time.Now().UTC().Format(time.RFC3339), line))
	}
}

func (s *webServer) appendJobRawLog(id, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		j.Logs = append(j.Logs, line)
	}
}

func (s *webServer) finishJob(id string, success bool, results []triggerBackupResult, logs []string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		j.Running = false
		j.Success = &success
		j.Results = results
		j.EndedAt = &now
		for _, line := range logs {
			j.Logs = append(j.Logs, fmt.Sprintf("%s %s", now.Format(time.RFC3339), line))
		}
	}
}

func (s *webServer) getJob(id string) (*backupJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	return s.snapshotJob(job), true
}

func (s *webServer) snapshotJob(job *backupJob) *backupJob {
	logs := make([]string, len(job.Logs))
	copy(logs, job.Logs)
	results := make([]triggerBackupResult, len(job.Results))
	copy(results, job.Results)

	var endedAt *time.Time
	if job.EndedAt != nil {
		t := *job.EndedAt
		endedAt = &t
	}

	var success *bool
	if job.Success != nil {
		v := *job.Success
		success = &v
	}

	return &backupJob{
		ID:        job.ID,
		StartedAt: job.StartedAt,
		EndedAt:   endedAt,
		Running:   job.Running,
		Success:   success,
		Request:   job.Request,
		Results:   results,
		Logs:      logs,
	}
}

func (s *webServer) toJobResponse(job *backupJob) triggerBackupResponse {
	status := "running"
	if !job.Running {
		if job.Success != nil && *job.Success {
			status = "completed"
		} else {
			status = "failed"
		}
	}

	return triggerBackupResponse{
		JobID:     job.ID,
		Running:   job.Running,
		Success:   job.Success,
		Status:    status,
		Results:   job.Results,
		Logs:      job.Logs,
		StartedAt: job.StartedAt,
		EndedAt:   job.EndedAt,
	}
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

		policy := toStorageRetention(appCfg.Retention)
		bucketMap := storage.ClassifyRetentionBuckets(backups, policy)

		type backupWithBuckets struct {
			Key              string    `json:"key"`
			AppName          string    `json:"appName"`
			FileName         string    `json:"fileName"`
			Size             int64     `json:"size"`
			CreatedAt        time.Time `json:"createdAt"`
			RetentionBuckets []string  `json:"retentionBuckets"`
		}

		enriched := make([]backupWithBuckets, 0, len(backups))
		for _, b := range backups {
			buckets := bucketMap[b.Key]
			if buckets == nil {
				buckets = []string{}
			}
			enriched = append(enriched, backupWithBuckets{
				Key:              b.Key,
				AppName:          b.AppName,
				FileName:         b.FileName,
				Size:             b.Size,
				CreatedAt:        b.CreatedAt,
				RetentionBuckets: buckets,
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{"backups": enriched})
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
