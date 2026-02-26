package storage

import (
	"context"
	"log"
	"sort"
	"time"
)

// RetentionPolicy defines how many backups to keep in each time bucket.
type RetentionPolicy struct {
	KeepLast    int
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
}

// ApplyRetention lists existing backups and deletes those that exceed the policy.
// Returns the number of backups deleted.
func ApplyRetention(ctx context.Context, backend Backend, appName string, policy RetentionPolicy) (int, error) {
	backups, err := backend.List(ctx, appName)
	if err != nil {
		return 0, err
	}

	if len(backups) == 0 {
		return 0, nil
	}

	toKeep := selectBackupsToKeep(backups, policy)

	deleted := 0
	for _, b := range backups {
		if _, keep := toKeep[b.Key]; !keep {
			if err := backend.Delete(ctx, b.Key); err != nil {
				log.Printf("[%s] Failed to delete old backup %s: %v", appName, b.FileName, err)
				continue
			}
			deleted++
		}
	}

	return deleted, nil
}

// selectBackupsToKeep returns a set of backup keys that should be retained.
// The algorithm is modeled after restic/PBS/Borg: each backup is assigned to
// time buckets, and the oldest backup in each bucket is kept.
func selectBackupsToKeep(backups []BackupMetadata, policy RetentionPolicy) map[string]struct{} {
	keep := make(map[string]struct{})

	// Ensure sorted newest-first
	sorted := make([]BackupMetadata, len(backups))
	copy(sorted, backups)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	// KeepLast: keep the N most recent
	for i := 0; i < policy.KeepLast && i < len(sorted); i++ {
		keep[sorted[i].Key] = struct{}{}
	}

	// Time-based buckets: for each bucket type, walk backups oldest-first
	// and keep one per distinct time period.
	if policy.KeepHourly > 0 {
		markByBucket(sorted, policy.KeepHourly, truncateHour, keep)
	}
	if policy.KeepDaily > 0 {
		markByBucket(sorted, policy.KeepDaily, truncateDay, keep)
	}
	if policy.KeepWeekly > 0 {
		markByBucket(sorted, policy.KeepWeekly, truncateWeek, keep)
	}
	if policy.KeepMonthly > 0 {
		markByBucket(sorted, policy.KeepMonthly, truncateMonth, keep)
	}
	if policy.KeepYearly > 0 {
		markByBucket(sorted, policy.KeepYearly, truncateYear, keep)
	}

	return keep
}

// markByBucket walks backups newest-first, assigns each to a time bucket using
// the truncation function, and keeps the newest backup in up to `count` distinct buckets.
func markByBucket(sortedNewestFirst []BackupMetadata, count int, truncate func(time.Time) time.Time, keep map[string]struct{}) {
	seen := make(map[time.Time]bool)
	for _, b := range sortedNewestFirst {
		bucket := truncate(b.CreatedAt)
		if !seen[bucket] {
			seen[bucket] = true
			keep[b.Key] = struct{}{}
			if len(seen) >= count {
				return
			}
		}
	}
}

func truncateHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func truncateWeek(t time.Time) time.Time {
	// ISO week: Monday is the first day
	year, week := t.ISOWeek()
	// Find the Monday of this ISO week
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, t.Location())
	weekday := jan4.Weekday()
	if weekday == 0 {
		weekday = 7
	}
	monday := jan4.AddDate(0, 0, -(int(weekday)-1)+(week-1)*7)
	return monday
}

func truncateMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

func truncateYear(t time.Time) time.Time {
	return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
}

// ClassifyRetentionBuckets returns a map from backup key to the retention
// bucket labels (e.g. "daily", "weekly") that justify keeping it. Backups
// that would be pruned are mapped to an empty slice.
func ClassifyRetentionBuckets(backups []BackupMetadata, policy RetentionPolicy) map[string][]string {
	labels := make(map[string][]string, len(backups))
	for _, b := range backups {
		labels[b.Key] = nil
	}

	sorted := make([]BackupMetadata, len(backups))
	copy(sorted, backups)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	// KeepLast
	for i := 0; i < policy.KeepLast && i < len(sorted); i++ {
		labels[sorted[i].Key] = append(labels[sorted[i].Key], "latest")
	}

	type bucketDef struct {
		count    int
		truncate func(time.Time) time.Time
		label    string
	}
	defs := []bucketDef{
		{policy.KeepHourly, truncateHour, "hourly"},
		{policy.KeepDaily, truncateDay, "daily"},
		{policy.KeepWeekly, truncateWeek, "weekly"},
		{policy.KeepMonthly, truncateMonth, "monthly"},
		{policy.KeepYearly, truncateYear, "yearly"},
	}

	for _, def := range defs {
		if def.count <= 0 {
			continue
		}
		seen := make(map[time.Time]bool)
		for _, b := range sorted {
			bucket := def.truncate(b.CreatedAt)
			if !seen[bucket] {
				seen[bucket] = true
				labels[b.Key] = append(labels[b.Key], def.label)
				if len(seen) >= def.count {
					break
				}
			}
		}
	}

	return labels
}
