package com.backuparr.impl

import cats.effect.{Async, Temporal}
import cats.syntax.all.*
import org.http4s.*
import org.http4s.client.Client
import org.http4s.circe.*
import org.http4s.headers.Authorization
import org.typelevel.log4cats.Logger
import org.typelevel.ci.CIStringSyntax
import io.circe.syntax.*
import fs2.io.file.{Files, Path as Fs2Path}
import fs2.Stream
import scala.concurrent.duration.*
import java.nio.file.Path

import com.backuparr.algebras.ArrClient
import com.backuparr.config.ArrInstanceConfig
import com.backuparr.domain.*

/**
 * Exception wrapper for BackupError to make it compatible with Cats Effect error handling.
 * This allows us to use BackupError with raiseError and adaptError while maintaining type safety.
 *
 * @param error The underlying BackupError
 */
case class BackupErrorException(error: BackupError) extends RuntimeException(error.toString)

/**
 * HTTP-based implementation of ArrClient.
 * 
 * This implementation communicates with *arr applications via their HTTP API.
 * It handles:
 * - Authentication via X-Api-Key header
 * - Retry logic with exponential backoff
 * - Streaming file downloads
 * - Error handling and conversion to domain errors
 * 
 * @param httpClient HTTP client for making requests
 * @param logger Functional logger for operation tracking
 */
