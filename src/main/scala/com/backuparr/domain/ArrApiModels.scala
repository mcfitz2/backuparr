package com.backuparr.domain

import io.circe.{Decoder, Encoder}
import io.circe.generic.semiauto.*
import java.time.Instant

/**
 * Models for *arr API requests and responses.
 * 
 * These models represent the JSON structures used by the *arr APIs
 * (Sonarr, Radarr, Lidarr, etc.). All these applications share a
 * common API structure for backup operations.
 * 
 * API Documentation:
 * - Sonarr: https://sonarr.tv/docs/api/
 * - Radarr: https://radarr.video/docs/api/
 */

/**
 * Request to create a backup.
 * 
 * This is sent as a POST to /api/v3/command with:
 * {
 *   "name": "Backup"
 * }
 */
case class BackupRequest(
  name: String = "Backup"
)

object BackupRequest:
  given Encoder[BackupRequest] = deriveEncoder[BackupRequest]
  given Decoder[BackupRequest] = deriveDecoder[BackupRequest]

/**
 * Response from a command request.
 * 
 * When we trigger a backup, the *arr API returns a command object
 * that tracks the execution of that command.
 */
case class CommandResponse(
  /** Unique ID for this command */
  id: Int,
  
  /** Name of the command (e.g., "Backup") */
  name: String,
  
  /** Current state of the command */
  status: CommandStatus,
  
  /** When the command was queued */
  queued: Instant,
  
  /** When the command started executing (if started) */
  started: Option[Instant],
  
  /** When the command completed (if completed) */
  ended: Option[Instant],
  
  /** Error message if the command failed */
  message: Option[String],
  
  /** Body of the command (contains backup-specific info) */
  body: Option[CommandBody]
)

object CommandResponse:
  given Decoder[CommandResponse] = deriveDecoder[CommandResponse]
  given Encoder[CommandResponse] = deriveEncoder[CommandResponse]

/**
 * Status of a command.
 * 
 * Commands can be in various states as they execute.
 */
enum CommandStatus:
  case Queued
  case Started
  case Completed
  case Failed
  case Aborted
  case Unknown

object CommandStatus:
  given Decoder[CommandStatus] = Decoder.decodeString.map:
    case "queued" => Queued
    case "started" => Started
    case "completed" => Completed
    case "failed" => Failed
    case "aborted" => Aborted
    case _ => Unknown
  
  given Encoder[CommandStatus] = Encoder.encodeString.contramap:
    case Queued => "queued"
    case Started => "started"
    case Completed => "completed"
    case Failed => "failed"
    case Aborted => "aborted"
    case Unknown => "unknown"

/**
 * Body of a command response.
 * 
 * For backup commands, this contains information about the backup file.
 */
case class CommandBody(
  /** Path to the backup file (if completed) */
  path: Option[String]
)

object CommandBody:
  given Decoder[CommandBody] = deriveDecoder[CommandBody]
  given Encoder[CommandBody] = deriveEncoder[CommandBody]

/**
 * Information about a backup file.
 * 
 * Retrieved from /api/v3/system/backup
 */
case class BackupInfo(
  /** Name of the backup file */
  name: String,
  
  /** Full path to the backup file */
  path: String,
  
  /** Type of backup (scheduled, manual, update) */
  `type`: String,
  
  /** When the backup was created */
  time: Instant,
  
  /** Unique identifier for this backup */
  id: Option[Int]
)

object BackupInfo:
  given Decoder[BackupInfo] = deriveDecoder[BackupInfo]
  given Encoder[BackupInfo] = deriveEncoder[BackupInfo]

/**
 * System status response from /api/v3/system/status
 * 
 * Used to verify connectivity and get instance information.
 */
case class SystemStatus(
  /** Version of the *arr application */
  version: String,
  
  /** Build time */
  buildTime: Instant,
  
  /** Whether authentication is required */
  isDebug: Boolean,
  
  /** Whether the instance is production */
  isProduction: Boolean,
  
  /** Whether authentication is required */
  isAdmin: Boolean,
  
  /** Whether the instance uses authentication */
  authentication: String,
  
  /** Application name (Sonarr, Radarr, etc.) */
  appName: String,
  
  /** Instance name */
  instanceName: Option[String]
)

object SystemStatus:
  given Decoder[SystemStatus] = deriveDecoder[SystemStatus]
  given Encoder[SystemStatus] = deriveEncoder[SystemStatus]

/**
 * Error response from the *arr API.
 * 
 * When an API call fails, the *arr API typically returns JSON
 * with error details.
 */
case class ArrApiErrorResponse(
  message: String,
  description: Option[String]
)

object ArrApiErrorResponse:
  given Decoder[ArrApiErrorResponse] = deriveDecoder[ArrApiErrorResponse]
