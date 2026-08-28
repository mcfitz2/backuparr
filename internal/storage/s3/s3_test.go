package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"backuparr/internal/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	testBucket   = "backuparr-test"
	testEndpoint = "http://localhost:9000"
	testAccess   = "minioadmin"
	testSecret   = "minioadmin"
	testRegion   = "us-east-1"
)

func skipUnlessS3(t *testing.T) {
	t.Helper()
	if os.Getenv("S3_TEST") == "" {
		t.Skip("S3_TEST not set, skipping S3 integration tests")
	}
}

// createTestBucket creates the test bucket if it doesn't exist.
func createTestBucket(t *testing.T, ctx context.Context) {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(testRegion),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccess, testSecret, ""),
		),
	)
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(testEndpoint)
		o.UsePathStyle = true
	})

	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(testBucket),
	})
	if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
		t.Fatalf("failed to create test bucket: %v", err)
	}
}

func newTestBackend(t *testing.T, ctx context.Context) *S3Backend {
	t.Helper()
	backend, err := New(ctx, Config{
		Bucket:          testBucket,
		Prefix:          fmt.Sprintf("test-%d", time.Now().UnixNano()),
		Region:          testRegion,
		Endpoint:        testEndpoint,
		AccessKeyID:     testAccess,
		SecretAccessKey: testSecret,
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("failed to create S3 backend: %v", err)
	}
	return backend
}

func TestS3Backend_Type(t *testing.T) {
	skipUnlessS3(t)
	ctx := context.Background()
	createTestBucket(t, ctx)
	backend := newTestBackend(t, ctx)
	if backend.Type() != "s3" {
		t.Errorf("Type() = %q, want %q", backend.Type(), "s3")
	}
	if backend.Name() != "s3" {
		t.Errorf("Name() = %q, want %q (should default to type)", backend.Name(), "s3")
	}
	backend.SetName("offsite")
	if backend.Name() != "offsite" {
		t.Errorf("Name() after SetName = %q, want %q", backend.Name(), "offsite")
	}
}

func TestS3Backend_UploadAndDownload(t *testing.T) {
	skipUnlessS3(t)
	ctx := context.Background()
	createTestBucket(t, ctx)
	backend := newTestBackend(t, ctx)

	data := []byte("hello backup world")
	fileName := "sonarr_2026-02-06T120000Z.zip"

	// Upload
	meta, err := backend.Upload(ctx, "sonarr", fileName, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if meta.FileName != fileName {
		t.Errorf("Upload meta.FileName = %q, want %q", meta.FileName, fileName)
	}
	if meta.AppName != "sonarr" {
		t.Errorf("Upload meta.AppName = %q, want %q", meta.AppName, "sonarr")
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("Upload meta.Size = %d, want %d", meta.Size, len(data))
	}

	// Download
	reader, dlMeta, err := backend.Download(ctx, meta.Key)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	defer reader.Close()

	downloaded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(downloaded, data) {
		t.Errorf("Downloaded data = %q, want %q", downloaded, data)
	}
	if dlMeta.FileName != fileName {
		t.Errorf("Download meta.FileName = %q, want %q", dlMeta.FileName, fileName)
	}
	if dlMeta.AppName != "sonarr" {
		t.Errorf("Download meta.AppName = %q, want %q", dlMeta.AppName, "sonarr")
	}
}

func TestS3Backend_List(t *testing.T) {
	skipUnlessS3(t)
	ctx := context.Background()
	createTestBucket(t, ctx)
	backend := newTestBackend(t, ctx)

	// Upload 3 backups with small delays to ensure distinct timestamps
	files := []string{
		"sonarr_2026-02-04T120000Z.zip",
		"sonarr_2026-02-05T120000Z.zip",
		"sonarr_2026-02-06T120000Z.zip",
	}
	for _, f := range files {
		_, err := backend.Upload(ctx, "sonarr", f, bytes.NewReader([]byte("data-"+f)), 5+int64(len(f)))
		if err != nil {
			t.Fatalf("Upload %s failed: %v", f, err)
		}
	}

	// Also upload a radarr backup to verify isolation
	_, err := backend.Upload(ctx, "radarr", "radarr_2026-02-06T120000Z.zip", bytes.NewReader([]byte("radarr")), 6)
	if err != nil {
		t.Fatalf("Upload radarr failed: %v", err)
	}

	// List sonarr backups
	backups, err := backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("List returned %d backups, want 3", len(backups))
	}

	// Verify all are sonarr
	for _, b := range backups {
		if b.AppName != "sonarr" {
			t.Errorf("backup.AppName = %q, want %q", b.AppName, "sonarr")
		}
		if !strings.HasSuffix(b.FileName, ".zip") {
			t.Errorf("backup.FileName = %q, want .zip suffix", b.FileName)
		}
	}

	// List radarr backups
	radarrBackups, err := backend.List(ctx, "radarr")
	if err != nil {
		t.Fatalf("List radarr failed: %v", err)
	}
	if len(radarrBackups) != 1 {
		t.Fatalf("List radarr returned %d backups, want 1", len(radarrBackups))
	}
}

func TestS3Backend_Delete(t *testing.T) {
	skipUnlessS3(t)
	ctx := context.Background()
	createTestBucket(t, ctx)
	backend := newTestBackend(t, ctx)

	// Upload
	data := []byte("delete me")
	meta, err := backend.Upload(ctx, "sonarr", "sonarr_2026-02-06T120000Z.zip", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// Verify it exists
	backups, err := backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}

	// Delete
	if err := backend.Delete(ctx, meta.Key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify it's gone
	backups, err = backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List after delete failed: %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("expected 0 backups after delete, got %d", len(backups))
	}
}

func TestS3Backend_Retention(t *testing.T) {
	skipUnlessS3(t)
	ctx := context.Background()
	createTestBucket(t, ctx)
	backend := newTestBackend(t, ctx)

	// Upload 5 backups
	for i := 0; i < 5; i++ {
		ts := time.Date(2026, 2, 1+i, 12, 0, 0, 0, time.UTC)
		fileName := storage.FormatBackupName("sonarr", ts)
		_, err := backend.Upload(ctx, "sonarr", fileName, bytes.NewReader([]byte(fmt.Sprintf("backup-%d", i))), 8)
		if err != nil {
			t.Fatalf("Upload %d failed: %v", i, err)
		}
	}

	// Verify 5 exist
	backups, err := backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(backups) != 5 {
		t.Fatalf("expected 5 backups, got %d", len(backups))
	}

	// Apply retention: keep last 2
	policy := storage.RetentionPolicy{KeepLast: 2}
	deleted, err := storage.ApplyRetention(ctx, backend, "sonarr", policy)
	if err != nil {
		t.Fatalf("ApplyRetention failed: %v", err)
	}
	if deleted != 3 {
		t.Errorf("ApplyRetention deleted %d, want 3", deleted)
	}

	// Verify 2 remain
	backups, err = backend.List(ctx, "sonarr")
	if err != nil {
		t.Fatalf("List after retention failed: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("expected 2 backups after retention, got %d", len(backups))
	}
}

func TestS3Backend_EmptyBucket(t *testing.T) {
	skipUnlessS3(t)
	ctx := context.Background()
	createTestBucket(t, ctx)
	backend := newTestBackend(t, ctx)

	// List from empty prefix should return empty
	backups, err := backend.List(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("expected 0 backups from empty prefix, got %d", len(backups))
	}
}

func TestS3Backend_ConfigValidation(t *testing.T) {
	ctx := context.Background()
	_, err := New(ctx, Config{
		// Missing required bucket
		Region:          testRegion,
		AccessKeyID:     testAccess,
		SecretAccessKey: testSecret,
	})
	if err == nil {
		t.Error("expected error for missing bucket, got nil")
	}
}

func TestParseKey(t *testing.T) {
	tests := []struct {
		prefix   string
		key      string
		wantApp  string
		wantFile string
	}{
		{"backuparr", "backuparr/sonarr/sonarr_2026-02-06T120000Z.zip", "sonarr", "sonarr_2026-02-06T120000Z.zip"},
		{"prefix", "prefix/radarr/backup.zip", "radarr", "backup.zip"},
		{"a/b", "a/b/sonarr/file.zip", "sonarr", "file.zip"},
		{"backuparr", "backuparr/onlyone", "", "onlyone"},
	}
	for _, tt := range tests {
		app, file := parseKey(tt.prefix, tt.key)
		if app != tt.wantApp || file != tt.wantFile {
			t.Errorf("parseKey(%q, %q) = (%q, %q), want (%q, %q)",
				tt.prefix, tt.key, app, file, tt.wantApp, tt.wantFile)
		}
	}
}

// --- manager.Uploader swap, without MinIO or live S3 -----------------------
//
// Upload() now goes through manager.Uploader instead of a bare PutObject
// call. These tests point the S3 client at an in-process httptest server
// that speaks just enough of the S3 REST API to exercise that path — no
// Docker/MinIO required, unlike the tests above.
//
// TestS3Backend_Upload_Multipart is the closest thing here to proving the
// multipart mechanism works, but it only proves the SDK's manager package
// drives CreateMultipartUpload/UploadPart/CompleteMultipartUpload correctly
// against a fake server that always succeeds; it says nothing about behavior
// against real S3 semantics (e.g. actual minimum part size enforcement,
// retries on part failure, or true concurrent network transfer). That would
// need MinIO (make test-s3) or live S3.

func newFakeS3Backend(t *testing.T, handler http.HandlerFunc) *S3Backend {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	backend, err := New(context.Background(), Config{
		Bucket:          "fake-bucket",
		Prefix:          "backuparr",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("failed to create backend: %v", err)
	}
	return backend
}

func TestS3Backend_Upload_SinglePart(t *testing.T) {
	var gotMethod, gotPath string
	backend := newFakeS3Backend(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		// Drain the body: an unread large request body left when the handler
		// returns makes the OS close the connection with a RST on Linux.
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"single-part-etag"`)
		w.WriteHeader(http.StatusOK)
	})

	data := []byte("small backup payload")
	fileName := "sonarr_2026-02-06T120000Z.zip"
	meta, err := backend.Upload(context.Background(), "sonarr", fileName, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("a payload under the default part size should upload via a single PUT, got method %q", gotMethod)
	}
	if !strings.Contains(gotPath, fileName) {
		t.Errorf("request path = %q, want it to contain %q", gotPath, fileName)
	}
	if meta.Key != "backuparr/sonarr/"+fileName {
		t.Errorf("meta.Key = %q, want %q", meta.Key, "backuparr/sonarr/"+fileName)
	}
	if meta.AppName != "sonarr" || meta.FileName != fileName {
		t.Errorf("meta = %+v, unexpected AppName/FileName", meta)
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("meta.Size = %d, want %d", meta.Size, len(data))
	}
}

func TestS3Backend_Upload_Multipart(t *testing.T) {
	const uploadID = "test-upload-id"

	var (
		mu        sync.Mutex
		partsSeen = map[string]bool{}
		completed bool
	)

	backend := newFakeS3Backend(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult><Bucket>fake-bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
				r.URL.Path, uploadID)

		case r.Method == http.MethodPut && q.Get("partNumber") != "":
			mu.Lock()
			partsSeen[q.Get("partNumber")] = true
			mu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf(`"part-%s-etag"`, q.Get("partNumber")))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && q.Get("uploadId") != "":
			mu.Lock()
			completed = true
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult><Location>http://fake/%s</Location><Bucket>fake-bucket</Bucket><Key>%s</Key><ETag>&quot;final-etag&quot;</ETag></CompleteMultipartUploadResult>`,
				r.URL.Path, r.URL.Path)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	// One byte over the SDK's default 5MiB part size forces two parts, so the
	// upload can only succeed if CreateMultipartUpload/UploadPart/Complete all
	// went through manager.Uploader correctly. bytes.NewReader supports
	// io.ReaderAt, so the SDK can size this up front without buffering.
	data := bytes.Repeat([]byte{0xAB}, 5*1024*1024+1)
	fileName := "sonarr_2026-02-06T120000Z.zip"
	meta, err := backend.Upload(context.Background(), "sonarr", fileName, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(partsSeen) != 2 {
		t.Errorf("saw %d part(s) uploaded, want 2: %v", len(partsSeen), partsSeen)
	}
	if !completed {
		t.Error("CompleteMultipartUpload was never called")
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("meta.Size = %d, want %d", meta.Size, len(data))
	}
	if meta.AppName != "sonarr" || meta.FileName != fileName {
		t.Errorf("meta = %+v, unexpected AppName/FileName", meta)
	}
}
