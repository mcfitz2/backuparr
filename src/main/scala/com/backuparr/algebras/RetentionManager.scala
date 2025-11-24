package com.backuparr.algebras

import cats.effect.Async
import com.backuparr.config.{RetentionPolicyConfig, S3BucketConfig}
import com.backuparr.domain.RetentionResult

/**
 * Algebra for managing backup retention policies.
 * 
 * The RetentionManager determines which backups to keep and which to delete
 * based on configured retention policies. It supports multiple strategies:
 * - Keep last N backups
 * - Keep daily backups for N days
 * - Keep weekly backups for N weeks
 * - Keep monthly backups for N months
 * 
 * Multiple policies can be combined - a backup is kept if it matches ANY policy.
 */
trait RetentionManager[F[_]]:
  /**
   * Apply retention policy to backups in an S3 bucket.
   * 
   * This operation:
   * 1. Lists all backups for the instance in the bucket
   * 2. Evaluates which backups to keep based on the policy
   * 3. Deletes backups that don't match any retention rule
   * 4. Returns a summary of what was kept and deleted
   * 
   * @param bucket S3 bucket configuration
   * @param policy Retention policy to apply
   * @param instanceName Name of the instance (for filtering backups)
   * @return Result showing kept/deleted backups and any errors
   * 
   * Example:
   * {{{
   * val manager: RetentionManager[IO] = ???
   * val bucket: S3BucketConfig = ???
   * val policy = RetentionPolicyConfig(
   *   keepLast = Some(7),
   *   keepDaily = Some(30),
   *   keepMonthly = Some(12)
   * )
   * 
   * val apply: IO[RetentionResult] = for
   *   result <- manager.applyRetention(bucket, policy, "sonarr-main")
   *   _ <- IO.println(s"Kept: \${result.kept.size}, Deleted: \${result.deleted.size}")
   *   _ <- result.errors.traverse_(err => IO.println(s"Error: \${err.message}"))
   * yield result
   * }}}
   */
  def applyRetention(
    bucket: S3BucketConfig,
    policy: RetentionPolicyConfig,
    instanceName: String
  ): F[RetentionResult]

object RetentionManager:
  /**
   * Create a RetentionManager instance.
   * 
   * The implementation will:
   * - Use S3Client to list and delete objects
   * - Implement retention policy evaluation logic
   * - Handle errors gracefully (continue if some deletes fail)
   * - Log retention decisions for audit trail
   * 
   * Retention Policy Logic:
   * 
   * - keepLast: Keep the N most recent backups
   * - keepDaily: Keep one backup per day for the last N days
   * - keepWeekly: Keep one backup per week for the last N weeks
   * - keepMonthly: Keep one backup per month for the last N months
   * 
   * A backup is kept if it satisfies ANY of the specified policies.
   * This prevents accidental deletion of backups that might be needed.
   */
  def make[F[_]: Async](s3Client: S3Client[F]): F[RetentionManager[F]] =
    import com.backuparr.impl.RetentionManagerImpl
    RetentionManagerImpl.make[F](s3Client)
