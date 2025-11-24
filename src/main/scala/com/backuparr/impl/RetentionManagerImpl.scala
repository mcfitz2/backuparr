package com.backuparr.impl

import cats.effect.Async
import cats.syntax.all.*
import com.backuparr.algebras.{RetentionManager, S3Client}
import com.backuparr.config.{RetentionPolicyConfig, S3BucketConfig}
import com.backuparr.domain.{BackupError, RetentionResult, S3Object}
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger

import java.time.{Instant, LocalDate, ZoneOffset}
import java.time.temporal.ChronoUnit
import scala.util.Try

/**
 * Implementation of RetentionManager that applies backup retention policies.
 * 
 * This implementation:
 * - Lists backups from S3 for a specific instance
 * - Evaluates which backups to keep based on retention rules
 * - Deletes backups that don't match any retention criteria
 * - Handles deletion errors gracefully (continues on failure)
 * - Provides detailed logging of retention decisions
 * 
 * Retention Logic:
 * - A backup is kept if it matches ANY retention rule
 * - keepLast: Keep the N most recent backups
 * - keepDaily: Keep one backup per day for last N days
 * - keepWeekly: Keep one backup per week for last N weeks (Monday-Sunday)
 * - keepMonthly: Keep one backup per month for last N months
 * 
 * @param s3Client S3 client for listing and deleting objects
 */