class ArrClientImpl[F[_]: Async: Files](
  httpClient: Client[F],
  logger: Logger[F]
) extends ArrClient[F]:
  
  // Implicit decoders for JSON responses
  // These allow http4s to automatically decode JSON responses
  given EntityDecoder[F, CommandResponse] = jsonOf[F, CommandResponse]
  given EntityDecoder[F, List[BackupInfo]] = jsonOf[F, List[BackupInfo]]
  given EntityDecoder[F, SystemStatus] = jsonOf[F, SystemStatus]
  given EntityDecoder[F, ArrApiErrorResponse] = jsonOf[F, ArrApiErrorResponse]
  
  // Implicit encoder for JSON requests
  given EntityEncoder[F, BackupRequest] = jsonEncoderOf[F, BackupRequest]
  
  /**
   * Request a backup from the *arr instance.
   * 
   * This sends a POST request to /api/v{version}/command with the Backup command.
   * The *arr instance will queue the backup and return a command ID.
   * 
   * Implementation notes:
   * - Detects the correct API version (v1 for Lidarr, v3 for Sonarr/Radarr)
   * - Uses retry logic for transient failures
   * - Validates response contains a valid command ID
   * - Converts HTTP errors to domain BackupError types
   */
  override def requestBackup(instance: ArrInstanceConfig): F[BackupId] =
    for
      _ <- logger.info(s"Requesting backup for instance: ${instance.name}")
      
      // Load API key (from direct config or file)
      apiKey <- loadApiKey(instance)
      
      // Detect the API version
      apiVersion <- detectApiVersion(instance, apiKey)
      _ <- logger.debug(s"Detected API version $apiVersion for ${instance.name}")
      
      // Build the request
      request = Request[F](
        method = Method.POST,
        uri = Uri.unsafeFromString(s"${instance.url}/api/$apiVersion/command")
      )
        .withHeaders(Header.Raw(ci"X-Api-Key", apiKey))
        .withEntity(BackupRequest())
      
      // Execute with retry logic
      response <- retryWithBackoff(
        httpClient.expect[CommandResponse](request),
        maxAttempts = 3,
        initialDelay = 1.second
      ).adaptError:
        case e: org.http4s.client.UnexpectedStatus if e.status == Status.Unauthorized =>
          BackupErrorException(BackupError.ArrApiError("Unauthorized - check API key", Some(e)))
        case e: org.http4s.client.UnexpectedStatus =>
          BackupErrorException(BackupError.ArrApiError(s"HTTP ${e.status.code}: ${e.status.reason}", Some(e)))
        case e: Exception =>
          BackupErrorException(BackupError.ArrApiError(s"Failed to request backup: ${e.getMessage}", Some(e)))
      
      _ <- logger.info(s"Backup requested successfully for ${instance.name}, command ID: ${response.id}")
      
    yield BackupId(response.id.toString)
  
  /**
   * Get the status of a backup command.
   * 
   * Polls the /api/v{version}/command/{id} endpoint to check if the backup is complete.
   * When complete, fetches the backup list to get the most recent backup path.
   * 
   * @param instance Instance configuration
   * @param backupId ID returned from requestBackup
   * @return Current backup status
   */
  override def getBackupStatus(
    instance: ArrInstanceConfig,
    backupId: BackupId
  ): F[BackupStatus] =
    for
      apiKey <- loadApiKey(instance)
      apiVersion <- detectApiVersion(instance, apiKey)
      
      request = Request[F](
        method = Method.GET,
        uri = Uri.unsafeFromString(s"${instance.url}/api/$apiVersion/command/${backupId.value}")
      ).withHeaders(Header.Raw(ci"X-Api-Key", apiKey))
      
      response <- httpClient.expect[CommandResponse](request).adaptError:
        case e: Exception =>
          BackupErrorException(BackupError.ArrApiError(s"Failed to get backup status: ${e.getMessage}", Some(e)))
      
      // Convert command status to backup status
      status <- response.status match
        case CommandStatus.Queued | CommandStatus.Started =>
          Async[F].pure(BackupStatus.Requesting)
        
        case CommandStatus.Completed =>
          // Backup is complete, fetch the backup list to get the most recent backup
          val backupsRequest = Request[F](
            method = Method.GET,
            uri = Uri.unsafeFromString(s"${instance.url}/api/$apiVersion/system/backup")
          ).withHeaders(Header.Raw(ci"X-Api-Key", apiKey))
          
          httpClient.expect[List[BackupInfo]](backupsRequest).flatMap: backups =>
            backups.headOption match
              case Some(backupInfo) =>
                logger.info(s"Backup complete for ${instance.name}: ${backupInfo.path}") *>
                  Async[F].pure(BackupStatus.Downloading)
              case None =>
                Async[F].raiseError(
                  BackupErrorException(BackupError.ArrApiError("Backup completed but no backups found", None))
                )
          .adaptError:
            case e: Exception =>
              BackupErrorException(BackupError.ArrApiError(s"Failed to fetch backup list: ${e.getMessage}", Some(e)))
        
        case CommandStatus.Failed =>
          val errorMsg = response.message.getOrElse("Backup failed with no error message")
          Async[F].raiseError(
            BackupErrorException(BackupError.ArrApiError(s"Backup failed: $errorMsg", None))
          )
        
        case CommandStatus.Aborted =>
          Async[F].raiseError(
            BackupErrorException(BackupError.ArrApiError("Backup was aborted", None))
          )
        
        case CommandStatus.Unknown =>
          Async[F].raiseError(
            BackupErrorException(BackupError.ArrApiError("Backup has unknown status", None))
          )
      
      _ <- logger.debug(s"Backup status for ${instance.name} (${backupId.value}): $status")
      
    yield status
  
  /**
   * Download a completed backup from the *arr instance.
   * 
   * This streams the backup file from the *arr API to local storage.
   * Uses streaming to avoid loading the entire file into memory.
   * 
   * The backup is downloaded from /api/v{version}/system/backup/{filename}
   * 
   * @param instance Instance configuration
   * @param backupId ID of the completed backup
   * @param destination Local path where backup should be saved
   * @return Path to the downloaded file
   */
  /**
   * Wait for a backup to become available with retry logic.
   * Backups may take a few seconds to appear in the backup list after being triggered.
   */
  private def waitForBackup(
    instance: ArrInstanceConfig,
    apiKey: String,
    apiVersion: String,
    maxAttempts: Int = 10,
    initialDelay: FiniteDuration = 500.millis
  ): F[BackupInfo] =
    def attempt(attemptNum: Int, delay: FiniteDuration): F[BackupInfo] =
      val backupsRequest = Request[F](
        method = Method.GET,
        uri = Uri.unsafeFromString(s"${instance.url}/api/$apiVersion/system/backup")
      ).withHeaders(Header.Raw(ci"X-Api-Key", apiKey))
      
      for
        backups <- httpClient.expect[List[BackupInfo]](backupsRequest).adaptError:
          case e: Exception =>
            BackupErrorException(BackupError.ArrApiError(s"Failed to list backups: ${e.getMessage}", Some(e)))
        
        result <- backups.headOption match
          case Some(info) => 
            logger.debug(s"Found backup on attempt $attemptNum: ${info.name}") *>
            Async[F].pure(info)
          case None if attemptNum < maxAttempts =>
            logger.debug(s"No backup found yet (attempt $attemptNum/$maxAttempts), retrying in ${delay.toMillis}ms...") *>
            Temporal[F].sleep(delay) *>
            attempt(attemptNum + 1, delay * 2) // Exponential backoff
          case None =>
            Async[F].raiseError(
              BackupErrorException(BackupError.DownloadError(
                s"No backups found on instance after $maxAttempts attempts", 
                None
              ))
            )
      yield result
    
    attempt(1, initialDelay)

  override def downloadBackup(
    instance: ArrInstanceConfig,
    backupId: BackupId,
    destinationDir: Path
  ): F[Path] =
    for
      _ <- logger.info(s"Downloading backup ${backupId.value} for ${instance.name} to $destinationDir")
      
      // Get API credentials and version
      apiKey <- loadApiKey(instance)
      apiVersion <- detectApiVersion(instance, apiKey)
      
      // Wait for backup to become available (with retry logic)
      backupInfo <- waitForBackup(instance, apiKey, apiVersion)
      
      _ <- logger.debug(s"Found backup: ${backupInfo.name}")
      
      // Construct the full file path (destinationDir is a directory, append the backup filename)
      destinationFile = destinationDir.resolve(backupInfo.name)
      _ <- logger.debug(s"Downloading to file: $destinationFile")
      
      // Download the backup file using the path from the backup info
      // The path is relative to the instance URL (e.g., /backup/manual/filename.zip)
      downloadRequest = Request[F](
        method = Method.GET,
        uri = Uri.unsafeFromString(s"${instance.url}${backupInfo.path}")
      ).withHeaders(Header.Raw(ci"X-Api-Key", apiKey))
      
      // Stream the response to file
      _ <- httpClient.stream(downloadRequest).flatMap: response =>
        response.status match
          case Status.Ok =>
            // Stream the body to the destination file
            response.body
              .through(Files[F].writeAll(Fs2Path.fromNioPath(destinationFile)))
          case status =>
            Stream.raiseError[F](
              BackupErrorException(BackupError.DownloadError(s"Download failed with status: $status", None))
            )
      .compile.drain.adaptError:
        case e: Exception =>
          BackupErrorException(BackupError.DownloadError(s"Failed to download backup: ${e.getMessage}", Some(e)))
      
      _ <- logger.info(s"Successfully downloaded backup to $destinationFile")
      
    yield destinationFile
  
  /**
   * Load the API key for an instance.
   * 
   * The API key can be specified directly in the config or loaded from a file.
   * This is to support Kubernetes secrets mounted as files.
   * 
   * @param instance Instance configuration
   * @return API key string
   */
  private def loadApiKey(instance: ArrInstanceConfig): F[String] =
    instance.apiKey match
      case Some(key) => 
        Async[F].pure(key)
      case None =>
        instance.apiKeyFile match
          case Some(filePath) =>
            // Read the API key from the file
            Files[F]
              .readAll(Fs2Path(filePath))
              .through(fs2.text.utf8.decode)
              .compile
              .string
              .map(_.trim)
              .adaptError:
                case e: Exception =>
                  BackupErrorException(BackupError.ConfigurationError(
                    s"Failed to read API key from file $filePath: ${e.getMessage}"
                  ))
          case None =>
            Async[F].raiseError(
              BackupErrorException(BackupError.ConfigurationError("No API key or API key file specified"))
            )
  
  /**
   * Detect the API version for an *arr instance.
   * 
   * Different *arr applications use different API versions:
   * - Lidarr 3.x uses /api/v1/
   * - Sonarr v4 and Radarr v6 use /api/v3/
   * 
   * We detect this by trying v3 first, then falling back to v1.
   * 
   * @param instance Instance configuration
   * @param apiKey API key for authentication
   * @return API version string ("v1" or "v3")
   */
  private def detectApiVersion(instance: ArrInstanceConfig, apiKey: String): F[String] =
    def tryVersion(version: String): F[Option[String]] =
      val request = Request[F](
        method = Method.GET,
        uri = Uri.unsafeFromString(s"${instance.url}/api/$version/system/status")
      ).withHeaders(Header.Raw(ci"X-Api-Key", apiKey))
      
      httpClient.expect[SystemStatus](request)
        .map(_ => Some(version))
        .handleError(_ => None)
    
    // Try v3 first (most common), then v1
    tryVersion("v3").flatMap:
      case Some(version) => Async[F].pure(version)
      case None => tryVersion("v1").flatMap:
        case Some(version) => Async[F].pure(version)
        case None => Async[F].raiseError(
          BackupErrorException(BackupError.ArrApiError(
            "Could not detect API version (tried v3 and v1)",
            None
          ))
        )
  
  /**
   * Retry an operation with exponential backoff.
   * 
   * This is a common pattern in distributed systems for handling transient failures.
   * 
   * @param fa The operation to retry
   * @param maxAttempts Maximum number of attempts
   * @param initialDelay Initial delay between retries
   * @return Result of the operation
   */
  private def retryWithBackoff[A](
    fa: F[A],
    maxAttempts: Int,
    initialDelay: FiniteDuration
  ): F[A] =
    def attempt(attemptNumber: Int, delay: FiniteDuration): F[A] =
      fa.handleErrorWith: error =>
        if attemptNumber >= maxAttempts then
          logger.error(error)(s"Operation failed after $maxAttempts attempts") *>
            Async[F].raiseError(error)
        else
          logger.warn(s"Operation failed (attempt $attemptNumber/$maxAttempts), retrying after $delay: ${error.getMessage}") *>
            Temporal[F].sleep(delay) *>
            attempt(attemptNumber + 1, delay * 2)
    
    attempt(1, initialDelay)

object ArrClientImpl:
  /**
   * Create an ArrClient instance.
   * 
   * This is the smart constructor that should be used to create ArrClient instances.
   * 
   * @param httpClient HTTP client for making requests
   * @param logger Logger for operation tracking
   * @return ArrClient instance
   */
  def make[F[_]: Async: Files](
    httpClient: Client[F],
    logger: Logger[F]
  ): F[ArrClient[F]] =
    Async[F].pure(new ArrClientImpl[F](httpClient, logger))
