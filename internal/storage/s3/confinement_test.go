package s3

import (
	"context"
	"strings"
	"testing"
)

// TestConfinement_ConfineKeyRejectsOutOfPrefixKeys exercises confineKey
// directly against the key shapes Delete/Download/List must reject or
// accept, independent of any S3 API call.
func TestConfinement_ConfineKeyRejectsOutOfPrefixKeys(t *testing.T) {
	b := &S3Backend{bucket: "test-bucket", prefix: "backuparr"}

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"well-formed key under prefix", "backuparr/sonarr/sonarr_2026-01-01T000000Z.zip", false},
		{"different prefix entirely", "other-prefix/sonarr/file.zip", true},
		{"prefix as a bare string match with no separator", "backuparrEVIL/sonarr/file.zip", true},
		{"key equal to the bare prefix, no trailing slash", "backuparr", true},
		{"empty key", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := b.confineKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("confineKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
		})
	}
}

// TestConfinement_DeleteRejectsOutOfPrefixKey asserts that Delete rejects a
// key outside the configured prefix before making any API call. The backend
// is built with a nil client, so if confineKey did not run first, the call
// into the (nil) client would panic instead of returning a clean error.
func TestConfinement_DeleteRejectsOutOfPrefixKey(t *testing.T) {
	b := &S3Backend{bucket: "test-bucket", prefix: "backuparr"}

	err := b.Delete(context.Background(), "other-prefix/sonarr/file.zip")
	if err == nil {
		t.Fatal("Delete succeeded, want error")
	}
	if !strings.Contains(err.Error(), "outside configured prefix") {
		t.Fatalf("Delete error = %v, want mention of prefix confinement", err)
	}
}

// TestConfinement_DownloadRejectsOutOfPrefixKey mirrors the Delete case for
// Download.
func TestConfinement_DownloadRejectsOutOfPrefixKey(t *testing.T) {
	b := &S3Backend{bucket: "test-bucket", prefix: "backuparr"}

	reader, meta, err := b.Download(context.Background(), "other-prefix/sonarr/file.zip")
	if err == nil {
		t.Fatal("Download succeeded, want error")
	}
	if reader != nil || meta != nil {
		t.Fatalf("Download returned non-nil results alongside an error: reader=%v meta=%v", reader, meta)
	}
	if !strings.Contains(err.Error(), "outside configured prefix") {
		t.Fatalf("Download error = %v, want mention of prefix confinement", err)
	}
}

// TestConfinement_PrefixNormalization asserts that New normalizes every
// prefix shape that would otherwise trim/clean down to something confineKey
// can't accept — e.g. a prefix consisting only of slashes trims to "" and
// would make confineKey reject every key — so the resulting backend's own
// keys always pass its own confineKey check.
func TestConfinement_PrefixNormalization(t *testing.T) {
	ctx := context.Background()

	rawPrefixes := []string{
		"",      // empty -> default
		"/",     // slash only -> default
		"//",    // multiple slashes only -> default
		"///",   // same
		".",     // cleans to "." -> default
		"./",    // trims to "." -> default
		"a//b",  // internal double slash, collapsed by path.Join/Clean
		"/a/b/", // ordinary leading/trailing slashes
		"backuparr",
	}

	for _, raw := range rawPrefixes {
		t.Run("prefix="+raw, func(t *testing.T) {
			backend, err := New(ctx, Config{Bucket: "test-bucket", Prefix: raw})
			if err != nil {
				t.Fatalf("New(Prefix=%q) failed: %v", raw, err)
			}
			key := backend.objectKey("sonarr", "sonarr_2026-01-01T000000Z.zip")
			if err := backend.confineKey(key); err != nil {
				t.Errorf("confineKey(%q) = %v, want nil (backend.prefix=%q from raw config %q)", key, err, backend.prefix, raw)
			}
		})
	}
}

// TestConfinement_ListThenActKeysPassConfinement proves that every key shape
// List can produce (<prefix>/<appName>/<fileName>) satisfies confineKey, so
// legitimate List -> Download/Delete round trips are never rejected.
func TestConfinement_ListThenActKeysPassConfinement(t *testing.T) {
	b := &S3Backend{bucket: "test-bucket", prefix: "backuparr"}

	keys := []string{
		b.objectKey("sonarr", "sonarr_2026-01-01T000000Z.zip"),
		b.objectKey("radarr", "radarr_2026-02-06T120000Z.zip"),
	}
	for _, key := range keys {
		if err := b.confineKey(key); err != nil {
			t.Errorf("confineKey(%q) = %v, want nil (List-then-act must not be broken)", key, err)
		}
	}
}
