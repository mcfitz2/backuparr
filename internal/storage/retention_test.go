package storage

import (
	"testing"
	"time"
)

func TestFormatBackupName(t *testing.T) {
	ts := time.Date(2026, 2, 6, 12, 30, 45, 0, time.UTC)
	got := FormatBackupName("sonarr", ts)
	want := "sonarr_2026-02-06T123045Z.zip"
	if got != want {
		t.Errorf("FormatBackupName = %q, want %q", got, want)
	}
}

func TestFormatBackupName_NonUTC(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	ts := time.Date(2026, 2, 6, 8, 0, 0, 0, loc) // 08:00 EST = 13:00 UTC
	got := FormatBackupName("radarr", ts)
	want := "radarr_2026-02-06T130000Z.zip"
	if got != want {
		t.Errorf("FormatBackupName = %q, want %q", got, want)
	}
}

func makeBackup(key string, created time.Time) BackupMetadata {
	return BackupMetadata{
		Key:       key,
		AppName:   "test",
		FileName:  key + ".zip",
		Size:      1024,
		CreatedAt: created,
	}
}

func TestSelectBackupsToKeep_KeepLast(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	backups := []BackupMetadata{
		makeBackup("b1", now.Add(-3*time.Hour)),
		makeBackup("b2", now.Add(-2*time.Hour)),
		makeBackup("b3", now.Add(-1*time.Hour)),
		makeBackup("b4", now),
	}
	policy := RetentionPolicy{KeepLast: 2}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["b4"]; !ok {
		t.Error("expected b4 (newest) to be kept")
	}
	if _, ok := keep["b3"]; !ok {
		t.Error("expected b3 (second newest) to be kept")
	}
	if _, ok := keep["b2"]; ok {
		t.Error("expected b2 to be pruned")
	}
	if _, ok := keep["b1"]; ok {
		t.Error("expected b1 to be pruned")
	}
}

func TestSelectBackupsToKeep_KeepHourly(t *testing.T) {
	base := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	backups := []BackupMetadata{
		makeBackup("h10a", base.Add(15*time.Minute)),
		makeBackup("h10b", base.Add(45*time.Minute)),
		makeBackup("h11a", base.Add(75*time.Minute)),
		makeBackup("h11b", base.Add(105*time.Minute)),
	}
	policy := RetentionPolicy{KeepHourly: 2}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["h11b"]; !ok {
		t.Error("expected h11b (newest in 11:xx bucket) to be kept")
	}
	if _, ok := keep["h10b"]; !ok {
		t.Error("expected h10b (newest in 10:xx bucket) to be kept")
	}
	if len(keep) != 2 {
		t.Errorf("expected 2 kept, got %d", len(keep))
	}
}

func TestSelectBackupsToKeep_KeepDaily(t *testing.T) {
	backups := []BackupMetadata{
		makeBackup("d1a", time.Date(2026, 6, 13, 8, 0, 0, 0, time.UTC)),
		makeBackup("d1b", time.Date(2026, 6, 13, 20, 0, 0, 0, time.UTC)),
		makeBackup("d2", time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)),
		makeBackup("d3", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
	}
	policy := RetentionPolicy{KeepDaily: 3}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["d3"]; !ok {
		t.Error("expected d3 to be kept")
	}
	if _, ok := keep["d2"]; !ok {
		t.Error("expected d2 to be kept")
	}
	if _, ok := keep["d1b"]; !ok {
		t.Error("expected d1b (newest on June 13) to be kept")
	}
	if _, ok := keep["d1a"]; ok {
		t.Error("expected d1a (older on June 13) to be pruned")
	}
}

