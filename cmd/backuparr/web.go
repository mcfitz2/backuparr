package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
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

// jobLogWriter is the io.Writer backing a job's dedicated *log.Logger. Each
// job gets its own logger instead of redirecting the process-global one, so
// concurrent jobs no longer serialize on a shared mutex and unrelated log
// output from the rest of the process never leaks into a job's log.
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
	Name      string        `json:"name"`
	AppType   string        `json:"appType"`
	Backends  []string      `json:"backends"`
	Retention retentionInfo `json:"retention"`
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

	// LogsTruncated is the running count of log lines dropped to keep Logs
	// bounded. Zero means nothing has been dropped.
	LogsTruncated int
}

const (
	// maxJobLogLines caps how many log lines a single job retains; once
	// exceeded, the oldest lines are dropped and replaced with a marker.
	maxJobLogLines = 1000

	// maxCompletedJobs and completedJobMaxAge bound s.jobs so a long-running
	// process doesn't accumulate job state forever. Running jobs are never
	// evicted regardless of age.
	maxCompletedJobs   = 50
	completedJobMaxAge = 24 * time.Hour
)

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

func newJobID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing indicates a broken OS entropy source; there is
		// no sane fallback that keeps IDs unguessable, so surface it loudly.
		panic(fmt.Sprintf("backuparr: failed to generate job id: %v", err))
	}
	return hex.EncodeToString(buf)
}

func (s *webServer) startBackupJob(req triggerBackupRequest) *backupJob {
	id := newJobID()
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

	// Read the snapshot back through getJob rather than the local job
	// pointer: the goroutine above may already be mutating that same
	// *backupJob concurrently, and getJob is the one path that snapshots
	// under s.mu.
	snapshot, _ := s.getJob(id)
	return snapshot
}

func (s *webServer) executeBackupJob(id string) {
	logger := log.New(jobLogWriter{server: s, jobID: id}, "", log.LstdFlags)

	if err := preflightCheck(s.cfg); err != nil {
		s.finishJob(id, false, []triggerBackupResult{}, []string{fmt.Sprintf("Preflight failed: %v", err)})
		return
	}

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
		name := config.AppConfigName(appCfg)
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

		if err := runBackup(ctx, client, backends, appCfg.Retention, logger); err != nil {
			results = append(results, triggerBackupResult{App: name, OK: false, Status: "failed", Error: err.Error()})
			s.appendJobLog(id, fmt.Sprintf("[%s] failed: %v", name, err))
			continue
		}

		results = append(results, triggerBackupResult{App: name, OK: true, Status: "ok"})
		s.appendJobLog(id, fmt.Sprintf("[%s] backup completed", name))
	}

	if len(results) == 0 {
		msg := "No targets configured"
		if targetApp != "" {
			msg = fmt.Sprintf("No matching app for %q", targetApp)
		}
		s.finishJob(id, false, results, []string{msg})
		return
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
		appendCappedLog(j, fmt.Sprintf("%s %s", time.Now().UTC().Format(time.RFC3339), line))
	}
}

func (s *webServer) appendJobRawLog(id, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		appendCappedLog(j, line)
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
			appendCappedLog(j, fmt.Sprintf("%s %s", now.Format(time.RFC3339), line))
		}
	}
	s.evictStaleJobsLocked()
}

// appendCappedLog appends line to j.Logs, keeping at most maxJobLogLines
// entries. Once older lines are dropped, the oldest surviving entry is
// replaced with a running count of everything discarded so far, so the log
// never grows unbounded but the user can tell output was dropped. Callers
// must hold s.mu for writing.
func appendCappedLog(j *backupJob, line string) {
	content := j.Logs
	if j.LogsTruncated > 0 && len(content) > 0 {
		content = content[1:] // drop the stale marker; it's regenerated below
	}
	content = append(content, line)

	const limit = maxJobLogLines - 1 // reserve one slot for the marker
	if drop := len(content) - limit; drop > 0 {
		j.LogsTruncated += drop
		content = content[drop:]
	}

	if j.LogsTruncated > 0 {
		marker := fmt.Sprintf("... %d earlier line(s) truncated ...", j.LogsTruncated)
		j.Logs = append([]string{marker}, content...)
		return
	}
	j.Logs = content
}

// evictStaleJobsLocked drops completed jobs older than completedJobMaxAge and,
// if more than maxCompletedJobs remain, the oldest of those too. Running jobs
// are never evicted. Callers must hold s.mu for writing.
func (s *webServer) evictStaleJobsLocked() {
	now := time.Now()
	type candidate struct {
		id      string
		endedAt time.Time
	}
	var completed []candidate

	for id, j := range s.jobs {
		if j.Running {
			continue
		}
		endedAt := j.StartedAt
		if j.EndedAt != nil {
			endedAt = *j.EndedAt
		}
		if now.Sub(endedAt) > completedJobMaxAge {
			delete(s.jobs, id)
			continue
		}
		completed = append(completed, candidate{id: id, endedAt: endedAt})
	}

	if len(completed) <= maxCompletedJobs {
		return
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].endedAt.Before(completed[j].endedAt)
	})
	for _, c := range completed[:len(completed)-maxCompletedJobs] {
		delete(s.jobs, c.id)
	}
}

func (s *webServer) getJob(id string) (*backupJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	return snapshotJobLocked(job), true
}

// snapshotJobLocked copies a job's mutable fields into a fresh *backupJob so
// callers can read it without holding s.mu afterward. The caller must hold
// s.mu (for reading or writing) for the duration of this call.
func snapshotJobLocked(job *backupJob) *backupJob {
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
		ID:            job.ID,
		StartedAt:     job.StartedAt,
		EndedAt:       endedAt,
		Running:       job.Running,
		Success:       success,
		Request:       job.Request,
		Results:       results,
		Logs:          logs,
		LogsTruncated: job.LogsTruncated,
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
