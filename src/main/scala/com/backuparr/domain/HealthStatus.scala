package com.backuparr.domain

import java.time.Instant

/**
 * Health status information for the application.
 */
final case class HealthStatus(
  /** Whether the application is alive (can respond to requests) */
  alive: Boolean,
  /** Whether the application is ready to serve traffic */
  ready: Boolean,
  /** Detailed status message */
  message: String,
  /** Timestamp of the health check */
  timestamp: Instant
)

/**
 * Detailed application status including backup metrics.
 */
final case class ApplicationStatus(
  /** Overall health status */
  health: HealthStatus,
  /** Whether the scheduler is running */
  schedulerRunning: Boolean,
  /** Status for each configured instance */
  instances: Map[String, InstanceStatus]
)

/**
 * Status for a single *arr instance.
 */
final case class InstanceStatus(
  /** Instance name */
  name: String,
  /** *arr type (Sonarr, Radarr, etc.) */
  arrType: String,
  /** Current backup status */
  currentStatus: Option[BackupStatus],
  /** Timestamp of last successful backup */
  lastSuccessfulBackup: Option[Instant],
  /** Timestamp of last failed backup */
  lastFailedBackup: Option[Instant],
  /** Total successful backups */
  successCount: Long,
  /** Total failed backups */
  failureCount: Long
)
