package com.backuparr.integration

import cats.effect.{IO, Resource}
import cats.syntax.all.*
import com.backuparr.algebras.{ArrClient, BackupManager, RetentionManager, S3Client, Scheduler}
import com.backuparr.config.{ArrInstanceConfig, RetentionPolicyConfig, S3BucketConfig, S3Credentials}
import com.backuparr.domain.{ArrType, BackupStatus, S3Provider}
import com.backuparr.impl.{BackupManagerImpl, RetentionManagerImpl, S3ClientAwsSdk, SchedulerImpl}
import munit.CatsEffectSuite
import org.http4s.ember.client.EmberClientBuilder
import org.http4s.client.Client
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger

import java.nio.file.{Files, Paths}
import scala.concurrent.duration.*
import scala.util.control.NonFatal

/**
 * End-to-end integration test using the complete docker-compose setup.
 * 
 * This test verifies the complete backup system:
 * 1. Scheduler triggers backups for multiple instances
 * 2. BackupManager orchestrates the workflow
 * 3. Backups are uploaded to MinIO (S3)
 * 4. Retention policies are applied
 * 5. Verify backups exist in S3 with correct metadata
 * 6. Verify retention policy deleted old backups
 * 
 * Requirements:
 * - docker-compose -f docker-compose.test.yml up -d
 * - docker exec backuparr-test-minio mc mb local/backups
 * - API keys set in environment
 * 
 * Run with: INTEGRATION=true sbt "testOnly com.backuparr.integration.E2EIntegrationSpec"
 */
