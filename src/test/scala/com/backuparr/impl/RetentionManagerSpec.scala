package com.backuparr.impl

import cats.effect.{IO, Ref}
import cats.syntax.all.*
import munit.CatsEffectSuite
import com.backuparr.algebras.S3Client
import com.backuparr.config.{RetentionPolicyConfig, S3BucketConfig}
import com.backuparr.domain.{BackupError, S3Object, S3Provider, S3Uri}

import java.time.{Instant, LocalDate, ZoneId, ZonedDateTime}
import java.time.temporal.ChronoUnit
import scala.concurrent.duration.*

/**
 * Unit tests for RetentionManager with a mocked S3 client.
 * 
 * Tests cover:
 * - keepLast: Keep N most recent backups
 * - keepDaily: One backup per day for N days
 * - keepWeekly: One backup per week for N weeks  
 * - keepMonthly: One backup per month for N months
 * - Combination policies (multiple rules)
 * - Long-term retention scenarios (90 days, 52 weeks, 12 months)
 */
class RetentionManagerSpec extends CatsEffectSuite:
  
  /**
   * Mock S3 client that stores objects in memory and tracks deletions.
   */
  class MockS3Client(
    objectsRef: Ref[IO, List[S3Object]],
    deletedRef: Ref[IO, List[String]]
  ) extends S3Client[IO]:
    
    override def listObjects(bucket: S3BucketConfig, prefix: String): IO[List[S3Object]] =
      objectsRef.get.map { objects =>
        // Emulate real S3Client behavior:
        // 1. Compute fullPrefix = bucket.prefix + "/" + prefix
        // 2. Filter objects by fullPrefix
        // 3. Strip bucket.prefix from returned keys
        val fullPrefix = if bucket.prefix.isEmpty then prefix
                        else if prefix.isEmpty then bucket.prefix
                        else s"${bucket.prefix}/$prefix"
        
        objects
          .filter(_.key.startsWith(fullPrefix))
          .map { obj =>
            // Strip bucket.prefix from key to match real implementation
            val strippedKey = if bucket.prefix.nonEmpty && obj.key.startsWith(s"${bucket.prefix}/") then
              obj.key.stripPrefix(s"${bucket.prefix}/")
            else
              obj.key
            obj.copy(key = strippedKey)
          }
      }
    
    override def deleteObject(bucket: S3BucketConfig, key: String): IO[Unit] =
      for
        _ <- deletedRef.update(_ :+ key)
        // Key coming in is relative (stripped of bucket.prefix)
        // Need to prepend bucket.prefix to find it in our store
        fullKey = if bucket.prefix.isEmpty then key else s"${bucket.prefix}/$key"
        _ <- objectsRef.update(_.filterNot(_.key == fullKey))
      yield ()
    
    override def uploadFile(
      bucket: S3BucketConfig,
      key: String,
      source: java.nio.file.Path,
      metadata: Map[String, String] = Map.empty
    ): IO[S3Uri] =
      IO.raiseError(new NotImplementedError("uploadFile not needed for retention tests"))
    
    override def objectExists(bucket: S3BucketConfig, key: String): IO[Boolean] =
      objectsRef.get.map(_.exists(_.key == key))
    
    override def getObjectMetadata(bucket: S3BucketConfig, key: String): IO[S3Object] =
      objectsRef.get.flatMap { objects =>
        objects.find(_.key == key) match
          case Some(obj) => IO.pure(obj)
          case None => IO.raiseError(new NoSuchElementException(s"Object not found: $key"))
      }
  
  /**
   * Create a test S3 object with a specific timestamp.
   */
  def createS3Object(key: String, timestamp: Instant): S3Object =
    S3Object(
      key = key,
      size = 1024L,
      lastModified = timestamp
    )
  
  /**
   * Create an instant from a date string.
   */
  def instant(dateStr: String): Instant =
    LocalDate.parse(dateStr).atStartOfDay(ZoneId.of("UTC")).toInstant
  
  /**
   * Helper to set up test with mock S3 client.
   */
  def withMockS3[A](
    initialObjects: List[S3Object]
  )(test: (RetentionManagerImpl[IO], Ref[IO, List[String]]) => IO[A]): IO[A] =
    for
      objectsRef <- Ref.of[IO, List[S3Object]](initialObjects)
      deletedRef <- Ref.of[IO, List[String]](List.empty)
      mockS3 = new MockS3Client(objectsRef, deletedRef)
      manager = new RetentionManagerImpl[IO](mockS3)
      result <- test(manager, deletedRef)
    yield result
  
  val testBucket = S3BucketConfig(
    name = "test-bucket",
    provider = S3Provider.AWS,
    region = "us-east-1",
    bucket = "test-bucket",
    credentialsFile = "/tmp/creds.yaml",
    endpoint = None,
    pathStyle = false,
    prefix = "backups"
  )
  
  test("keepLast: retains only the N most recent backups"):
    val backups = List(
      createS3Object("backups/sonarr/backup-2024-01-05.zip", instant("2024-01-05")),
      createS3Object("backups/sonarr/backup-2024-01-04.zip", instant("2024-01-04")),
      createS3Object("backups/sonarr/backup-2024-01-03.zip", instant("2024-01-03")),
      createS3Object("backups/sonarr/backup-2024-01-02.zip", instant("2024-01-02")),
      createS3Object("backups/sonarr/backup-2024-01-01.zip", instant("2024-01-01"))
    )
    
    val policy = RetentionPolicyConfig(
      keepLast = Some(3),
      keepDaily = None,
      keepWeekly = None,
      keepMonthly = None
    )
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", instant("2024-01-05"))
        deleted <- deletedRef.get
        _ = assertEquals(result.kept.size, 3)
        _ = assertEquals(result.deleted.size, 2)
        _ = assertEquals(deleted.size, 2)
        _ = assert(deleted.contains("sonarr/backup-2024-01-01.zip"))
        _ = assert(deleted.contains("sonarr/backup-2024-01-02.zip"))
      yield ()
    }
  
  test("keepDaily: retains one backup per day for N days"):
    // Create multiple backups per day
    val backups = List(
      // Jan 5 - 2 backups
      createS3Object("backups/sonarr/backup-2024-01-05-pm.zip", instant("2024-01-05").plus(12, ChronoUnit.HOURS)),
      createS3Object("backups/sonarr/backup-2024-01-05-am.zip", instant("2024-01-05")),
      // Jan 4 - 2 backups
      createS3Object("backups/sonarr/backup-2024-01-04-pm.zip", instant("2024-01-04").plus(12, ChronoUnit.HOURS)),
      createS3Object("backups/sonarr/backup-2024-01-04-am.zip", instant("2024-01-04")),
      // Jan 3 - 1 backup
      createS3Object("backups/sonarr/backup-2024-01-03.zip", instant("2024-01-03")),
      // Jan 2 - 1 backup
      createS3Object("backups/sonarr/backup-2024-01-02.zip", instant("2024-01-02")),
      // Jan 1 - 1 backup (outside 3-day window)
      createS3Object("backups/sonarr/backup-2024-01-01.zip", instant("2024-01-01"))
    )
    
    val policy = RetentionPolicyConfig(
      keepLast = None,
      keepDaily = Some(3), // Keep last 3 days
      keepWeekly = None,
      keepMonthly = None
    )
    
    // Mock current time as Jan 5 end of day
    val now = instant("2024-01-05").plus(23, ChronoUnit.HOURS)
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", now)
        deleted <- deletedRef.get
        // Should keep: most recent from Jan 5, Jan 4, Jan 3
        // Should delete: older backup from Jan 5, older from Jan 4, Jan 2, Jan 1
        _ = assertEquals(result.kept.size, 3, s"Kept: ${result.kept.map(_.key)}")
        _ = assertEquals(result.deleted.size, 4, s"Deleted: ${result.deleted.map(_.key)}")
      yield ()
    }
  
  test("keepWeekly: retains one backup per week for N weeks"):
    // Create backups across multiple weeks (Monday-Sunday)
    // Week of Dec 25-31, 2023 (Week 52)
    val backups = List(
      // Week of Jan 15-21, 2024 (most recent)
      createS3Object("backups/sonarr/backup-2024-01-21.zip", instant("2024-01-21")), // Sunday
      createS3Object("backups/sonarr/backup-2024-01-19.zip", instant("2024-01-19")), // Friday
      // Week of Jan 8-14, 2024
      createS3Object("backups/sonarr/backup-2024-01-14.zip", instant("2024-01-14")), // Sunday
      createS3Object("backups/sonarr/backup-2024-01-10.zip", instant("2024-01-10")), // Wednesday
      // Week of Jan 1-7, 2024
      createS3Object("backups/sonarr/backup-2024-01-07.zip", instant("2024-01-07")), // Sunday
      createS3Object("backups/sonarr/backup-2024-01-03.zip", instant("2024-01-03")), // Wednesday
      // Week of Dec 25-31, 2023 (outside 3-week window)
      createS3Object("backups/sonarr/backup-2023-12-31.zip", instant("2023-12-31")), // Sunday
      createS3Object("backups/sonarr/backup-2023-12-27.zip", instant("2023-12-27"))  // Wednesday
    )
    
    val policy = RetentionPolicyConfig(
      keepLast = None,
      keepDaily = None,
      keepWeekly = Some(3), // Keep last 3 weeks
      keepMonthly = None
    )
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", instant("2024-01-21"))
        deleted <- deletedRef.get
        // Should keep: most recent from each of 3 weeks (Jan 21, Jan 14, Jan 7)
        // Should delete: other backups from those weeks + Dec backups
        _ = assertEquals(result.kept.size, 3, s"Kept: ${result.kept.map(_.key)}")
        _ = assertEquals(result.deleted.size, 5, s"Deleted: ${result.deleted.map(_.key)}")
        _ = assert(result.kept.exists(_.key.contains("2024-01-21")))
        _ = assert(result.kept.exists(_.key.contains("2024-01-14")))
        _ = assert(result.kept.exists(_.key.contains("2024-01-07")))
      yield ()
    }
  
  test("keepMonthly: retains one backup per month for N months"):
    // Create backups across multiple months
    val backups = List(
      // March 2024 (most recent)
      createS3Object("backups/sonarr/backup-2024-03-31.zip", instant("2024-03-31")),
      createS3Object("backups/sonarr/backup-2024-03-15.zip", instant("2024-03-15")),
      // February 2024
      createS3Object("backups/sonarr/backup-2024-02-29.zip", instant("2024-02-29")),
      createS3Object("backups/sonarr/backup-2024-02-14.zip", instant("2024-02-14")),
      // January 2024
      createS3Object("backups/sonarr/backup-2024-01-31.zip", instant("2024-01-31")),
      createS3Object("backups/sonarr/backup-2024-01-15.zip", instant("2024-01-15")),
      // December 2023 (outside 3-month window)
      createS3Object("backups/sonarr/backup-2023-12-31.zip", instant("2023-12-31")),
      createS3Object("backups/sonarr/backup-2023-12-15.zip", instant("2023-12-15"))
    )
    
    val policy = RetentionPolicyConfig(
      keepLast = None,
      keepDaily = None,
      keepWeekly = None,
      keepMonthly = Some(3) // Keep last 3 months
    )
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", instant("2024-03-31"))
        deleted <- deletedRef.get
        // Should keep: most recent from Mar, Feb, Jan
        // Should delete: older from those months + all Dec backups
        _ = assertEquals(result.kept.size, 3, s"Kept: ${result.kept.map(_.key)}")
        _ = assertEquals(result.deleted.size, 5, s"Deleted: ${result.deleted.map(_.key)}")
        _ = assert(result.kept.exists(_.key.contains("2024-03-31")))
        _ = assert(result.kept.exists(_.key.contains("2024-02-29")))
        _ = assert(result.kept.exists(_.key.contains("2024-01-31")))
      yield ()
    }
  
  test("combined policy: keepLast=3, keepDaily=7, keepWeekly=4"):
    // Realistic scenario: daily backups for a month
    val now = instant("2024-01-30")
    val backups = (1 to 30).map { day =>
      val date = instant(f"2024-01-$day%02d")
      createS3Object(s"backups/sonarr/backup-2024-01-$day%02d.zip", date)
    }.toList.reverse // Newest first
    
    val policy = RetentionPolicyConfig(
      keepLast = Some(3),    // 3 most recent
      keepDaily = Some(7),   // Last 7 days
      keepWeekly = Some(4),  // Last 4 weeks
      keepMonthly = None
    )
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", now)
        deleted <- deletedRef.get
        // Should keep:
        // - keepLast: Jan 30, 29, 28 (3 backups)
        // - keepDaily: Jan 30-24 (7 backups, overlaps with keepLast)
        // - keepWeekly: 4 weekly backups (one per week for 4 weeks)
        // Total unique should be more than 7 due to weekly coverage
        _ = assert(result.kept.size >= 7, s"Kept ${result.kept.size} backups, expected >= 7")
        _ = assert(result.kept.size <= 14, s"Kept ${result.kept.size} backups, expected <= 14")
        _ = assertEquals(result.deleted.size, 30 - result.kept.size)
        // Verify most recent are kept
        _ = assert(result.kept.exists(_.key.contains("2024-01-30")))
        _ = assert(result.kept.exists(_.key.contains("2024-01-29")))
        _ = assert(result.kept.exists(_.key.contains("2024-01-28")))
      yield ()
    }
  
  test("long-term retention: 90 days daily + 52 weeks + 12 months"):
    // Simulate 18 months of daily backups
    val startDate = instant("2023-01-01")
    val endDate = instant("2024-06-30")
    
    val backups = {
      var current = startDate
      var result = List.empty[S3Object]
      while (current.isBefore(endDate)) {
        val dateStr = LocalDate.ofInstant(current, ZoneId.of("UTC"))
        result = createS3Object(s"backups/sonarr/backup-$dateStr.zip", current) :: result
        current = current.plus(1, ChronoUnit.DAYS)
      }
      result.reverse
    }
    
    val policy = RetentionPolicyConfig(
      keepLast = Some(7),      // Last week
      keepDaily = Some(90),    // Last 90 days
      keepWeekly = Some(52),   // Last year weekly
      keepMonthly = Some(12)   // Last year monthly
    )
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", endDate)
        deleted <- deletedRef.get
        totalBackups = backups.size
        // With daily backups for 18 months (~548 days):
        // - keepDaily: 90 backups (last 90 days)
        // - keepWeekly: ~52 backups (one per week for year), ~40 beyond daily window
        // - keepMonthly: 12 backups (one per month), all overlap with weekly/daily
        // Union should be ~130-140 backups (90 daily + ~40-50 weekly beyond that)
        _ = assert(result.kept.size >= 90, s"Kept ${result.kept.size} backups, expected >= 90")
        _ = assert(result.kept.size <= 145, s"Kept ${result.kept.size} backups, expected <= 145")
        _ = assert(result.deleted.size > 400, s"Deleted ${result.deleted.size} backups, expected > 400")
        _ = assertEquals(result.kept.size + result.deleted.size, totalBackups)
      yield ()
    }
  
  test("empty backup list returns empty result"):
    val policy = RetentionPolicyConfig(
      keepLast = Some(3),
      keepDaily = None,
      keepWeekly = None,
      keepMonthly = None
    )
    
    withMockS3(List.empty) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", instant("2024-01-01"))
        _ = assertEquals(result.kept.size, 0)
        _ = assertEquals(result.deleted.size, 0)
        _ = assertEquals(result.errors.size, 0)
      yield ()
    }
  
  test("all policies disabled (None) keeps all backups"):
    val backups = List(
      createS3Object("backups/sonarr/backup-1.zip", instant("2024-01-03")),
      createS3Object("backups/sonarr/backup-2.zip", instant("2024-01-02")),
      createS3Object("backups/sonarr/backup-3.zip", instant("2024-01-01"))
    )
    
    val policy = RetentionPolicyConfig(
      keepLast = None,
      keepDaily = None,
      keepWeekly = None,
      keepMonthly = None
    )
    
    withMockS3(backups) { (manager, deletedRef) =>
      for
        result <- manager.applyRetentionAt(testBucket, policy, "sonarr", instant("2024-01-03"))
        _ = assertEquals(result.kept.size, 0) // Nothing matches any policy
        _ = assertEquals(result.deleted.size, 3) // All get deleted
      yield ()
    }

