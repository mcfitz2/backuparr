package com.backuparr.algebras

import cats.effect.Async
import com.backuparr.config.ArrInstanceConfig
import com.backuparr.domain.{BackupId, BackupStatus}
import java.nio.file.Path

/**
 * Algebra for interacting with *arr application APIs.
 * 
 * This trait defines the contract for communicating with *arr instances
 * (Sonarr, Radarr, Lidarr, etc.) to trigger and download backups.
 * 
 * The [F[_]] type parameter represents the effect type:
 * - In production, F will be IO
 * - In tests, F might be a simpler type for easier testing
 * 
 * Methods return F[A] instead of just A because they perform side effects:
 * - HTTP requests (network I/O)
 * - File downloads (disk I/O)
 * These effects must be explicitly tracked in the type system.
 */
trait ArrClient[F[_]]:
  /**
   * Request a backup from the *arr instance.
   * 
   * This sends a POST request to the *arr API to trigger a backup.
   * The *arr instance will create a backup asynchronously.
   * 
   * @param instance Configuration for the *arr instance
   * @return BackupId that can be used to poll for completion
   * 
   * Example flow:
   * {{{
   * val client: ArrClient[IO] = ???
   * val config: ArrInstanceConfig = ???
   * 
   * val program: IO[BackupId] = for
   *   backupId <- client.requestBackup(config)
   *   _ <- IO.println(s"Backup requested: \$backupId")
   * yield backupId
   * }}}
   */
  def requestBackup(instance: ArrInstanceConfig): F[BackupId]
  
  /**
   * Get the status of a backup.
   * 
   * Polls the *arr API to check if a backup is complete.
   * The *arr instance may take several minutes to create a backup.
   * 
   * @param instance Configuration for the *arr instance
   * @param backupId ID of the backup to check
   * @return Current status of the backup
   */
  def getBackupStatus(instance: ArrInstanceConfig, backupId: BackupId): F[BackupStatus]
  
  /**
   * Download a completed backup from the *arr instance.
   * 
   * Streams the backup file from the *arr API to a local file.
   * Uses streaming to avoid loading the entire file into memory.
   * 
   * The filename is determined from the API response (backup metadata)
   * and will be placed in the destination directory.
   * 
   * @param instance Configuration for the *arr instance
   * @param backupId ID of the backup to download
   * @param destinationDir Directory where the backup file should be saved
   * @return Path to the downloaded file (destinationDir/backupFilename.zip)
   * 
   * Note: The returned F will fail if:
   * - The backup doesn't exist
   * - The backup isn't complete yet
   * - Network errors occur
   * - Disk is full
   * 
   * Example:
   * {{{
   * val tempDir = Files.createTempDirectory("backups")
   * // Downloads to tempDir/sonarr_backup_v4.0.16.2944_2025.11.23.zip
   * downloadBackup(config, backupId, tempDir)
   * }}}
   */
  def downloadBackup(
    instance: ArrInstanceConfig,
    backupId: BackupId,
    destinationDir: Path
  ): F[Path]

object ArrClient:
  /**
   * Smart constructor for ArrClient.
   * 
   * Creates an HTTP-based ArrClient implementation.
   * The Async constraint means F must support:
   * - Asynchronous operations
   * - Cancellation
   * - Error handling
   * 
   * Example usage:
   * {{{
   * import cats.effect.IO
   * import org.http4s.ember.client.EmberClientBuilder
   * import org.typelevel.log4cats.slf4j.Slf4jLogger
   * 
   * val program = for
   *   logger <- Slf4jLogger.create[IO]
   *   client <- EmberClientBuilder.default[IO].build.use { httpClient =>
   *     ArrClient.make[IO](httpClient, logger)
   *   }
   * yield client
   * }}}
   */
  def make[F[_]: Async: fs2.io.file.Files](
    httpClient: org.http4s.client.Client[F],
    logger: org.typelevel.log4cats.Logger[F]
  ): F[ArrClient[F]] =
    com.backuparr.impl.ArrClientImpl.make[F](httpClient, logger)
