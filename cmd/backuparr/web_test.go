package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"backuparr/internal/config"
)

// --- job state race -------------------------------------------------------

// TestBackupJob_ConcurrentSnapshotRead starts backup jobs while concurrently
// reading their snapshots, under go test -race. Before the fix, startBackupJob
// launched the job's goroutine and then read job.Logs/job.Results outside
// s.mu, racing with that goroutine's writes (appendJobLog/finishJob). The app
// type below is unsupported so createClient fails immediately, giving the
// background goroutine something to write (logs, then results) as fast as
// possible, maximizing the odds of tripping the race detector.
func TestBackupJob_ConcurrentSnapshotRead(t *testing.T) {
	cfg := config.BackuparrConfig{
		AppConfigs: []config.AppConfig{
			{AppType: "unsupported-type", Name: "app-a"},
			{AppType: "unsupported-type", Name: "app-b"},
			{AppType: "unsupported-type", Name: "app-c"},
		},
	}

	for i := 0; i < 50; i++ {
		s := &webServer{cfg: cfg, jobs: map[string]*backupJob{}}

		job := s.startBackupJob(triggerBackupRequest{All: true})
		_ = job.Logs
		_ = job.Results

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				snap, ok := s.getJob(job.ID)
				if !ok || !snap.Running {
					return
				}
			}
		}()
		wg.Wait()
	}
}

// --- log cap ---------------------------------------------------------------

func TestAppendCappedLog_TruncatesAndMarks(t *testing.T) {
	j := &backupJob{}
	total := maxJobLogLines + 50
	for i := 0; i < total; i++ {
		appendCappedLog(j, fmt.Sprintf("line-%d", i))
	}

	if len(j.Logs) > maxJobLogLines {
		t.Fatalf("len(Logs) = %d, want at most %d", len(j.Logs), maxJobLogLines)
	}
	if j.LogsTruncated != total-(maxJobLogLines-1) {
		t.Errorf("LogsTruncated = %d, want %d", j.LogsTruncated, total-(maxJobLogLines-1))
	}
	if !strings.Contains(j.Logs[0], "truncated") {
		t.Fatalf("Logs[0] = %q, want a truncation marker", j.Logs[0])
	}

	want := fmt.Sprintf("line-%d", total-1)
	if got := j.Logs[len(j.Logs)-1]; got != want {
		t.Errorf("most recent log line = %q, want %q", got, want)
	}
}

func TestAppendJobRawLog_CapsThroughLockedPath(t *testing.T) {
	s := &webServer{jobs: map[string]*backupJob{}}
	id := "job-1"
	s.jobs[id] = &backupJob{ID: id}

	for i := 0; i < maxJobLogLines+25; i++ {
		s.appendJobRawLog(id, fmt.Sprintf("line-%d", i))
	}

	job, ok := s.getJob(id)
	if !ok {
		t.Fatal("job not found")
	}
	if len(job.Logs) > maxJobLogLines {
		t.Fatalf("len(Logs) = %d, want at most %d", len(job.Logs), maxJobLogLines)
	}
	if !strings.Contains(job.Logs[0], "truncated") {
		t.Fatalf("Logs[0] = %q, want a truncation marker", job.Logs[0])
	}
}

// --- job eviction ------------------------------------------------------------

func TestEvictStaleJobsLocked_ByAge(t *testing.T) {
	s := &webServer{jobs: map[string]*backupJob{}}
	old := time.Now().Add(-completedJobMaxAge - time.Hour)
	s.jobs["old"] = &backupJob{ID: "old", EndedAt: &old}
	recent := time.Now()
	s.jobs["recent"] = &backupJob{ID: "recent", EndedAt: &recent}

	s.mu.Lock()
	s.evictStaleJobsLocked()
	s.mu.Unlock()

	if _, ok := s.jobs["old"]; ok {
		t.Error("expected job past completedJobMaxAge to be evicted")
	}
	if _, ok := s.jobs["recent"]; !ok {
		t.Error("expected recently completed job to be retained")
	}
}

func TestEvictStaleJobsLocked_ByCount(t *testing.T) {
	s := &webServer{jobs: map[string]*backupJob{}}
	base := time.Now().Add(-time.Hour)
	total := maxCompletedJobs + 10
	for i := 0; i < total; i++ {
		end := base.Add(time.Duration(i) * time.Second)
		id := fmt.Sprintf("job-%d", i)
		s.jobs[id] = &backupJob{ID: id, EndedAt: &end}
	}

	s.mu.Lock()
	s.evictStaleJobsLocked()
	s.mu.Unlock()

	if len(s.jobs) != maxCompletedJobs {
		t.Fatalf("len(jobs) = %d, want %d", len(s.jobs), maxCompletedJobs)
	}
	if _, ok := s.jobs["job-0"]; ok {
		t.Error("expected oldest completed job to be evicted first")
	}
	newest := fmt.Sprintf("job-%d", total-1)
	if _, ok := s.jobs[newest]; !ok {
		t.Errorf("expected newest job %q to be retained", newest)
	}
}

func TestEvictStaleJobsLocked_RunningJobsSurvive(t *testing.T) {
	s := &webServer{jobs: map[string]*backupJob{}}
	s.jobs["running"] = &backupJob{ID: "running", Running: true, StartedAt: time.Now().Add(-completedJobMaxAge - time.Hour)}

	s.mu.Lock()
	s.evictStaleJobsLocked()
	s.mu.Unlock()

	if _, ok := s.jobs["running"]; !ok {
		t.Error("expected a still-running job to survive eviction regardless of age")
	}
}

// --- job IDs -----------------------------------------------------------------

var hexJobIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestNewJobID_FormatAndUniqueness(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := newJobID()
		if !hexJobIDPattern.MatchString(id) {
			t.Fatalf("newJobID() = %q, want 32 lowercase hex characters", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newJobID() produced duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewJobID_NotSequential guards against a regression back to the old
// time.Now().UnixNano()-based ID, which was guessable because consecutive IDs
// sorted in generation order. A random source should not preserve that order.
func TestNewJobID_NotSequential(t *testing.T) {
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = newJobID()
	}

	sorted := true
	for i := 1; i < len(ids); i++ {
		if ids[i] < ids[i-1] {
			sorted = false
			break
		}
	}
	if sorted {
		t.Fatal("newJobID() produced monotonically increasing ids; want unpredictable, non-sequential ids")
	}
}
