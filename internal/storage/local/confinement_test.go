package local

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// dotdotKey builds a key rooted at <base>/sonarr that reaches target via a
// literal "../" traversal, regardless of how base and target are nested on
// disk. It intentionally avoids filepath.Join for the final concatenation,
// which would clean away the ".." segments before the backend ever sees them.
func dotdotKey(t *testing.T, base, target string) string {
	t.Helper()
	sonarrDir := filepath.Join(base, "sonarr")
	rel, err := filepath.Rel(sonarrDir, target)
	if err != nil {
		t.Fatalf("failed to compute traversal path: %v", err)
	}
	return sonarrDir + string(filepath.Separator) + rel
}

// TestConfinement_DownloadRejectsEscapingKeys verifies that Download refuses
// any key that does not resolve inside basePath: "../" traversal, absolute
// paths outside basePath, and symlinks inside basePath that point outside it.
func TestConfinement_DownloadRejectsEscapingKeys(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	backend := New(base)

	// A well-formed backup that must remain downloadable.
	if _, err := backend.Upload(ctx, "sonarr", "sonarr_2026-01-01T000000Z.zip", bytes.NewReader([]byte("good")), 4); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// Secret file outside basePath, sibling directory.
	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("secret"), 0644); err != nil {
		t.Fatalf("failed to write secret file: %v", err)
	}

	// Symlink inside basePath's sonarr dir pointing outside basePath.
	linkPath := filepath.Join(base, "sonarr", "escape.zip")
	if err := os.Symlink(secretPath, linkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	tests := []struct {
		name string
		key  string
	}{
		{"dotdot traversal", dotdotKey(t, base, secretPath)},
		{"absolute path outside base", secretPath},
		{"symlink escaping base", linkPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, _, err := backend.Download(ctx, tt.key)
			if err == nil {
				reader.Close()
				t.Fatalf("Download(%q) succeeded, want error", tt.key)
			}
		})
	}
}

// TestConfinement_DeleteRejectsEscapingKeys mirrors the Download cases for
// Delete, and additionally proves the escaping file was never touched.
func TestConfinement_DeleteRejectsEscapingKeys(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	backend := New(base)

	if _, err := backend.Upload(ctx, "sonarr", "sonarr_2026-01-01T000000Z.zip", bytes.NewReader([]byte("good")), 4); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// Each case gets its own outside secret file so that one subtest
	// mistakenly deleting it can't mask another (e.g. a naive
	// implementation "passing" the second case only because the first
	// already removed the shared target).
	newSecret := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "secret.txt")
		if err := os.WriteFile(p, []byte("secret"), 0644); err != nil {
			t.Fatalf("failed to write secret file: %v", err)
		}
		return p
	}

	t.Run("dotdot traversal", func(t *testing.T) {
		secretPath := newSecret(t)
		key := dotdotKey(t, base, secretPath)
		if err := backend.Delete(ctx, key); err == nil {
			t.Fatalf("Delete(%q) succeeded, want error", key)
		}
		if _, err := os.Stat(secretPath); err != nil {
			t.Fatalf("secret file was affected by rejected Delete: %v", err)
		}
	})

	t.Run("absolute path outside base", func(t *testing.T) {
		secretPath := newSecret(t)
		if err := backend.Delete(ctx, secretPath); err == nil {
			t.Fatalf("Delete(%q) succeeded, want error", secretPath)
		}
		if _, err := os.Stat(secretPath); err != nil {
			t.Fatalf("secret file was affected by rejected Delete: %v", err)
		}
	})

	t.Run("symlink escaping base", func(t *testing.T) {
		secretPath := newSecret(t)
		linkPath := filepath.Join(base, "sonarr", "escape.zip")
		if err := os.Symlink(secretPath, linkPath); err != nil {
			t.Fatalf("failed to create symlink: %v", err)
		}
		defer os.Remove(linkPath)

		if err := backend.Delete(ctx, linkPath); err == nil {
			t.Fatalf("Delete(%q) succeeded, want error", linkPath)
		}
		if _, err := os.Stat(secretPath); err != nil {
			t.Fatalf("secret file was affected by rejected Delete: %v", err)
		}
	})
}

// TestConfinement_WellFormedKeysStillWork proves the confinement check does
// not break normal Upload -> List -> Download/Delete usage.
func TestConfinement_WellFormedKeysStillWork(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	backend := New(base)

	data := []byte("hello backup world")
	meta, err := backend.Upload(ctx, "sonarr", "sonarr_2026-01-01T000000Z.zip", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	reader, dlMeta, err := backend.Download(ctx, meta.Key)
	if err != nil {
		t.Fatalf("Download(%q) failed: %v", meta.Key, err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Download data = %q, want %q", got, data)
	}
	if dlMeta.FileName != meta.FileName {
		t.Fatalf("Download meta.FileName = %q, want %q", dlMeta.FileName, meta.FileName)
	}

	if err := backend.Delete(ctx, meta.Key); err != nil {
		t.Fatalf("Delete(%q) failed: %v", meta.Key, err)
	}
	if _, err := os.Stat(meta.Key); !os.IsNotExist(err) {
		t.Fatalf("expected backup file to be removed, stat err = %v", err)
	}
}

// TestConfinement_ListThenActRoundTrip verifies that every key produced by
// List can be passed straight into Download and Delete, since that is the
// exact pattern used by retention and CLI callers.
func TestConfinement_ListThenActRoundTrip(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	backend := New(base)

	files := []string{
		"sonarr_2026-01-01T000000Z.zip",
		"sonarr_2026-01-02T000000Z.zip",
		"sonarr_2026-01-03T000000Z.zip",
	}
	for _, f := range files {
		if _, err := backend.Upload(ctx, "sonarr", f, bytes.NewReader([]byte("data-"+f)), int64(5+len(f))); err != nil {
			t.Fatalf("Upload %s failed: %v", f, err)
		}
	}

	backups, err := backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(backups) != len(files) {
		t.Fatalf("List returned %d backups, want %d", len(backups), len(files))
	}

	for _, b := range backups {
		reader, _, err := backend.Download(ctx, b.Key)
		if err != nil {
			t.Fatalf("Download(%q) from List result failed: %v", b.Key, err)
		}
		reader.Close()

		if err := backend.Delete(ctx, b.Key); err != nil {
			t.Fatalf("Delete(%q) from List result failed: %v", b.Key, err)
		}
	}

	remaining, err := backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List after deletes failed: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 backups remaining, got %d", len(remaining))
	}
}