class E2EIntegrationSpec extends CatsEffectSuite:
  
  val integrationTag = new munit.Tag("integration")
  
  // Increase timeout for E2E tests (default is 30s, we need more for multiple backups)
  override val munitTimeout = 120.seconds
  
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
  
  // MinIO configuration
  val testBucket = S3BucketConfig(
    name = "test-bucket",
    provider = S3Provider.MinIO,
    region = "us-east-1",
    bucket = "backups",
    credentialsFile = "/tmp/minio-creds-e2e.yaml",
    endpoint = Some("http://localhost:9000"),
    pathStyle = true,
    prefix = "e2e-test"
  )
  
  val minioCredentials = S3Credentials(
    accessKeyId = "minioadmin",
    secretAccessKey = "minioadmin"
  )
  
  override def beforeAll(): Unit =
    super.beforeAll()
    
    // Create credentials file
    val credsContent = s"""accessKeyId: ${minioCredentials.accessKeyId}
                          |secretAccessKey: ${minioCredentials.secretAccessKey}
                          |""".stripMargin
    Files.writeString(Paths.get(testBucket.credentialsFile), credsContent)
    
    println("========================================")
    println("End-to-End Integration Test")
    println("========================================")
    println("Testing complete system with Scheduler")
    println("========================================")
  
  override def afterAll(): Unit =
    try
      val credsPath = Paths.get(testBucket.credentialsFile)
      if Files.exists(credsPath) then
        Files.delete(credsPath)
    catch
      case NonFatal(_) => // Ignore
    
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
   * Helper to get instance config from environment.
   */
  def getInstanceConfig(
    name: String,
    arrType: ArrType,
    url: String,
    envVar: String,
    retentionPolicy: RetentionPolicyConfig
  ): IO[ArrInstanceConfig] =
    IO.delay(sys.props.get(envVar).orElse(sys.env.get(envVar))) flatMap {
      case Some(apiKey) =>
        // Write API key to a temp file
        val tempFile = Files.createTempFile(s"backuparr-test-$name-", ".key")
        tempFile.toFile.deleteOnExit()
        IO.delay(Files.writeString(tempFile, apiKey)).as:
          ArrInstanceConfig(
            name = name,
            arrType = arrType,
            url = url,
            apiKeyFile = tempFile.toString,
            schedule = "* * * * *", // Every minute (for testing)
            s3BucketName = testBucket.name,
            retentionPolicy = retentionPolicy
          )
      case None =>
        IO.raiseError(
          new RuntimeException(s"API key not found for $envVar")
        )
    }
  
  /**
   * Clean up all backups for an instance from S3.
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
  
  /**
   * Verify backups exist in S3 and return their keys.
   */
  def verifyBackupsInS3(instanceName: String): IO[List[String]] =
    for
      prefix <- IO.pure(s"$instanceName/")
      objects <- s3ClientFixture().listObjects(testBucket, prefix)
      keys = objects.map(_.key).sorted
      _ <- IO.println(s"Found ${keys.size} backups for $instanceName: ${keys.mkString(", ")}")
    yield keys
  
  /**
   * Manually trigger a backup for an instance with retry logic.
   * Some *arr applications (especially Lidarr/Prowlarr) may fail on the first
   * attempt if their backup directories aren't fully initialized.
   */
  def triggerBackup(
    manager: BackupManager[IO], 
    instance: ArrInstanceConfig,
    maxAttempts: Int = 3
  ): IO[Unit] =
    def attempt(attemptNum: Int): IO[Unit] =
      manager.executeBackup(instance).flatMap { result =>
        if result.isSuccessful then
          IO.println(s"✓ Backup completed for ${instance.name}: ${result.s3Uri}")
        else if attemptNum < maxAttempts then
          IO.println(s"✗ Backup failed for ${instance.name}: ${result.status}, retrying (attempt ${attemptNum + 1}/$maxAttempts)...") *>
          IO.sleep(3.seconds) *>
          attempt(attemptNum + 1)
        else
          IO.raiseError(new AssertionError(
            s"Backup failed for ${instance.name} after $maxAttempts attempts. Status: ${result.status}"
          ))
      }.handleErrorWith { error =>
        if attemptNum < maxAttempts then
          IO.println(s"Backup execution error for ${instance.name}: ${error.getMessage}, retrying (attempt ${attemptNum + 1}/$maxAttempts)...") *>
          IO.sleep(3.seconds) *>
          attempt(attemptNum + 1)
        else
          IO.raiseError(error)
      }
    
    IO.println(s"Triggering backup for ${instance.name}...") *> attempt(1)
  
  test("E2E - Multiple instances with retention policy".tag(integrationTag)) {
    for
      // Configure multiple instances with different retention policies
      sonarrConfig <- getInstanceConfig(
        "sonarr-e2e",
        ArrType.Sonarr,
        "http://localhost:8989/sonarr",
        "SONARR_API_KEY",
        RetentionPolicyConfig(keepLast = Some(2))
      )
      
      radarrConfig <- getInstanceConfig(
        "radarr-e2e",
        ArrType.Radarr,
        "http://localhost:7878/radarr",
        "RADARR_API_KEY",
        RetentionPolicyConfig(keepLast = Some(2))
      )
      
      instances = List(sonarrConfig, radarrConfig) // Reduced to 2 instances for speed
      
      // Clean up any existing backups
      _ <- instances.traverse_(config => cleanupS3Backups(config.name))
      
      // Create the complete backup system
      arrClient <- ArrClient.make[IO](httpClientFixture(), loggerFixture())
      retentionManager <- RetentionManager.make[IO](s3ClientFixture())
      backupManager <- BackupManagerImpl.make[IO](
        arrClient,
        s3ClientFixture(),
        retentionManager,
        Map(testBucket.name -> testBucket)
      )
      
      _ <- IO.println("========================================")
      _ <- IO.println("Creating backups to test retention (3 backups per instance)")
      _ <- IO.println("========================================")
      
      // Create 3 backups for each instance to test retention (keepLast=2 will delete 1)
      // Run backups concurrently for speed
      _ <- instances.parTraverse { instance =>
        List.range(0, 3).traverse { i =>
          for
            _ <- IO.println(s"Creating backup ${i + 1}/3 for ${instance.name}")
            _ <- triggerBackup(backupManager, instance, maxAttempts = 3) // Increase retry attempts
            _ <- IO.sleep(1.second) // Reduced delay - just enough for unique timestamps
          yield ()
        }
      }
      
      _ <- IO.println("========================================")
      _ <- IO.println("Verifying retention policies applied")
      _ <- IO.println("========================================")
      
      // Verify retention policy was applied correctly (should have 2 backups each)
      sonarrBackups <- verifyBackupsInS3(sonarrConfig.name)
      _ <- IO(assertEquals(sonarrBackups.size, 2, s"Sonarr should have 2 backups after retention"))
      
      radarrBackups <- verifyBackupsInS3(radarrConfig.name)
      _ <- IO(assertEquals(radarrBackups.size, 2, s"Radarr should have 2 backups after retention"))
      
      _ <- IO.println("========================================")
      _ <- IO.println("Verifying backup metadata")
      _ <- IO.println("========================================")
      
      // Verify metadata on one of the backups
      _ <- sonarrBackups.headOption match
        case Some(key) =>
          for
            obj <- s3ClientFixture().getObjectMetadata(testBucket, key)
            _ <- IO.println(s"Backup metadata: ${obj.metadata}")
            _ <- IO(assert(obj.metadata.contains("instance-name"), "Should have instance-name metadata"))
            _ <- IO(assert(obj.metadata.contains("arr-type"), "Should have arr-type metadata"))
            _ <- IO(assertEquals(obj.metadata.get("instance-name"), Some(sonarrConfig.name)))
            _ <- IO(assertEquals(obj.metadata.get("arr-type"), Some("Sonarr")))
          yield ()
        case None =>
          IO.raiseError(new RuntimeException("No backups found for Sonarr"))
      
      _ <- IO.println("Test complete! ✅")
      
      // Cleanup
      _ <- instances.traverse_(config => cleanupS3Backups(config.name))
      
    yield ()
  }
  
  test("E2E - Scheduler-driven backups (short duration)".tag(integrationTag)) {
    // This test verifies the scheduler can orchestrate backups
    // We use a very short schedule for testing (every 2 minutes simulated with manual triggers)
    
    for
      // Configure instances
      sonarrConfig <- getInstanceConfig(
        "sonarr-scheduler",
        ArrType.Sonarr,
        "http://localhost:8989/sonarr",
        "SONARR_API_KEY",
        RetentionPolicyConfig(keepLast = Some(2))
      )
      
      radarrConfig <- getInstanceConfig(
        "radarr-scheduler",
        ArrType.Radarr,
        "http://localhost:7878/radarr",
        "RADARR_API_KEY",
        RetentionPolicyConfig(keepLast = Some(2))
      )
      
      instances = List(sonarrConfig, radarrConfig)
      
      _ <- instances.traverse_(config => cleanupS3Backups(config.name))
      
      // Create backup system
      arrClient <- ArrClient.make[IO](httpClientFixture(), loggerFixture())
      retentionManager <- RetentionManager.make[IO](s3ClientFixture())
      backupManager <- BackupManagerImpl.make[IO](
        arrClient,
        s3ClientFixture(),
        retentionManager,
        Map(testBucket.name -> testBucket)
      )
      
      // Create scheduler
      scheduler <- SchedulerImpl.make[IO](backupManager, instances, maxConcurrent = 2)
      
      _ <- IO.println("========================================")
      _ <- IO.println("Testing Scheduler-driven backups")
      _ <- IO.println("========================================")
      
      // Note: We can't actually run the scheduler for minutes in a test
      // Instead, we verify the scheduler can be created and manually trigger backups
      // to simulate what the scheduler would do
      
      _ <- IO.println("Simulating 3 scheduled backup cycles...")
      
      // Simulate 3 scheduler cycles
      _ <- List.range(0, 3).traverse { i =>
        for
          _ <- IO.println(s"Cycle ${i + 1}/3")
          _ <- instances.parTraverse { instance =>
            backupManager.executeBackup(instance)
          }
          _ <- IO.sleep(2.seconds) // Delay between cycles
        yield ()
      }
      
      // Verify retention worked (should keep only last 2)
      sonarrBackups <- verifyBackupsInS3(sonarrConfig.name)
      radarrBackups <- verifyBackupsInS3(radarrConfig.name)
      
      _ <- IO(assertEquals(sonarrBackups.size, 2, "Sonarr should have 2 backups after retention"))
      _ <- IO(assertEquals(radarrBackups.size, 2, "Radarr should have 2 backups after retention"))
      
      // Verify scheduler can start and stop gracefully
      _ <- IO.println("Testing scheduler start/stop...")
      schedulerFiber <- scheduler.start.start
      _ <- IO.sleep(1.second) // Let it run briefly
      _ <- scheduler.stop
      _ <- schedulerFiber.cancel
      _ <- IO.println("Scheduler stopped successfully")
      
      // Cleanup
      _ <- instances.traverse_(config => cleanupS3Backups(config.name))
      
      _ <- IO.println("Scheduler test complete! ✅")
      
    yield ()
  }
  
  test("E2E - Different retention policies".tag(integrationTag)) {
    // Test various retention policy combinations
    
    for
      // Instance with keepLast only
      keepLastConfig <- getInstanceConfig(
        "test-keeplast",
        ArrType.Sonarr,
        "http://localhost:8989/sonarr",
        "SONARR_API_KEY",
        RetentionPolicyConfig(keepLast = Some(3))
      )
      
      // Instance with combined policies
      combinedConfig <- getInstanceConfig(
        "test-combined",
        ArrType.Radarr,
        "http://localhost:7878/radarr",
        "RADARR_API_KEY",
        RetentionPolicyConfig(
          keepLast = Some(2),
          keepDaily = Some(7)
        )
      )
      
      instances = List(keepLastConfig, combinedConfig)
      _ <- instances.traverse_(config => cleanupS3Backups(config.name))
      
      arrClient <- ArrClient.make[IO](httpClientFixture(), loggerFixture())
      retentionManager <- RetentionManager.make[IO](s3ClientFixture())
      backupManager <- BackupManagerImpl.make[IO](
        arrClient,
        s3ClientFixture(),
        retentionManager,
        Map(testBucket.name -> testBucket)
      )
      
      _ <- IO.println("Testing different retention policies...")
      
      // Create 5 backups for each
      _ <- instances.traverse_ { instance =>
        List.range(0, 5).traverse_ { _ =>
          backupManager.executeBackup(instance) >> IO.sleep(1.second)
        }
      }
      
      // Verify keepLast policy
      keepLastBackups <- verifyBackupsInS3(keepLastConfig.name)
      _ <- IO(assertEquals(keepLastBackups.size, 3, "keepLast=3 should keep 3 backups"))
      
      // Verify combined policy (should keep at least 2, potentially more for daily)
      combinedBackups <- verifyBackupsInS3(combinedConfig.name)
      _ <- IO(assert(combinedBackups.size >= 2, s"Combined policy should keep at least 2 backups, got ${combinedBackups.size}"))
      
      _ <- instances.traverse_(config => cleanupS3Backups(config.name))
      
      _ <- IO.println("Retention policy test complete! ✅")
      
    yield ()
  }
