package com.backuparr.algebras

import cats.effect.Async
import com.backuparr.config.S3BucketConfig
import com.backuparr.domain.{S3Object, S3Uri}
import java.nio.file.Path

/**
 * Algebra for S3 operations.
 * 
 * This trait defines the contract for interacting with S3-compatible storage.
 * It abstracts over different S3 providers (AWS, MinIO, Backblaze B2, etc.).
 * 
 * All operations return F[A] because they perform side effects:
 * - Network I/O (uploading/downloading/listing)
 * - Authentication
 * - Potential retries on failure
 */
trait S3Client[F[_]]:
  /**
   * Upload a file to S3.
   * 
   * Streams the file to S3 to avoid loading it entirely into memory.
   * For large files (>5GB), this should use multipart upload.
   * 
   * @param bucket S3 bucket configuration
   * @param key S3 key (path within bucket)
   * @param source Local file path to upload
   * @param metadata Custom metadata to attach to the object
   * @return S3 URI of the uploaded file
   * 
   * Example:
   * {{{
   * val client: S3Client[IO] = ???
   * val bucket: S3BucketConfig = ???
   * 
   * val upload: IO[S3Uri] = client.uploadFile(
   *   bucket = bucket,
   *   key = "backups/sonarr/2025/11/23/backup.zip",
   *   source = Paths.get("/tmp/backup.zip"),
   *   metadata = Map(
   *     "instance-name" -> "sonarr-main",
   *     "backup-date" -> "2025-11-23T02:00:00Z"
   *   )
   * )
   * }}}
   */
  def uploadFile(
    bucket: S3BucketConfig,
    key: String,
    source: Path,
    metadata: Map[String, String] = Map.empty
  ): F[S3Uri]
  
  /**
   * List objects in an S3 bucket with a given prefix.
   * 
   * Used to discover existing backups for retention policy application.
   * 
   * @param bucket S3 bucket configuration
   * @param prefix Key prefix to filter by (e.g., "backups/sonarr/")
   * @return List of S3 objects matching the prefix
   * 
   * Note: For buckets with many objects, this should be paginated.
   * The current implementation assumes a reasonable number of backups.
   */
  def listObjects(bucket: S3BucketConfig, prefix: String): F[List[S3Object]]
  
  /**
   * Delete an object from S3.
   * 
   * Used by the retention manager to remove old backups.
   * 
   * @param bucket S3 bucket configuration
   * @param key S3 key to delete
   * @return Unit (success) or raises an error
   */
  def deleteObject(bucket: S3BucketConfig, key: String): F[Unit]
  
  /**
   * Check if an object exists in S3.
   * 
   * @param bucket S3 bucket configuration
   * @param key S3 key to check
   * @return true if the object exists, false otherwise
   */
  def objectExists(bucket: S3BucketConfig, key: String): F[Boolean]
  
  /**
   * Get metadata for an S3 object without downloading it.
   * 
   * @param bucket S3 bucket configuration
   * @param key S3 key
   * @return Object metadata
   */
  def getObjectMetadata(bucket: S3BucketConfig, key: String): F[S3Object]

object S3Client:
  /**
   * Create an S3Client instance.
   * 
   * This will be implemented with http4s for direct S3 API calls,
   * giving us full control and avoiding heavyweight AWS SDK dependencies.
   * 
   * The implementation will:
   * - Support multiple S3 providers (AWS, MinIO, Backblaze)
   * - Handle authentication (AWS Signature V4)
   * - Implement retry logic with exponential backoff
   * - Stream large files
   * - Support multipart uploads for files >5GB
   */
  def make[F[_]: Async]: F[S3Client[F]] = ???
