package com.backuparr.algebras

import cats.effect.Async
import com.backuparr.config.ArrInstanceConfig
import com.backuparr.domain.{BackupResult, BackupStatus}

/**
 * Algebra for backup orchestration.
 * 
 * The BackupManager coordinates the entire backup lifecycle:
 * 1. Request backup from *arr instance (via ArrClient)
 * 2. Poll for completion
 * 3. Download the backup file
 * 4. Upload to S3 (via S3Client)
 * 5. Apply retention policy (via RetentionManager)
 * 6. Clean up temporary files
 * 
 * This is a higher-level algebra that composes the lower-level
 * ArrClient, S3Client, and RetentionManager algebras.
 */
trait BackupManager[F[_]]:
  /**
   * Execute a complete backup for an *arr instance.
   * 
   * This is the main operation that orchestrates all steps of the backup process.
   * It's a long-running operation that may take several minutes.
   * 
   * @param instance Configuration for the instance to backup
   * @return Result of the backup operation (success or failure)
   * 
   * Example:
   * {{{
   * val manager: BackupManager[IO] = ???
   * val config: ArrInstanceConfig = ???
   * 
   * val backup: IO[BackupResult] = for
   *   result <- manager.executeBackup(config)
   *   _ <- result.status match
   *     case BackupStatus.Completed(uri, _, _) =>
   *       IO.println(s"Backup succeeded: \$uri")
   *     case BackupStatus.Failed(error, _) =>
   *       IO.println(s"Backup failed: \${error.message}")
   *     case _ =>
   *       IO.println(s"Unexpected status: \${result.status}")
   * yield result
   * }}}
   */
  def executeBackup(instance: ArrInstanceConfig): F[BackupResult]
  
  /**
   * Get the current status of all running backups.
   * 
   * Returns a map of instance name to current backup status.
   * Useful for monitoring and health checks.
   * 
   * @return Map of instance names to their current backup status
   */
  def getStatus: F[Map[String, BackupStatus]]

object BackupManager:
  /**
   * Create a BackupManager instance.
   * 
   * This will compose:
   * - ArrClient for *arr API operations
   * - S3Client for S3 operations
   * - RetentionManager for applying retention policies
   * - FileManager for temporary file handling
   * 
   * The implementation will:
   * - Handle errors at each step
   * - Retry transient failures
   * - Clean up resources on failure
   * - Track backup state in a Ref
   * - Use structured concurrency for timeouts
   */
  def make[F[_]: Async](
    arrClient: ArrClient[F],
    s3Client: S3Client[F],
    retentionManager: RetentionManager[F]
  ): F[BackupManager[F]] = ???
