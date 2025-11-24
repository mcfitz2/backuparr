package com.backuparr.integration

import cats.effect.{IO, Resource}
import cats.syntax.all.*
import com.backuparr.algebras.{ArrClient, BackupManager, RetentionManager, S3Client}
import com.backuparr.config.{ArrInstanceConfig, RetentionPolicyConfig, S3BucketConfig, S3Credentials}
import com.backuparr.domain.{ArrType, BackupResult, BackupStatus, S3Provider, S3Uri}
import com.backuparr.impl.{BackupManagerImpl, RetentionManagerImpl, S3ClientAwsSdk}
import munit.CatsEffectSuite
import org.http4s.ember.client.EmberClientBuilder
import org.http4s.client.Client
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger

import java.nio.file.{Files, Paths}
import scala.concurrent.duration.*
import scala.util.control.NonFatal

/**
 * Integration tests for BackupManager using real docker-compose setup.
 * 
 * These tests require:
 * 1. Docker containers running (start with docker-compose -f docker-compose.test.yml up -d)
 * 2. MinIO bucket created: docker exec backuparr-test-minio mc mb local/backups
 * 3. *arr instances configured with API keys
 * 
 * The tests verify the complete backup workflow:
 * - Request backup from *arr instance
 * - Download backup file
 * - Upload to MinIO (S3-compatible storage)
 * - Apply retention policy
 * - Verify backup exists in S3
 * 
 * Run these tests with: sbt integrationTest
 */
