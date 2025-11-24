package com.backuparr.impl

import cats.effect.kernel.{Async, Ref}
import cats.syntax.all.*
import com.backuparr.algebras.{BackupManager, HealthCheck, S3Client}
import com.backuparr.config.{ArrInstanceConfig, S3BucketConfig}
import com.backuparr.domain.{ApplicationStatus, BackupStatus, HealthStatus, InstanceStatus}

import java.time.Instant

/**
 * Implementation of health check operations.
 * 
 * Provides liveness, readiness, and detailed status checks for Kubernetes
 * probes and monitoring systems.
 */
final class HealthCheckImpl[F[_]: Async] private (
  backupManager: BackupManager[F],
  s3Client: S3Client[F],
  schedulerRunning: F[Boolean],
  instanceConfigs: List[ArrInstanceConfig],
  s3Buckets: Map[String, S3BucketConfig],
  startupComplete: Ref[F, Boolean]
) extends HealthCheck[F]:
  
  /**
   * Liveness check - application can respond to requests.
   * 
   * This is a simple check that always returns true unless the application
   * is in an unrecoverable state. The fact that this method can be called
   * means the application is alive.
   */
  override def liveness: F[HealthStatus] =
    Async[F].realTimeInstant.map { now =>
      HealthStatus(
        alive = true,
        ready = false, // Not relevant for liveness
        message = "Application is alive",
        timestamp = now
      )
    }
  
  /**
   * Readiness check - application can serve traffic.
   * 
   * Checks:
   * - Startup completed (configuration loaded)
   * - At least one instance configured
   * - S3 buckets configured
   * - Scheduler is running (if instances are configured)
   */
  override def readiness: F[HealthStatus] =
    for
      now <- Async[F].realTimeInstant
      startup <- startupComplete.get
      running <- schedulerRunning
      
      // Determine readiness
      ready = startup && 
              instanceConfigs.nonEmpty && 
              s3Buckets.nonEmpty &&
              running
      
      message = (startup, instanceConfigs.nonEmpty, s3Buckets.nonEmpty, running) match
        case (false, _, _, _) => "Startup not complete"
        case (_, false, _, _) => "No instances configured"
        case (_, _, false, _) => "No S3 buckets configured"
        case (_, _, _, false) => "Scheduler not running"
        case _ => "Application is ready"
      
    yield HealthStatus(
      alive = true,
      ready = ready,
      message = message,
      timestamp = now
    )
  
  /**
   * Detailed status check - operational metrics.
   * 
   * Returns comprehensive status including per-instance backup statistics.
   */
  override def status: F[ApplicationStatus] =
    for
      now <- Async[F].realTimeInstant
      startup <- startupComplete.get
      running <- schedulerRunning
      
      // Get status for all instances
      backupStatuses <- backupManager.getStatus
      
      // Build instance statuses
      instanceStatuses <- instanceConfigs.traverse { config =>
        val backupStatus = backupStatuses.get(config.name)
        
        // Count successes and failures (in a real implementation, we'd track this)
        // For now, we'll use placeholder values
        val successCount = 0L
        val failureCount = 0L
        
        Async[F].pure(
          config.name -> InstanceStatus(
            name = config.name,
            arrType = config.arrType.toString,
            currentStatus = backupStatus,
            lastSuccessfulBackup = None, // TODO: Track this in BackupManager
            lastFailedBackup = None,      // TODO: Track this in BackupManager
            successCount = successCount,
            failureCount = failureCount
          )
        )
      }.map(_.toMap)
      
      healthStatus = HealthStatus(
        alive = true,
        ready = startup && instanceConfigs.nonEmpty && s3Buckets.nonEmpty && running,
        message = if startup && running then "Operational" else "Not ready",
        timestamp = now
      )
      
    yield ApplicationStatus(
      health = healthStatus,
      schedulerRunning = running,
      instances = instanceStatuses
    )
  
  /**
   * Mark startup as complete.
   * Should be called after configuration is loaded and scheduler is started.
   */
  def markStartupComplete: F[Unit] =
    startupComplete.set(true)

object HealthCheckImpl:
  
  /**
   * Create a new HealthCheckImpl instance.
   * 
   * @param backupManager the backup manager for status queries
   * @param s3Client the S3 client (for potential connectivity checks)
   * @param schedulerRunning effect that returns whether scheduler is running
   * @param instanceConfigs list of configured instances
   * @param s3Buckets map of S3 bucket configurations
   * @return health check implementation
   */
  def make[F[_]: Async](
    backupManager: BackupManager[F],
    s3Client: S3Client[F],
    schedulerRunning: F[Boolean],
    instanceConfigs: List[ArrInstanceConfig],
    s3Buckets: Map[String, S3BucketConfig]
  ): F[HealthCheck[F]] =
    for
      startupRef <- Ref.of[F, Boolean](false)
    yield new HealthCheckImpl[F](
      backupManager,
      s3Client,
      schedulerRunning,
      instanceConfigs,
      s3Buckets,
      startupRef
    )
