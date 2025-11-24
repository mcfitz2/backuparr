package com.backuparr.domain

import java.time.Instant
import io.circe.{Decoder, Encoder}

/**
 * Opaque type for backup IDs returned from *arr APIs.
 * 
 * Using an opaque type provides type safety - we can't accidentally
 * use a regular String where a BackupId is expected, and vice versa.
 * This is a Scala 3 feature that provides zero-cost abstraction.
 */
opaque type BackupId = String

object BackupId:
  /**
   * Create a BackupId from a String.
   * This is the only way to construct a BackupId from outside this object.
   */
  def apply(value: String): BackupId = value
  
  /**
   * Extension methods for BackupId.
   * These are only available when BackupId is in scope.
   */
  extension (id: BackupId)
    /** Get the underlying String value */
    def value: String = id

/**
 * Opaque type for S3 URIs.
 * Represents a fully qualified S3 location: s3://bucket/key
 */
opaque type S3Uri = String

object S3Uri:
  /**
   * Create an S3Uri from a String.
   * In a production system, you might want to validate the format here.
   */
  def apply(value: String): S3Uri = value
  
  extension (uri: S3Uri)
    def value: String = uri
    
    /** Extract bucket name from S3 URI */
    def bucket: Option[String] =
      if uri.startsWith("s3://") then
        uri.stripPrefix("s3://").split("/").headOption
      else None
    
    /** Extract key from S3 URI */
    def key: Option[String] =
      if uri.startsWith("s3://") then
        uri.stripPrefix("s3://").split("/", 2).drop(1).headOption
      else None

/**
 * Enumeration of supported *arr application types.
 * 
 * Each type has the same backup API, but we track the type
 * for logging and organization purposes.
 */
enum ArrType:
  case Sonarr
  case Radarr
  case Lidarr
  case Prowlarr
  case Readarr
  
object ArrType:
  /**
   * Parse an ArrType from a string (case-insensitive).
   * Returns None if the string doesn't match any known type.
   */
  def fromString(s: String): Option[ArrType] =
    s.toLowerCase match
      case "sonarr" => Some(Sonarr)
      case "radarr" => Some(Radarr)
      case "lidarr" => Some(Lidarr)
      case "prowlarr" => Some(Prowlarr)
      case "readarr" => Some(Readarr)
      case _ => None
  
  /**
   * Circe decoder for ArrType.
   * Allows parsing from YAML/JSON configuration files.
   */
  given Decoder[ArrType] = Decoder.decodeString.emap: str =>
    fromString(str).toRight(s"Invalid ArrType: $str")
  
  given Encoder[ArrType] = Encoder.encodeString.contramap(_.toString.toLowerCase)

/**
 * Status of a backup operation.
 * 
 * This ADT represents all possible states a backup can be in.
 * Using an ADT (sealed trait + case objects/classes) gives us:
 * 1. Exhaustive pattern matching (compiler warns if we miss a case)
 * 2. Type safety (can't create invalid states)
 * 3. Clear domain modeling
 */
enum BackupStatus:
  /** Backup is queued but not yet started */
  case Pending
  
  /** Backup request is being sent to *arr instance */
  case Requesting
  
  /** Backup file is being downloaded from *arr instance */
  case Downloading
  
  /** Backup file is being uploaded to S3 */
  case Uploading
  
  /** Retention policy is being applied */
  case ApplyingRetention
  
  /** Backup completed successfully */
  case Completed(uri: S3Uri, timestamp: Instant, backupSize: Long)
  
  /** Backup failed at some stage */
  case Failed(error: BackupError, timestamp: Instant)

/**
 * Errors that can occur during backup operations.
 * 
 * Each error type captures specific failure scenarios with relevant context.
 * This allows for specific error handling and better error reporting.
 */
enum BackupError:
  /**
   * Error communicating with *arr API.
   * @param message Human-readable error message
   * @param cause Optional underlying exception
   */
  case ArrApiError(message: String, cause: Option[Throwable] = None)
  
  /**
   * Error downloading backup file from *arr instance.
   */
  case DownloadError(message: String, cause: Option[Throwable] = None)
  
  /**
   * Error uploading backup to S3.
   */
  case S3UploadError(message: String, cause: Option[Throwable] = None)
  
  /**
   * Error applying retention policy.
   */
  case RetentionError(message: String, cause: Option[Throwable] = None)
  
  /**
   * Configuration validation error.
   */
  case ConfigurationError(message: String)
  
  /**
   * Operation timed out.
   * @param operation Name of the operation that timed out
   */
  case TimeoutError(operation: String)
  
  /**
   * File system error (e.g., can't write to temp directory).
   */
  case FileSystemError(message: String, cause: Option[Throwable] = None)

