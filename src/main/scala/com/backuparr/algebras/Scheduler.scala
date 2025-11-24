package com.backuparr.algebras

import cats.effect.Async
import com.backuparr.config.ArrInstanceConfig

/**
 * Algebra for scheduling backups.
 * 
 * The Scheduler is responsible for triggering backups at the configured times.
 * It uses cron-like expressions to determine when to run backups for each instance.
 * 
 * This is a long-running process that should:
 * - Parse cron schedules
 * - Calculate next execution times
 * - Trigger backups at the right time
 * - Handle multiple concurrent schedules
 * - Support graceful shutdown
 */
trait Scheduler[F[_]]:
  /**
   * Start the scheduler.
   * 
   * This begins scheduling backups for all configured instances.
   * The operation runs indefinitely until cancelled or the application stops.
   * 
   * @return F[Unit] that runs until cancelled
   * 
   * Note: This should be run in the background using .background or similar.
   * 
   * Example:
   * {{{
   * val scheduler: Scheduler[IO] = ???
   * 
   * val app = scheduler.start.background.use { _ =>
   *   // Main application logic
   *   IO.never // Keep running
   * }
   * }}}
   */
  def start: F[Unit]
  
  /**
   * Stop the scheduler gracefully.
   * 
   * This should:
   * - Stop scheduling new backups
   * - Allow in-flight backups to complete
   * - Clean up resources
   * 
   * @return F[Unit] that completes when shutdown is done
   */
  def stop: F[Unit]

object Scheduler:
  /**
   * Create a Scheduler instance.
   * 
   * @param backupManager Manager to execute backups
   * @param instances List of instances to schedule
   * @return Scheduler instance
   * 
   * The implementation will:
   * - Parse cron expressions (custom implementation)
   * - Use fs2.Stream for continuous scheduling
   * - Calculate next execution time for each schedule
   * - Sleep until next execution
   * - Execute backup and repeat
   * - Use Ref to track scheduler state
   * - Support concurrent execution with Semaphore
   * 
   * Example cron expressions:
   * - "0 2 * * *"    - Daily at 2 AM
   * - "0 0/6 * * *"  - Every 6 hours (starting at midnight)
   * - "0 0 * * 0"    - Weekly on Sunday at midnight
   * - "0 0 1 * *"    - Monthly on the 1st at midnight
   */
  def make[F[_]: Async](
    backupManager: BackupManager[F],
    instances: List[ArrInstanceConfig],
    maxConcurrent: Int = 3
  ): F[Scheduler[F]] =
    import com.backuparr.impl.SchedulerImpl
    SchedulerImpl.make[F](backupManager, instances, maxConcurrent)
