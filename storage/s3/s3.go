package s3

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"backuparr/storage"
)

// Ensure S3Backend implements storage.Backend at compile time.
var _ storage.Backend = (*S3Backend)(nil)

// Config holds the configuration for an S3-compatible storage backend.
type Config struct {
	Bucket         string
	Prefix         string // object key prefix, defaults to "backuparr"
	Region         string
	Endpoint       string // custom endpoint for MinIO/R2/B2/Wasabi
	AccessKeyID    string // optional — falls back to AWS credential chain
	SecretAccessKey string
	StorageClass   string // e.g. "STANDARD", "STANDARD_IA", "DEEP_ARCHIVE"
	ForcePathStyle bool   // required for MinIO and some S3-compatible stores
}

// S3Backend stores backups in an S3-compatible object store.
type S3Backend struct {
	client       *s3.Client
	bucket       string
	prefix       string
	storageClass s3types.StorageClass
}

// New creates a new S3 storage backend from the given config.
func New(ctx context.Context, cfg Config) (*S3Backend, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}

	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "backuparr"
	}
	// Ensure prefix doesn\'t have leading/trailing slashes
	prefix = strings.Trim(prefix, "/")

	// Build AWS config options
	var opts []func(*awsconfig.LoadOptions) error

	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: failed to load AWS config: %w", err)
	}

	// Build S3 client options
	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // Required for most S3-compatible stores
		})
	}
	if cfg.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	sc := s3types.StorageClassStandard
	if cfg.StorageClass != "" {
		sc = s3types.StorageClass(cfg.StorageClass)
	}

	return &S3Backend{
		client:       client,
		bucket:       cfg.Bucket,
		prefix:       prefix,
		storageClass: sc,
	}, nil
}

func (b *S3Backend) Name() string {
	return "s3"
}

// objectKey returns the full S3 object key for a backup file.
// Layout: <prefix>/<appName>/<fileName>
func (b *S3Backend) objectKey(appName, fileName string) string {
	return path.Join(b.prefix, appName, fileName)
}

// Upload stores backup data as an S3 object.
func (b *S3Backend) Upload(ctx context.Context, appName string, fileName string, data io.Reader, size int64) (*storage.BackupMetadata, error) {
	key := b.objectKey(appName, fileName)

	input := &s3.PutObjectInput{
		Bucket:       aws.String(b.bucket),
		Key:          aws.String(key),
		Body:         data,
		StorageClass: b.storageClass,
	}
	if size > 0 {
		input.ContentLength = aws.Int64(size)
	}

	_, err := b.client.PutObject(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("s3: failed to upload %s: %w", key, err)
	}

	return &storage.BackupMetadata{
		Key:      key,
		AppName:  appName,
		FileName: fileName,
		Size:     size,
	}, nil
}

// Download retrieves a backup object from S3. Caller must close the reader.
func (b *S3Backend) Download(ctx context.Context, key string) (io.ReadCloser, *storage.BackupMetadata, error) {
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("s3: failed to download %s: %w", key, err)
	}

	// Extract appName and fileName from the key
	// Key format: <prefix>/<appName>/<fileName>
	appName, fileName := parseKey(b.prefix, key)

	var size int64
	if output.ContentLength != nil {
		size = *output.ContentLength
	}

	meta := &storage.BackupMetadata{
		Key:      key,
		AppName:  appName,
		FileName: fileName,
		Size:     size,
	}
	if output.LastModified != nil {
		meta.CreatedAt = *output.LastModified
	}

	return output.Body, meta, nil
}

// List returns all backups for the given app, sorted newest-first.
func (b *S3Backend) List(ctx context.Context, appName string) ([]storage.BackupMetadata, error) {
	prefix := path.Join(b.prefix, appName) + "/"

	var backups []storage.BackupMetadata
	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: failed to list objects with prefix %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			_, fileName := parseKey(b.prefix, *obj.Key)
			// Only include .zip files
			if !strings.HasSuffix(fileName, ".zip") {
				continue
			}
			meta := storage.BackupMetadata{
				Key:      *obj.Key,
				AppName:  appName,
				FileName: fileName,
			}
			if obj.Size != nil {
				meta.Size = *obj.Size
			}
			if obj.LastModified != nil {
				meta.CreatedAt = *obj.LastModified
			}
			backups = append(backups, meta)
		}
	}

	// Sort newest-first
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// Delete removes a backup object from S3.
func (b *S3Backend) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3: failed to delete %s: %w", key, err)
	}
	return nil
}

// parseKey extracts appName and fileName from an S3 object key.
// Expected format: <prefix>/<appName>/<fileName>
func parseKey(prefix, key string) (appName, fileName string) {
	// Strip prefix
	rel := strings.TrimPrefix(key, prefix+"/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", rel
}
