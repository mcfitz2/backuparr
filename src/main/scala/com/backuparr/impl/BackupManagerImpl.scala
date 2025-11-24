package com.backuparr.impl

import cats.effect.{Async, Ref, Resource, Temporal}
import cats.syntax.all.*
import com.backuparr.algebras.{ArrClient, BackupManager, RetentionManager, S3Client}
import com.backuparr.config.{ArrInstanceConfig, S3BucketConfig}
import com.backuparr.domain.*
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger

import java.nio.file.{Files as JFiles, Path}
import java.time.Instant
import scala.concurrent.duration.*

/**
 * Implementation of BackupManager that orchestrates the complete backup workflow.
 * 
 * This implementation:
 * - Coordinates ArrClient for backup creation and download
 * - Uses S3Client for uploading backups to storage
 * - Applies retention policies via RetentionManager
 * - Manages temporary files safely with Resource
 * - Tracks backup state in a concurrent Ref
 * - Provides comprehensive logging
 * 
 * The backup workflow:
 * 1. Create temporary directory for this backup
 * 2. Request backup from *arr application
 * 3. Poll for backup completion (with timeout)
 * 4. Download backup file to temp directory
 * 5. Upload file to S3
 * 6. Apply retention policy to clean old backups
 * 7. Clean up temporary directory
 * 
 * All steps are wrapped in Resource for automatic cleanup on failure.
 * 
 * @param arrClient Client for *arr API operations
 * @param s3Client Client for S3 operations
 * @param retentionManager Manager for applying retention policies
 * @param s3BucketConfigs Map from bucket name to S3BucketConfig
 * @param statusRef Concurrent state for tracking backup statuses
 */
