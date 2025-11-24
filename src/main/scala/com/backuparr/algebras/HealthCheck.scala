package com.backuparr.algebras

import cats.effect.kernel.Async
import com.backuparr.domain.{ApplicationStatus, HealthStatus}

/**
 * Algebra for health check operations.
 * 
 * Provides endpoints for Kubernetes probes and monitoring:
 * - Liveness: Is the application running? (restart if false)
 * - Readiness: Can the application serve traffic? (remove from LB if false)
 * - Status: Detailed operational metrics
 */
trait HealthCheck[F[_]]:
  
  /**
   * Liveness check - determines if the application is alive.
   * 
   * Returns true if the application can respond to requests.
   * If this returns false, Kubernetes will restart the container.
   * 
   * Checks:
   * - Application can respond (not deadlocked)
   * - No unrecoverable errors
   * 
   * @return health status with alive flag
   */
  def liveness: F[HealthStatus]
  
  /**
   * Readiness check - determines if the application is ready to serve traffic.
   * 
   * Returns true if all dependencies are available and the application
   * can successfully process backup requests.
   * If this returns false, Kubernetes will remove the pod from the load balancer.
   * 
   * Checks:
   * - Configuration loaded successfully
   * - S3 credentials available
   * - At least one instance configured
   * - Scheduler is running (if enabled)
   * 
   * @return health status with ready flag
   */
  def readiness: F[HealthStatus]
  
  /**
   * Detailed status check - provides operational metrics.
   * 
   * Returns comprehensive status including:
   * - Per-instance backup statistics
   * - Scheduler state
   * - Last backup times
   * - Success/failure counts
   * 
   * Used for monitoring and debugging.
   * 
   * @return detailed application status
   */
  def status: F[ApplicationStatus]

object HealthCheck:
  
  /**
   * Create a HealthCheck instance.
   * 
   * Implementation will be provided by HealthCheckImpl.
   */
  def make[F[_]: Async](
    backupManager: BackupManager[F],
    s3Client: S3Client[F],
    schedulerRunning: F[Boolean],
    instanceConfigs: List[com.backuparr.config.ArrInstanceConfig],
    s3Buckets: Map[String, com.backuparr.config.S3BucketConfig]
  ): F[HealthCheck[F]] = 
    com.backuparr.impl.HealthCheckImpl.make[F](
      backupManager,
      s3Client,
      schedulerRunning,
      instanceConfigs,
      s3Buckets
    )