func TestSelectBackupsToKeep_KeepWeekly(t *testing.T) {
	backups := []BackupMetadata{
		makeBackup("w1", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)),
		makeBackup("w2", time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)),
		makeBackup("w3", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
		makeBackup("w4", time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)),
	}
	policy := RetentionPolicy{KeepWeekly: 2}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["w4"]; !ok {
		t.Error("expected w4 (newest in Week 25) to be kept")
	}
	if _, ok := keep["w2"]; !ok {
		t.Error("expected w2 (Week 24) to be kept")
	}
	if len(keep) != 2 {
		t.Errorf("expected 2 kept, got %d", len(keep))
	}
}

func TestSelectBackupsToKeep_KeepMonthly(t *testing.T) {
	backups := []BackupMetadata{
		makeBackup("m1", time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)),
		makeBackup("m2", time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)),
		makeBackup("m3", time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)),
		makeBackup("m4", time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)),
	}
	policy := RetentionPolicy{KeepMonthly: 3}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["m4"]; !ok {
		t.Error("expected m4 (June) to be kept")
	}
	if _, ok := keep["m3"]; !ok {
		t.Error("expected m3 (May) to be kept")
	}
	if _, ok := keep["m2"]; !ok {
		t.Error("expected m2 (April) to be kept")
	}
	if _, ok := keep["m1"]; ok {
		t.Error("expected m1 (March) to be pruned")
	}
}

func TestSelectBackupsToKeep_KeepYearly(t *testing.T) {
	backups := []BackupMetadata{
		makeBackup("y1", time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)),
		makeBackup("y2", time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)),
		makeBackup("y3", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
	}
	policy := RetentionPolicy{KeepYearly: 2}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["y3"]; !ok {
		t.Error("expected y3 (2026) to be kept")
	}
	if _, ok := keep["y2"]; !ok {
		t.Error("expected y2 (2025) to be kept")
	}
	if _, ok := keep["y1"]; ok {
		t.Error("expected y1 (2024) to be pruned")
	}
}

func TestSelectBackupsToKeep_CombinedPolicy(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	var backups []BackupMetadata
	for i := 0; i < 30; i++ {
		ts := now.AddDate(0, 0, -i)
		backups = append(backups, makeBackup("d"+ts.Format("0102"), ts))
	}
	policy := RetentionPolicy{
		KeepLast:   3,
		KeepDaily:  7,
		KeepWeekly: 4,
	}
	keep := selectBackupsToKeep(backups, policy)
	if _, ok := keep["d0630"]; !ok {
		t.Error("expected d0630 to be kept (keepLast)")
	}
	if _, ok := keep["d0629"]; !ok {
		t.Error("expected d0629 to be kept (keepLast)")
	}
	if _, ok := keep["d0628"]; !ok {
		t.Error("expected d0628 to be kept (keepLast)")
	}
	if len(keep) > 14 {
		t.Errorf("expected at most ~14 kept (3+7+4), got %d", len(keep))
	}
	if len(keep) < 7 {
		t.Errorf("expected at least 7 kept, got %d", len(keep))
	}
}

func TestSelectBackupsToKeep_EmptyBackups(t *testing.T) {
	policy := RetentionPolicy{KeepLast: 5, KeepDaily: 7}
	keep := selectBackupsToKeep(nil, policy)
	if len(keep) != 0 {
		t.Errorf("expected 0 kept for empty backups, got %d", len(keep))
	}
}

func TestSelectBackupsToKeep_ZeroPolicy(t *testing.T) {
	backups := []BackupMetadata{
		makeBackup("b1", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
		makeBackup("b2", time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)),
	}
	policy := RetentionPolicy{}
	keep := selectBackupsToKeep(backups, policy)
	if len(keep) != 0 {
		t.Errorf("expected 0 kept for zero policy, got %d", len(keep))
	}
}

func TestSelectBackupsToKeep_KeepLastExceedsTotal(t *testing.T) {
	backups := []BackupMetadata{
		makeBackup("b1", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
		makeBackup("b2", time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)),
	}
	policy := RetentionPolicy{KeepLast: 10}
	keep := selectBackupsToKeep(backups, policy)
	if len(keep) != 2 {
		t.Errorf("expected 2 kept, got %d", len(keep))
	}
}
