package com.backuparr.integration

import cats.effect.{IO, Resource}
import munit.CatsEffectSuite
import org.http4s.ember.client.EmberClientBuilder
import org.http4s.client.Client
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger
import scala.concurrent.duration.*
import java.nio.file.{Files => JFiles, Paths}
import fs2.io.file.Files

import com.backuparr.algebras.ArrClient
import com.backuparr.config.ArrInstanceConfig
import com.backuparr.domain.{ArrType, BackupId, BackupStatus}

/**
 * Integration tests for ArrClient against real *arr instances.
 * 
 * These tests require running *arr instances. You can start them with:
 * 
 *   docker-compose -f docker-compose.test.yml up -d
 * 
 * After starting the containers, you need to:
 * 1. Access each instance's web UI
 * 2. Complete the initial setup wizard
 * 3. Get the API key from Settings > General
 * 4. Set the API keys in environment variables:
 *    - SONARR_API_KEY
 *    - RADARR_API_KEY
 *    - LIDARR_API_KEY
 * 
 * To run only integration tests:
 * 
 *   sbt "testOnly com.backuparr.integration.*"
 * 
 * To skip integration tests:
 * 
 *   sbt "testOnly com.backuparr.* -- -l integration"
 */
class ArrClientIntegrationSpec extends CatsEffectSuite:
  
  // Fixtures for HTTP client and logger
  // These are created once and shared across all tests
  
  val httpClientFixture: Fixture[Client[IO]] = ResourceSuiteLocalFixture(
    "http-client",
    EmberClientBuilder.default[IO].build
  )
  
  val loggerFixture: Fixture[Logger[IO]] = ResourceSuiteLocalFixture(
    "logger",
    Resource.eval(Slf4jLogger.create[IO])
  )
  
  override def munitFixtures = List(httpClientFixture, loggerFixture)
  
  // Helper to create an ArrClient
  def makeClient: IO[ArrClient[IO]] =
    ArrClient.make[IO](httpClientFixture(), loggerFixture())
  
  /**
   * Helper to get config for a test instance.
   * Reads API key from environment variable and writes it to a temp file.
   */
  def getInstanceConfig(
    name: String,
    arrType: ArrType,
    url: String,
    envVar: String
  ): IO[ArrInstanceConfig] =
    // Try sys.props first (set by sbt task), then fall back to sys.env
    IO.delay(sys.props.get(envVar).orElse(sys.env.get(envVar))) flatMap:
      case Some(apiKey) =>
        // Write API key to a temp file
        val tempFile = JFiles.createTempFile(s"backuparr-test-$name-", ".key")
        tempFile.toFile.deleteOnExit()
        IO.delay(JFiles.writeString(tempFile, apiKey)).as:
          ArrInstanceConfig(
            name = name,
            arrType = arrType,
            url = url,
            apiKeyFile = tempFile.toString,
            schedule = "0 2 * * *",
            s3BucketName = "test-bucket",
            retentionPolicy = com.backuparr.config.RetentionPolicyConfig(keepLast = Some(7))
          )
      case None =>
        IO.raiseError(
          new RuntimeException(
            s"API key not found for $envVar. When running via 'sbt integrationTest', " +
            s"API keys are automatically extracted from containers. When running manually, " +
            s"set environment variable $envVar to your ${arrType} API key."
          )
        )
  
  // Tag for integration tests - create a custom tag
  val integrationTag = new munit.Tag("integration")
  
  // Transform to ignore tests unless INTEGRATION environment variable or system property is set
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
  
  test("Sonarr - request backup and check status".tag(integrationTag)) {
    for
      config <- getInstanceConfig("sonarr-test", ArrType.Sonarr, "http://localhost:8989/sonarr", "SONARR_API_KEY")
      client <- makeClient
      
      // Request a backup
      backupId <- client.requestBackup(config)
      _ = assert(backupId.value.nonEmpty, "Backup ID should not be empty")
      
      // Check status immediately (should be queued or started)
      initialStatus <- client.getBackupStatus(config, backupId)
      _ = assert(
        initialStatus == BackupStatus.Requesting || initialStatus == BackupStatus.Downloading,
        s"Initial status should be Requesting or Downloading, got: $initialStatus"
      )
      
      // Poll until complete (with timeout)
      finalStatus <- pollUntilComplete(client, config, backupId, maxAttempts = 60)
      _ = assert(
        finalStatus == BackupStatus.Downloading,
        s"Final status should be Downloading (backup complete), got: $finalStatus"
      )
      
    yield ()
  }
  
  test("Radarr - request backup and check status".tag(integrationTag)) {
    for
      config <- getInstanceConfig("radarr-test", ArrType.Radarr, "http://localhost:7878/radarr", "RADARR_API_KEY")
      client <- makeClient
      
      backupId <- client.requestBackup(config)
      _ = assert(backupId.value.nonEmpty)
      
      initialStatus <- client.getBackupStatus(config, backupId)
      _ = assert(
        initialStatus == BackupStatus.Requesting || initialStatus == BackupStatus.Downloading
      )
      
      finalStatus <- pollUntilComplete(client, config, backupId, maxAttempts = 60)
      _ = assert(finalStatus == BackupStatus.Downloading)
      
    yield ()
  }
  
  test("Lidarr - request backup and check status".tag(integrationTag)) {
    for
      config <- getInstanceConfig("lidarr-test", ArrType.Lidarr, "http://localhost:8686/lidarr", "LIDARR_API_KEY")
      client <- makeClient
      
      backupId <- client.requestBackup(config)
      _ = assert(backupId.value.nonEmpty)
      
      initialStatus <- client.getBackupStatus(config, backupId)
      _ = assert(
        initialStatus == BackupStatus.Requesting || initialStatus == BackupStatus.Downloading
      )
      
      finalStatus <- pollUntilComplete(client, config, backupId, maxAttempts = 60)
      _ = assert(finalStatus == BackupStatus.Downloading)
      
    yield ()
  }
  
  test("Prowlarr - request backup and check status".tag(integrationTag)) {
    for
      config <- getInstanceConfig("prowlarr-test", ArrType.Prowlarr, "http://localhost:9696", "PROWLARR_API_KEY")
      client <- makeClient
      
      backupId <- client.requestBackup(config)
      _ = assert(backupId.value.nonEmpty)
      
      initialStatus <- client.getBackupStatus(config, backupId)
      _ = assert(
        initialStatus == BackupStatus.Requesting || initialStatus == BackupStatus.Downloading
      )
      
      finalStatus <- pollUntilComplete(client, config, backupId, maxAttempts = 60)
      _ = assert(finalStatus == BackupStatus.Downloading)
      
    yield ()
  }
  
  test("Sonarr - download backup file".tag(integrationTag)) {
    for
      config <- getInstanceConfig("sonarr-test", ArrType.Sonarr, "http://localhost:8989/sonarr", "SONARR_API_KEY")
      client <- makeClient
      
      // Request and wait for backup
      backupId <- client.requestBackup(config)
      _ <- pollUntilComplete(client, config, backupId, maxAttempts = 60)
      
      // Download the backup - pass directory, not full file path
      // This matches how BackupManagerImpl actually calls it
      tempDir = JFiles.createTempDirectory("backuparr-test")
      
      downloadedPath <- client.downloadBackup(config, backupId, tempDir)
      
      // Verify the file exists and has content
      _ = assert(JFiles.exists(downloadedPath), "Downloaded file should exist")
      _ = assert(downloadedPath.getParent == tempDir, s"Downloaded file should be in temp directory: expected $tempDir, got ${downloadedPath.getParent}")
      _ = assert(downloadedPath.getFileName.toString.contains("sonarr_backup"), s"Downloaded file should have backup name from API, got: ${downloadedPath.getFileName}")
      fileSize = JFiles.size(downloadedPath)
      _ = assert(fileSize > 0, s"Downloaded file should have content, got size: $fileSize")
      
      // Cleanup
      _ <- IO.delay(JFiles.delete(downloadedPath))
      _ <- IO.delay(JFiles.delete(tempDir))
      
    yield ()
  }
  
  test("Invalid API key returns error".tag(integrationTag)) {
    for
      // Create temp file with invalid API key
      tempFile <- IO.delay:
        val f = JFiles.createTempFile("backuparr-test-invalid-", ".key")
        f.toFile.deleteOnExit()
        JFiles.writeString(f, "invalid-api-key-12345")
        f
      
      invalidConfig = ArrInstanceConfig(
        name = "invalid-sonarr",
        arrType = ArrType.Sonarr,
        url = "http://localhost:8989/sonarr",
        apiKeyFile = tempFile.toString,
        schedule = "0 2 * * *",
        s3BucketName = "test-bucket",
        retentionPolicy = com.backuparr.config.RetentionPolicyConfig(keepLast = Some(7))
      )
      
      client <- makeClient
      
      // This should fail during API version detection with an auth error
      result <- client.requestBackup(invalidConfig).attempt
      
      _ = result match
        case Left(error) =>
          // Error happens during version detection now
          assert(
            error.getMessage.contains("Could not detect API version") || 
            error.getMessage.contains("Unauthorized"),
            s"Error should mention API version detection failure or Unauthorized: ${error.getMessage}"
          )
        case Right(_) =>
          fail("Expected an error for invalid API key")
      
    yield ()
  }
  
  test("Non-existent instance returns error".tag(integrationTag)) {
    for
      // Create temp file with dummy API key
      tempFile <- IO.delay:
        val f = JFiles.createTempFile("backuparr-test-nonexistent-", ".key")
        f.toFile.deleteOnExit()
        JFiles.writeString(f, "some-key")
        f
      
      nonExistentConfig = ArrInstanceConfig(
        name = "non-existent",
        arrType = ArrType.Sonarr,
        url = "http://localhost:9999",  // Non-existent port
        apiKeyFile = tempFile.toString,
        schedule = "0 2 * * *",
        s3BucketName = "test-bucket",
        retentionPolicy = com.backuparr.config.RetentionPolicyConfig(keepLast = Some(7))
      )
      
      client <- makeClient
      
      // This should fail during API version detection with a connection error
      result <- client.requestBackup(nonExistentConfig).attempt
      
      _ = result match
        case Left(error) =>
          // Error happens during version detection now
          assert(
            error.getMessage.contains("Could not detect API version") ||
            error.getMessage.contains("Connection refused"),
            s"Error should indicate API version detection failure or connection failure: ${error.getMessage}"
          )
        case Right(_) =>
          fail("Expected an error for non-existent instance")
      
    yield ()
  }
  
  /**
   * Helper to poll backup status until complete.
   * 
   * @param client ArrClient instance
   * @param config Instance configuration
   * @param backupId Backup ID to poll
   * @param maxAttempts Maximum number of polling attempts
   * @return Final backup status
   */
  private def pollUntilComplete(
    client: ArrClient[IO],
    config: ArrInstanceConfig,
    backupId: BackupId,
    maxAttempts: Int
  ): IO[BackupStatus] =
    def poll(attempt: Int): IO[BackupStatus] =
      for
        status <- client.getBackupStatus(config, backupId)
        result <- status match
          case BackupStatus.Downloading =>
            // Backup is complete
            IO.pure(status)
          case BackupStatus.Requesting if attempt >= maxAttempts =>
            IO.raiseError(
              new RuntimeException(s"Backup did not complete after $maxAttempts attempts")
            )
          case _ =>
            // Still in progress, wait and retry
            IO.sleep(2.seconds) *> poll(attempt + 1)
      yield result
    
    poll(1)