class RetentionManagerImpl[F[_]: Async](
  s3Client: S3Client[F]
) extends RetentionManager[F]:
  
  private val logger: Logger[F] = Slf4jLogger.getLogger[F]
  
  override def applyRetention(
    bucket: S3BucketConfig,
    policy: RetentionPolicyConfig,
    instanceName: String
  ): F[RetentionResult] =
    for
      _ <- logger.info(s"Applying retention policy for instance: $instanceName")
      _ <- logger.debug(s"Policy: keepLast=${policy.keepLast}, keepDaily=${policy.keepDaily}, " +
        s"keepWeekly=${policy.keepWeekly}, keepMonthly=${policy.keepMonthly}")
      
      // List all backups for this instance
      // Note: S3Client.listObjects will prepend bucket.prefix automatically
      prefix = s"$instanceName/"
      allBackups <- s3Client.listObjects(bucket, prefix)
      _ <- logger.info(s"Found ${allBackups.size} backups for $instanceName")
      
      // Sort by last modified (newest first)
      sortedBackups = allBackups.sortBy(_.lastModified.toEpochMilli).reverse
      
      // Determine which backups to keep
      toKeep = evaluateRetentionPolicy(sortedBackups, policy)
      toDelete = sortedBackups.filterNot(backup => toKeep.contains(backup))
      
      _ <- logger.info(s"Retention evaluation: ${toKeep.size} to keep, ${toDelete.size} to delete")
      
      // Delete backups that don't match retention policy
      deleteResults <- toDelete.traverse { backup =>
        s3Client.deleteObject(bucket, backup.key).attempt.flatMap {
          case Right(_) =>
            logger.info(s"Deleted backup: ${backup.key}") >>
            Async[F].pure(Right(backup))
          case Left(error) =>
            logger.error(error)(s"Failed to delete backup: ${backup.key}") >>
            Async[F].pure(Left(BackupError.RetentionError(
              s"Failed to delete ${backup.key}: ${error.getMessage}",
              Some(error)
            )))
        }
      }
      
      // Separate successful deletes from errors
      (errors, deleted) = deleteResults.partitionMap(identity)
      
      _ <- logger.info(s"Retention complete: kept ${toKeep.size}, deleted ${deleted.size}, errors ${errors.size}")
      
    yield RetentionResult(
      kept = toKeep,
      deleted = deleted,
      errors = errors
    )
  
  /**
   * Evaluate which backups to keep based on retention policy.
   * 
   * A backup is kept if it matches ANY of the retention rules.
   * This prevents accidental deletion of important backups.
   */
  private def evaluateRetentionPolicy(
    backups: List[S3Object],
    policy: RetentionPolicyConfig
  ): List[S3Object] =
    val now = Instant.now()
    
    // Determine which backups match each retention rule
    val keepLastBackups = policy.keepLast.fold(Set.empty[S3Object]) { n =>
      backups.take(n).toSet
    }
    
    val keepDailyBackups = policy.keepDaily.fold(Set.empty[S3Object]) { days =>
      selectDailyBackups(backups, now, days)
    }
    
    val keepWeeklyBackups = policy.keepWeekly.fold(Set.empty[S3Object]) { weeks =>
      selectWeeklyBackups(backups, now, weeks)
    }
    
    val keepMonthlyBackups = policy.keepMonthly.fold(Set.empty[S3Object]) { months =>
      selectMonthlyBackups(backups, now, months)
    }
    
    // Union of all retention rules (keep if matches ANY rule)
    val toKeep = keepLastBackups ++ keepDailyBackups ++ keepWeeklyBackups ++ keepMonthlyBackups
    
    // Return in original order (newest first)
    backups.filter(toKeep.contains)
  
  /**
   * Select one backup per day for the last N days.
   * 
   * For each day, selects the most recent backup from that day.
   */
  private def selectDailyBackups(
    backups: List[S3Object],
    now: Instant,
    days: Int
  ): Set[S3Object] =
    val cutoffDate = LocalDate.ofInstant(now, ZoneOffset.UTC).minusDays(days)
    
    backups
      .filter { backup =>
        val backupDate = LocalDate.ofInstant(backup.lastModified, ZoneOffset.UTC)
        !backupDate.isBefore(cutoffDate)
      }
      .groupBy { backup =>
        LocalDate.ofInstant(backup.lastModified, ZoneOffset.UTC)
      }
      .values
      .map(_.maxBy(_.lastModified.toEpochMilli))
      .toSet
  
  /**
   * Select one backup per week for the last N weeks.
   * 
   * Weeks are Monday-Sunday. For each week, selects the most recent backup.
   */
  private def selectWeeklyBackups(
    backups: List[S3Object],
    now: Instant,
    weeks: Int
  ): Set[S3Object] =
    val cutoffDate = LocalDate.ofInstant(now, ZoneOffset.UTC).minusWeeks(weeks)
    
    backups
      .filter { backup =>
        val backupDate = LocalDate.ofInstant(backup.lastModified, ZoneOffset.UTC)
        !backupDate.isBefore(cutoffDate)
      }
      .groupBy { backup =>
        val backupDate = LocalDate.ofInstant(backup.lastModified, ZoneOffset.UTC)
        // Get the Monday of the week (ISO week starts on Monday)
        val weekStart = backupDate.minusDays(backupDate.getDayOfWeek.getValue - 1)
        weekStart
      }
      .values
      .map(_.maxBy(_.lastModified.toEpochMilli))
      .toSet
  
  /**
   * Select one backup per month for the last N months.
   * 
   * For each month, selects the most recent backup.
   */
  private def selectMonthlyBackups(
    backups: List[S3Object],
    now: Instant,
    months: Int
  ): Set[S3Object] =
    val cutoffDate = LocalDate.ofInstant(now, ZoneOffset.UTC).minusMonths(months)
    
    backups
      .filter { backup =>
        val backupDate = LocalDate.ofInstant(backup.lastModified, ZoneOffset.UTC)
        !backupDate.isBefore(cutoffDate)
      }
      .groupBy { backup =>
        val backupDate = LocalDate.ofInstant(backup.lastModified, ZoneOffset.UTC)
        // Group by year-month
        (backupDate.getYear, backupDate.getMonthValue)
      }
      .values
      .map(_.maxBy(_.lastModified.toEpochMilli))
      .toSet

object RetentionManagerImpl:
  /**
   * Create a RetentionManagerImpl instance.
   * 
   * @param s3Client S3 client for listing and deleting objects
   */
  def make[F[_]: Async](s3Client: S3Client[F]): F[RetentionManager[F]] =
    Async[F].pure(new RetentionManagerImpl[F](s3Client))