object BackupError:
  /**
   * Extension methods for BackupError to make them easier to work with.
   */
  extension (error: BackupError)
    /**
     * Get a human-readable message for this error.
     * Useful for logging and displaying to users.
     */
    def message: String = error match
      case ArrApiError(msg, _) => s"*arr API error: $msg"
      case DownloadError(msg, _) => s"Download error: $msg"
      case S3UploadError(msg, _) => s"S3 upload error: $msg"
      case RetentionError(msg, _) => s"Retention policy error: $msg"
      case ConfigurationError(msg) => s"Configuration error: $msg"
      case TimeoutError(op) => s"Operation timed out: $op"
      case FileSystemError(msg, _) => s"File system error: $msg"
    
    /**
     * Get the underlying cause if available.
     */
    def cause: Option[Throwable] = error match
      case ArrApiError(_, c) => c
      case DownloadError(_, c) => c
      case S3UploadError(_, c) => c
      case RetentionError(_, c) => c
      case FileSystemError(_, c) => c
      case _ => None

/**
 * Result of a backup operation.
 * 
 * This is returned by the BackupManager after a backup attempt.
 * It contains all relevant information about the backup.
 */
case class BackupResult(
  /** Name of the *arr instance that was backed up */
  instanceName: String,
  
  /** Final status of the backup */
  status: BackupStatus,
  
  /** When the backup started */
  startTime: Instant,
  
  /** When the backup completed (or failed) */
  endTime: Instant,
  
  /** Size of the backup file in bytes (if successful) */
  backupSize: Option[Long],
  
  /** S3 URI where the backup was stored (if successful) */
  s3Uri: Option[S3Uri]
):
  /**
   * Calculate the duration of the backup operation.
   */
  def duration: java.time.Duration =
    java.time.Duration.between(startTime, endTime)
  
  /**
   * Check if the backup was successful.
   */
  def isSuccessful: Boolean = status match
    case BackupStatus.Completed(_, _, _) => true
    case _ => false

/**
 * Metadata about an S3 object (backup file).
 * 
 * This is used when listing backups in S3 to apply retention policies.
 */
case class S3Object(
  /** S3 key (path within bucket) */
  key: String,
  
  /** Size in bytes */
  size: Long,
  
  /** Last modified timestamp */
  lastModified: Instant,
  
  /** Custom metadata (tags) */
  metadata: Map[String, String] = Map.empty
):
  /**
   * Get the instance name from metadata or key.
   * Useful for organizing backups by instance.
   */
  def instanceName: Option[String] =
    metadata.get("instance-name").orElse(
      // Try to extract from key pattern: backups/{instanceName}/...
      key.split("/").drop(1).headOption
    )

/**
 * Result of applying a retention policy.
 * 
 * Tracks which backups were kept and which were deleted.
 */
case class RetentionResult(
  /** Backups that were kept */
  kept: List[S3Object],
  
  /** Backups that were deleted */
  deleted: List[S3Object],
  
  /** Errors that occurred during retention application */
  errors: List[BackupError]
):
  /**
   * Total number of backups processed.
   */
  def totalProcessed: Int = kept.size + deleted.size
  
  /**
   * Check if any errors occurred.
   */
  def hasErrors: Boolean = errors.nonEmpty

/**
 * Enumeration of S3 providers.
 * 
 * Different providers may require different endpoint configurations.
 */
enum S3Provider:
  /** Amazon Web Services S3 */
  case AWS
  
  /** MinIO (self-hosted S3-compatible storage) */
  case MinIO
  
  /** Backblaze B2 (S3-compatible) */
  case Backblaze
  
  /** Generic S3-compatible service */
  case Generic

object S3Provider:
  /**
   * Parse an S3Provider from a string (case-insensitive).
   */
  def fromString(s: String): Option[S3Provider] =
    s.toLowerCase match
      case "aws" => Some(AWS)
      case "minio" => Some(MinIO)
      case "backblaze" => Some(Backblaze)
      case "generic" => Some(Generic)
      case _ => None
  
  /**
   * Get the default endpoint for a provider and region.
   * Returns None if the provider uses AWS default endpoints.
   */
  def defaultEndpoint(provider: S3Provider, region: String): Option[String] =
    provider match
      case AWS => None // Use AWS SDK defaults
      case MinIO => Some("http://minio:9000") // Common default, should be configurable
      case Backblaze => Some(s"https://s3.$region.backblazeb2.com")
      case Generic => None // Must be explicitly configured
  
  /**
   * Circe decoder for S3Provider.
   */
  given Decoder[S3Provider] = Decoder.decodeString.emap: str =>
    fromString(str).toRight(s"Invalid S3Provider: $str")
  
  given Encoder[S3Provider] = Encoder.encodeString.contramap(_.toString.toLowerCase)
