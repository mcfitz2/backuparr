package com.backuparr.impl

import cats.effect.{Async, Deferred, Ref}
import cats.effect.std.Semaphore
import cats.syntax.all.*
import com.backuparr.algebras.{BackupManager, Scheduler}
import com.backuparr.config.ArrInstanceConfig
import fs2.Stream
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger

import java.time.{Duration, LocalDateTime, ZoneId}
import scala.concurrent.duration.*

/**
 * Implementation of Scheduler using fs2 streams.
 * 
 * This implementation:
 * - Parses cron expressions for each instance
 * - Creates an fs2.Stream for each schedule
 * - Calculates next execution time and sleeps until then
 * - Executes backups concurrently with Semaphore for rate limiting
 * - Supports graceful shutdown via Deferred
 * - Logs all scheduling decisions and backup executions
 * 
 * The scheduler runs continuously until stop() is called or the application terminates.
 * 
 * @param backupManager Manager to execute backups
 * @param instances List of instances to schedule
 * @param maxConcurrent Maximum number of concurrent backups
 * @param stopSignal Deferred signal for graceful shutdown
 * @param semaphore Semaphore for concurrent execution control
 */
class SchedulerImpl[F[_]: Async](
  backupManager: BackupManager[F],
  instances: List[ArrInstanceConfig],
  maxConcurrent: Int,
  stopSignal: Deferred[F, Unit],
  semaphore: Semaphore[F]
) extends Scheduler[F]:
  
  private val logger: Logger[F] = Slf4jLogger.getLogger[F]
  
  override def start: F[Unit] =
    for
      _ <- logger.info(s"Starting scheduler with ${instances.size} instances, max concurrent: $maxConcurrent")
      
      // Parse cron expressions for all instances
      schedules <- instances.traverse { instance =>
        CronExpression.parse(instance.schedule) match
          case Some(cron) =>
            Async[F].pure((instance, cron))
          case None =>
            logger.error(s"Invalid cron expression for ${instance.name}: ${instance.schedule}") >>
            Async[F].raiseError(new IllegalArgumentException(s"Invalid cron expression: ${instance.schedule}"))
      }
      
      _ <- logger.info(s"Successfully parsed ${schedules.size} cron schedules")
      
      // Create a stream for each schedule
      streams = schedules.map { case (instance, cron) =>
        scheduleStream(instance, cron)
      }
      
      // Merge all streams and run until stop signal
      _ <- Stream.emits(streams)
        .parJoinUnbounded
        .interruptWhen(stopSignal.get.attempt)
        .compile
        .drain
      
      _ <- logger.info("Scheduler stopped")
      
    yield ()
  
  override def stop: F[Unit] =
    for
      _ <- logger.info("Stopping scheduler...")
      _ <- stopSignal.complete(())
      _ <- logger.info("Scheduler stop signal sent")
    yield ()
  
  /**
   * Create a stream that schedules backups for a single instance.
   * 
   * This stream:
   * 1. Calculates next execution time
   * 2. Sleeps until that time
   * 3. Executes backup (with semaphore for concurrency control)
   * 4. Repeats
   */
  private def scheduleStream(
    instance: ArrInstanceConfig,
    cron: CronExpression
  ): Stream[F, Unit] =
    Stream.eval(logger.debug(s"Creating schedule stream for ${instance.name}: ${instance.schedule}")) >>
    Stream.unfoldLoopEval(LocalDateTime.now(ZoneId.systemDefault())) { currentTime =>
      for
        // Calculate next execution time
        nextTime <- Async[F].fromOption(
          cron.nextExecution(currentTime),
          new RuntimeException(s"Could not calculate next execution time for ${instance.name}")
        )
        
        // Calculate delay until next execution
        now = LocalDateTime.now(ZoneId.systemDefault())
        delay = Duration.between(now, nextTime)
        delayMillis = delay.toMillis.max(0)
        
        _ <- logger.debug(s"${instance.name}: Next backup scheduled for $nextTime (in ${delayMillis}ms)")
        
        // Sleep until next execution
        _ <- Async[F].sleep(delayMillis.millis)
        
        // Execute backup with semaphore (prevents too many concurrent backups)
        _ <- semaphore.permit.use { _ =>
          for
            _ <- logger.info(s"Executing scheduled backup for ${instance.name}")
            startTime <- Async[F].delay(System.currentTimeMillis())
            
            result <- backupManager.executeBackup(instance).attempt
            
            endTime <- Async[F].delay(System.currentTimeMillis())
            duration = endTime - startTime
            
            _ <- result match
              case Right(backupResult) =>
                if backupResult.isSuccessful then
                  logger.info(s"Scheduled backup completed for ${instance.name} in ${duration}ms: ${backupResult.s3Uri.getOrElse("no URI")}")
                else
                  logger.error(s"Scheduled backup failed for ${instance.name} after ${duration}ms: ${backupResult.status}")
              
              case Left(error) =>
                logger.error(error)(s"Scheduled backup threw exception for ${instance.name} after ${duration}ms")
          yield ()
        }
        
      yield ((), Some(nextTime))
    }

object SchedulerImpl:
  /**
   * Create a SchedulerImpl instance.
   * 
   * @param backupManager Manager to execute backups
   * @param instances List of instances to schedule
   * @param maxConcurrent Maximum number of concurrent backups (default: 3)
   */
  def make[F[_]: Async](
    backupManager: BackupManager[F],
    instances: List[ArrInstanceConfig],
    maxConcurrent: Int = 3
  ): F[Scheduler[F]] =
    for
      stopSignal <- Deferred[F, Unit]
      semaphore <- Semaphore[F](maxConcurrent)
    yield new SchedulerImpl[F](
      backupManager,
      instances,
      maxConcurrent,
      stopSignal,
      semaphore
    )