class BackupManagerImpl[F[_]: Async](
  arrClient: ArrClient[F],
  s3Client: S3Client[F],
  retentionManager: RetentionManager[F],
  s3BucketConfigs: Map[String, S3BucketConfig],
  statusRef: Ref[F, Map[String, BackupStatus]]
) extends BackupManager[F]:
  
  private val logger: Logger[F] = Slf4jLogger.getLogger[F]
  
  /**
   * Execute a complete backup for an *arr instance.
   * 
   * This orchestrates the entire workflow and ensures cleanup happens
   * even if any step fails.
   */
  override def executeBackup(instance: ArrInstanceConfig): F[BackupResult] =
    val startTime = Instant.now()
    
    for
      _ <- logger.info(s"Starting backup for ${instance.name} (${instance.arrType})")
      _ <- updateStatus(instance.name, BackupStatus.Requesting)
      
      // Execute the backup workflow
      result <- runBackupWorkflow(instance, startTime).attempt
      
      // Convert result to BackupResult
      backupResult <- result match
        case Right(s3Uri) =>
          val endTime = Instant.now()
          val status = BackupStatus.Completed(s3Uri, endTime, 0L) // TODO: Track actual size
          for
            _ <- updateStatus(instance.name, status)
            _ <- logger.info(s"Backup completed for ${instance.name}: $s3Uri")
          yield BackupResult(
            instanceName = instance.name,
            status = status,
            startTime = startTime,
            endTime = endTime,
            backupSize = Some(0L), // TODO: Track actual size
            s3Uri = Some(s3Uri)
          )
        
        case Left(error) =>
          val endTime = Instant.now()
          val backupError = BackupError.ArrApiError(s"Backup workflow failed: ${error.getMessage}", Some(error))
          val status = BackupStatus.Failed(backupError, endTime)
          for
            _ <- updateStatus(instance.name, status)
            _ <- logger.error(error)(s"Backup failed for ${instance.name}")
          yield BackupResult(
            instanceName = instance.name,
            status = status,
            startTime = startTime,
            endTime = endTime,
            backupSize = None,
            s3Uri = None
          )
    yield backupResult
  
  /**
   * Run the complete backup workflow with resource management.
   * 
   * Uses Resource to ensure temporary directory is cleaned up
   * even if any step fails.
   */
  private def runBackupWorkflow(
    instance: ArrInstanceConfig,
    startTime: Instant
  ): F[S3Uri] =
    // Create temporary directory for this backup
    val tempDirResource: Resource[F, Path] = Resource.make(
      Async[F].delay {
        val tempDir = JFiles.createTempDirectory(s"backuparr-${instance.name}-")
        logger.debug(s"Created temp directory: $tempDir")
        tempDir
      }
    )(tempDir =>
      Async[F].delay {
        // Clean up temp directory and all contents
        if JFiles.exists(tempDir) then
          JFiles.walk(tempDir)
            .sorted(java.util.Comparator.reverseOrder())
            .forEach(path => JFiles.deleteIfExists(path))
          logger.debug(s"Cleaned up temp directory: $tempDir")
      }.handleErrorWith(err =>
        logger.warn(err)(s"Failed to clean up temp directory: $tempDir")
      )
    )
    
    tempDirResource.use { tempDir =>
      for
        // Step 1: Request backup from *arr application
        _ <- logger.info(s"Requesting backup from ${instance.name}")
        backupId <- arrClient.requestBackup(instance)
        _ <- logger.debug(s"Backup requested: $backupId")
        
        // Step 2: Give the *arr application a moment to queue the backup
        // Some apps (especially Lidarr/Prowlarr) need a brief delay before the backup appears
        _ <- Temporal[F].sleep(1.second)
        
        // Step 3: Poll for backup completion (arrClient handles this internally with retry)
        _ <- logger.info(s"Waiting for backup to complete...")
        _ <- updateStatus(instance.name, BackupStatus.Downloading)
        
        // Step 4: Download backup to temp directory (includes retry logic)
        backupFile <- arrClient.downloadBackup(instance, backupId, tempDir)
        _ <- logger.info(s"Downloaded backup to ${backupFile.getFileName}")
        
        // Step 4: Generate S3 key based on timestamp and instance name
        s3Key = generateS3Key(instance, startTime)
        
        // Step 5: Look up S3 bucket config
        bucketConfig <- Async[F].fromOption(
          s3BucketConfigs.get(instance.s3BucketName),
          new RuntimeException(s"S3 bucket config not found: ${instance.s3BucketName}")
        )
        
        // Step 6: Upload to S3
        _ <- logger.info(s"Uploading backup to S3: $s3Key")
        _ <- updateStatus(instance.name, BackupStatus.Uploading)
        s3Uri <- s3Client.uploadFile(
          bucket = bucketConfig,
          key = s3Key,
          source = backupFile,
          metadata = Map(
            "instance-name" -> instance.name,
            "arr-type" -> instance.arrType.toString,
            "backup-date" -> startTime.toString,
            "backup-id" -> backupId.value
          )
        )
        _ <- logger.info(s"Uploaded to S3: $s3Uri")
        
        // Step 7: Apply retention policy
        _ <- logger.info(s"Applying retention policy for ${instance.name}")
        _ <- updateStatus(instance.name, BackupStatus.ApplyingRetention)
        result <- retentionManager.applyRetention(
          bucketConfig,
          instance.retentionPolicy,
          instance.name
        )
        _ <- logger.info(s"Retention applied: kept ${result.kept.size}, deleted ${result.deleted.size}")
        
      yield s3Uri
    }
  
  /**
   * Generate an S3 key for the backup file.
   * 
   * Format: {instanceName}/backup-{timestamp}.zip
   * Example: sonarr-prod/backup-2025-11-23T15-30-00Z.zip
   */
  private def generateS3Key(instance: ArrInstanceConfig, timestamp: Instant): String =
    val timestampStr = timestamp.toString.replace(":", "-")
    s"${instance.name}/backup-$timestampStr.zip"
  
  /**
   * Update the status map for an instance.
   */
  private def updateStatus(instanceName: String, status: BackupStatus): F[Unit] =
    statusRef.update(_.updated(instanceName, status))
  
  /**
   * Get the current status of all running backups.
   */
  override def getStatus: F[Map[String, BackupStatus]] =
    statusRef.get

object BackupManagerImpl:
  /**
   * Create a BackupManagerImpl instance.
   * 
   * Initializes the status tracking Ref and returns a configured manager.
   * 
   * @param s3BucketConfigs Map from bucket name to S3BucketConfig
   */
  def make[F[_]: Async](
    arrClient: ArrClient[F],
    s3Client: S3Client[F],
    retentionManager: RetentionManager[F],
    s3BucketConfigs: Map[String, S3BucketConfig]
  ): F[BackupManager[F]] =
    for
      statusRef <- Ref.of[F, Map[String, BackupStatus]](Map.empty)
    yield new BackupManagerImpl[F](arrClient, s3Client, retentionManager, s3BucketConfigs, statusRef)