class BackupManagerIntegrationSpec extends CatsEffectSuite:
  
  // Skip if not running integration tests
  val integrationTag = new munit.Tag("integration")
  
  override def munitTestTransforms = super.munitTestTransforms ++ List(
    new TestTransform("Integration Test",
      test => {
        val integrationEnabled = sys.props.get("INTEGRATION").orElse(sys.env.get("INTEGRATION")).isDefined
        if test.tags.contains(integrationTag) && !integrationEnabled then
          test.tag(munit.Ignore)
        else
          test
      }
    )
  )
  
  // MinIO configuration (matches docker-compose.test.yml)
  val testBucket = S3BucketConfig(
    name = "test-bucket",
    provider = S3Provider.MinIO,
    region = "us-east-1",
    bucket = "backups",
    credentialsFile = "/tmp/minio-creds-backup-test.yaml",
    endpoint = Some("http://localhost:9000"),
    pathStyle = true,
    prefix = "test"
  )
  
  // MinIO credentials
  val minioCredentials = S3Credentials(
    accessKeyId = "minioadmin",
    secretAccessKey = "minioadmin"
  )
  
  // Setup: Create credentials file before all tests
  override def beforeAll(): Unit =
    super.beforeAll()
    
    // Create credentials file
    val credsContent = s"""accessKeyId: ${minioCredentials.accessKeyId}
                          |secretAccessKey: ${minioCredentials.secretAccessKey}
                          |""".stripMargin
    Files.writeString(Paths.get(testBucket.credentialsFile), credsContent)
    
    println("========================================")
    println("BackupManager Integration Tests")
    println("========================================")
    println("Requirements:")
    println("1. docker-compose -f docker-compose.test.yml up -d")
    println("2. docker exec backuparr-test-minio mc mb local/backups")
    println("3. Set API keys: SONARR_API_KEY, RADARR_API_KEY, etc.")
    println("========================================")
  
  // Cleanup: Remove credentials file after all tests
  override def afterAll(): Unit =
    try
      val credsPath = Paths.get(testBucket.credentialsFile)
      if Files.exists(credsPath) then
        Files.delete(credsPath)
    catch
      case NonFatal(_) => // Ignore cleanup errors
    
    super.afterAll()
  
  // Shared fixtures
  val httpClientFixture: Fixture[Client[IO]] = ResourceSuiteLocalFixture(
    "http-client",
    EmberClientBuilder.default[IO].build
  )
  
  val loggerFixture: Fixture[Logger[IO]] = ResourceSuiteLocalFixture(
    "logger",
    Resource.eval(Slf4jLogger.create[IO])
  )
  
  val s3ClientFixture: Fixture[S3Client[IO]] = ResourceSuiteLocalFixture(
    "s3-client",
    S3ClientAwsSdk.resource[IO](testBucket)
  )
  
  override def munitFixtures = List(httpClientFixture, loggerFixture, s3ClientFixture)
  
  /**
   * Helper to get instance config with API key from environment.
   */
  def getInstanceConfig(
    name: String,
    arrType: ArrType,
    url: String,
    envVar: String
  ): IO[ArrInstanceConfig] =
    IO.delay(sys.props.get(envVar).orElse(sys.env.get(envVar))) flatMap {
      case Some(apiKey) =>
        IO.pure(ArrInstanceConfig(
          name = name,
          arrType = arrType,
          url = url,
          apiKey = Some(apiKey),
          schedule = "0 2 * * *",
          s3BucketName = testBucket.name,
          retentionPolicy = RetentionPolicyConfig(keepLast = Some(5))
        ))
      case None =>
        IO.raiseError(
          new RuntimeException(
            s"API key not found for $envVar. Set environment variable $envVar to your ${arrType} API key."
          )
        )
    }
  
  /**
   * Create a BackupManager instance for testing.
   */
  def makeBackupManager: IO[BackupManager[IO]] =
    for
      arrClient <- ArrClient.make[IO](httpClientFixture(), loggerFixture())
      retentionManager <- RetentionManager.make[IO](s3ClientFixture())
      backupManager <- BackupManagerImpl.make[IO](
        arrClient,
        s3ClientFixture(),
        retentionManager,
        Map(testBucket.name -> testBucket)
      )
    yield backupManager
  
  /**
   * Helper to retry backup execution for flaky *arr applications.
   * Some apps (Lidarr/Prowlarr) may fail initially if backup directories aren't initialized.
   */
  def retryBackup(
    manager: BackupManager[IO],
    config: ArrInstanceConfig,
    maxAttempts: Int = 2
  ): IO[BackupResult] =
    def attempt(attemptNum: Int): IO[BackupResult] =
      for
        result <- manager.executeBackup(config)
        finalResult <- result.isSuccessful match
          case true => IO.pure(result)
          case false if attemptNum < maxAttempts =>
            IO.println(s"Backup failed for ${config.name} (attempt $attemptNum/$maxAttempts), retrying...") *>
            IO.sleep(2.seconds) *>
            attempt(attemptNum + 1)
          case false => IO.pure(result)
      yield finalResult
    
    attempt(1)
  
  /**
   * Helper to verify a backup exists in S3.
   */
  def verifyBackupInS3(instanceName: String): IO[List[String]] =
    for
      prefix <- IO.pure(s"$instanceName/")
      objects <- s3ClientFixture().listObjects(testBucket, prefix)
      keys = objects.map(_.key)
      _ <- IO.println(s"Found ${keys.size} backups in S3 for $instanceName: ${keys.mkString(", ")}")
    yield keys
  
  /**
   * Helper to clean up test backups from S3.
   */
  def cleanupS3Backups(instanceName: String): IO[Unit] =
    for
      prefix <- IO.pure(s"$instanceName/")
      objects <- s3ClientFixture().listObjects(testBucket, prefix)
      _ <- objects.traverse_ { obj =>
        s3ClientFixture().deleteObject(testBucket, obj.key).attempt.void
      }
      _ <- IO.println(s"Cleaned up ${objects.size} backups for $instanceName")
    yield ()
  
  test("Sonarr - complete backup workflow".tag(integrationTag)) {
    for
      // Get configuration
      config <- getInstanceConfig("sonarr-test", ArrType.Sonarr, "http://localhost:8989", "SONARR_API_KEY")
      
      // Clean up any existing backups
      _ <- cleanupS3Backups(config.name)
      
      // Create backup manager
      manager <- makeBackupManager
      
      // Execute backup
      _ <- IO.println("Starting Sonarr backup...")
      result <- manager.executeBackup(config)
      
      // Verify result
      _ <- IO(assert(result.isSuccessful, s"Backup should succeed, got status: ${result.status}"))
      _ <- IO(assert(result.s3Uri.isDefined, "S3 URI should be present"))
      _ <- IO.println(s"Backup completed: ${result.s3Uri.get}")
      
      // Verify backup exists in S3
      backups <- verifyBackupInS3(config.name)
      _ <- IO(assert(backups.nonEmpty, "At least one backup should exist in S3"))
      
      // Verify status tracking
      status <- manager.getStatus
      _ <- IO(assert(status.contains(config.name), "Status should be tracked for instance"))
      
      // Cleanup
      _ <- cleanupS3Backups(config.name)
      
    yield ()
  }
  
  test("Radarr - complete backup workflow".tag(integrationTag)) {
    for
      config <- getInstanceConfig("radarr-test", ArrType.Radarr, "http://localhost:7878", "RADARR_API_KEY")
      _ <- cleanupS3Backups(config.name)
      
      manager <- makeBackupManager
      
      _ <- IO.println("Starting Radarr backup...")
      result <- manager.executeBackup(config)
      
      _ <- IO(assert(result.isSuccessful, s"Backup should succeed, got status: ${result.status}"))
      _ <- IO(assert(result.s3Uri.isDefined, "S3 URI should be present"))
      
      backups <- verifyBackupInS3(config.name)
      _ <- IO(assert(backups.nonEmpty, "At least one backup should exist in S3"))
      
      _ <- cleanupS3Backups(config.name)
      
    yield ()
  }
  
  test("Lidarr - complete backup workflow".tag(integrationTag)) {
    for
      config <- getInstanceConfig("lidarr-test", ArrType.Lidarr, "http://localhost:8686", "LIDARR_API_KEY")
      _ <- cleanupS3Backups(config.name)
      
      manager <- makeBackupManager
      
      _ <- IO.println("Starting Lidarr backup...")
      
      // Retry logic for flaky Lidarr backups (backup directory may not be initialized immediately)
      result <- retryBackup(manager, config, maxAttempts = 2)
      
      _ <- IO(assert(result.isSuccessful, s"Backup should succeed, got status: ${result.status}"))
      _ <- IO(assert(result.s3Uri.isDefined, "S3 URI should be present"))
      
      backups <- verifyBackupInS3(config.name)
      _ <- IO(assert(backups.nonEmpty, "At least one backup should exist in S3"))
      
      _ <- cleanupS3Backups(config.name)
      
    yield ()
  }
  
  test("Prowlarr - complete backup workflow".tag(integrationTag)) {
    for
      config <- getInstanceConfig("prowlarr-test", ArrType.Prowlarr, "http://localhost:9696", "PROWLARR_API_KEY")
      _ <- cleanupS3Backups(config.name)
      
      manager <- makeBackupManager
      
      _ <- IO.println("Starting Prowlarr backup...")
      
      // Retry logic for flaky Prowlarr backups (backup directory may not be initialized immediately)
      result <- retryBackup(manager, config, maxAttempts = 2)
      
      _ <- IO(assert(result.isSuccessful, s"Backup should succeed, got status: ${result.status}"))
      _ <- IO(assert(result.s3Uri.isDefined, "S3 URI should be present"))
      
      backups <- verifyBackupInS3(config.name)
      _ <- IO(assert(backups.nonEmpty, "At least one backup should exist in S3"))
      
      _ <- cleanupS3Backups(config.name)
      
    yield ()
  }
  
  test("Retention policy - keep last N backups".tag(integrationTag)) {
    for
      // Create config with keepLast = 3
      config <- getInstanceConfig("sonarr-retention", ArrType.Sonarr, "http://localhost:8989", "SONARR_API_KEY")
      configWithRetention = config.copy(
        retentionPolicy = RetentionPolicyConfig(keepLast = Some(3))
      )
      
      _ <- cleanupS3Backups(configWithRetention.name)
      
      manager <- makeBackupManager
      
      // Create 5 backups
      _ <- IO.println("Creating 5 backups to test retention...")
      results <- List.range(0, 5).traverse { i =>
        for
          _ <- IO.println(s"Creating backup ${i + 1}/5...")
          result <- manager.executeBackup(configWithRetention)
          _ <- IO.sleep(2.seconds) // Wait between backups to ensure different timestamps
        yield result
      }
      
      // Verify all backups succeeded
      _ <- IO(assert(results.forall(_.isSuccessful), "All backups should succeed"))
      
      // Check how many backups exist (should be 3 due to retention policy)
      backups <- verifyBackupInS3(configWithRetention.name)
      _ <- IO.println(s"After retention: ${backups.size} backups remain")
      _ <- IO(assert(backups.size == 3, s"Should have exactly 3 backups after retention, got ${backups.size}"))
      
      _ <- cleanupS3Backups(configWithRetention.name)
      
    yield ()
  }
  
  test("Multiple instances - concurrent backups".tag(integrationTag)) {
    for
      // Get configs for multiple instances
      sonarrConfig <- getInstanceConfig("sonarr-concurrent", ArrType.Sonarr, "http://localhost:8989", "SONARR_API_KEY")
      radarrConfig <- getInstanceConfig("radarr-concurrent", ArrType.Radarr, "http://localhost:7878", "RADARR_API_KEY")
      
      _ <- cleanupS3Backups(sonarrConfig.name)
      _ <- cleanupS3Backups(radarrConfig.name)
      
      manager <- makeBackupManager
      
      // Execute backups concurrently
      _ <- IO.println("Starting concurrent backups...")
      results <- List(sonarrConfig, radarrConfig).parTraverse { config =>
        manager.executeBackup(config)
      }
      
      // Verify both succeeded
      _ <- IO(assert(results.forall(_.isSuccessful), "All concurrent backups should succeed"))
      
      // Verify both exist in S3
      sonarrBackups <- verifyBackupInS3(sonarrConfig.name)
      radarrBackups <- verifyBackupInS3(radarrConfig.name)
      
      _ <- IO(assert(sonarrBackups.nonEmpty, "Sonarr backup should exist"))
      _ <- IO(assert(radarrBackups.nonEmpty, "Radarr backup should exist"))
      
      _ <- cleanupS3Backups(sonarrConfig.name)
      _ <- cleanupS3Backups(radarrConfig.name)
      
    yield ()
  }
  
  test("Status tracking - multiple backups".tag(integrationTag)) {
    for
      config <- getInstanceConfig("sonarr-status", ArrType.Sonarr, "http://localhost:8989", "SONARR_API_KEY")
      _ <- cleanupS3Backups(config.name)
      
      manager <- makeBackupManager
      
      // Start backup
      _ <- IO.println("Starting backup to test status tracking...")
      resultFiber <- manager.executeBackup(config).start
      
      // Check status while running (might catch it in progress)
      _ <- IO.sleep(1.second)
      statusDuring <- manager.getStatus
      _ <- IO.println(s"Status during backup: ${statusDuring.get(config.name)}")
      
      // Wait for completion
      result <- resultFiber.joinWithNever
      
      // Check final status
      statusAfter <- manager.getStatus
      _ <- IO.println(s"Status after backup: ${statusAfter.get(config.name)}")
      _ <- IO(assert(statusAfter.contains(config.name), "Status should be tracked"))
      
      _ <- cleanupS3Backups(config.name)
      
    yield ()
  }
