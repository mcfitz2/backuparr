package com.backuparr.http

import cats.effect.IO
import com.backuparr.algebras.{BackupManager, HealthCheck, S3Client}
import com.backuparr.config.{ArrInstanceConfig, RetentionPolicyConfig, S3BucketConfig}
import com.backuparr.domain.{ArrType, BackupStatus, S3Provider}
import com.backuparr.impl.HealthCheckImpl
import munit.CatsEffectSuite
import org.http4s.*
import org.http4s.implicits.*
import org.http4s.circe.*
import io.circe.Json

/**
 * Unit tests for health check endpoints.
 * 
 * Tests the HTTP routes and JSON encoding for liveness, readiness, and status endpoints.
 */
class HealthCheckRoutesSpec extends CatsEffectSuite:
  
  // Mock configuration
  val testInstance = ArrInstanceConfig(
    name = "test-sonarr",
    arrType = ArrType.Sonarr,
    url = "http://localhost:8989",
    apiKeyFile = "/tmp/test-key",
    schedule = "0 0 * * *",
    s3BucketName = "test-bucket",
    retentionPolicy = RetentionPolicyConfig(keepLast = Some(3))
  )
  
  val testBucket = S3BucketConfig(
    name = "test-bucket",
    provider = S3Provider.AWS,
    region = "us-east-1",
    bucket = "my-backups",
    credentialsFile = "/tmp/creds.yaml"
  )
  
  // Mock BackupManager
  val mockBackupManager = new BackupManager[IO] {
    override def executeBackup(instance: ArrInstanceConfig) = ???
    override def getStatus = IO.pure(Map.empty[String, BackupStatus])
  }
  
  // Mock S3Client
  val mockS3Client = new S3Client[IO] {
    override def uploadFile(bucket: S3BucketConfig, key: String, source: java.nio.file.Path, metadata: Map[String, String]) = ???
    override def listObjects(bucket: S3BucketConfig, prefix: String) = ???
    override def getObjectMetadata(bucket: S3BucketConfig, key: String) = ???
    override def deleteObject(bucket: S3BucketConfig, key: String) = ???
    override def objectExists(bucket: S3BucketConfig, key: String) = ???
  }
  
  test("GET /health returns 200 OK with alive status") {
    for
      healthCheck <- HealthCheckImpl.make[IO](
        mockBackupManager,
        mockS3Client,
        IO.pure(true),
        List(testInstance),
        Map(testBucket.name -> testBucket)
      )
      routes = HealthCheckRoutes.routes[IO](healthCheck)
      
      request = Request[IO](Method.GET, uri"/health")
      response <- routes.orNotFound.run(request)
      
      _ <- IO(assertEquals(response.status, Status.Ok))
      
      body <- response.as[Json]
      _ <- IO(assert(body.hcursor.get[Boolean]("alive").contains(true)))
      _ <- IO(assertEquals(body.hcursor.get[String]("message").toOption, Some("Application is alive")))
      
    yield ()
  }
  
  test("GET /ready returns 503 when startup not complete") {
    for
      healthCheck <- HealthCheckImpl.make[IO](
        mockBackupManager,
        mockS3Client,
        IO.pure(true),
        List(testInstance),
        Map(testBucket.name -> testBucket)
      )
      routes = HealthCheckRoutes.routes[IO](healthCheck)
      
      request = Request[IO](Method.GET, uri"/ready")
      response <- routes.orNotFound.run(request)
      
      // Should be not ready initially (startup not complete)
      _ <- IO(assertEquals(response.status, Status.ServiceUnavailable))
      
      body <- response.as[Json]
      _ <- IO(assert(body.hcursor.get[Boolean]("ready").contains(false)))
      _ <- IO(assertEquals(body.hcursor.get[String]("message").toOption, Some("Startup not complete")))
      
    yield ()
  }
  
  test("GET /ready returns 200 when ready") {
    for
      healthCheck <- HealthCheckImpl.make[IO](
        mockBackupManager,
        mockS3Client,
        IO.pure(true),
        List(testInstance),
        Map(testBucket.name -> testBucket)
      )
      
      // Mark startup complete
      _ <- healthCheck.asInstanceOf[HealthCheckImpl[IO]].markStartupComplete
      
      routes = HealthCheckRoutes.routes[IO](healthCheck)
      
      request = Request[IO](Method.GET, uri"/ready")
      response <- routes.orNotFound.run(request)
      
      _ <- IO(assertEquals(response.status, Status.Ok))
      
      body <- response.as[Json]
      _ <- IO(assert(body.hcursor.get[Boolean]("ready").contains(true)))
      _ <- IO(assertEquals(body.hcursor.get[String]("message").toOption, Some("Application is ready")))
      
    yield ()
  }
  
  test("GET /ready returns 503 when scheduler not running") {
    for
      healthCheck <- HealthCheckImpl.make[IO](
        mockBackupManager,
        mockS3Client,
        IO.pure(false), // Scheduler not running
        List(testInstance),
        Map(testBucket.name -> testBucket)
      )
      
      _ <- healthCheck.asInstanceOf[HealthCheckImpl[IO]].markStartupComplete
      
      routes = HealthCheckRoutes.routes[IO](healthCheck)
      
      request = Request[IO](Method.GET, uri"/ready")
      response <- routes.orNotFound.run(request)
      
      _ <- IO(assertEquals(response.status, Status.ServiceUnavailable))
      
      body <- response.as[Json]
      _ <- IO(assert(body.hcursor.get[Boolean]("ready").contains(false)))
      _ <- IO(assertEquals(body.hcursor.get[String]("message").toOption, Some("Scheduler not running")))
      
    yield ()
  }
  
  test("GET /status returns detailed status") {
    for
      healthCheck <- HealthCheckImpl.make[IO](
        mockBackupManager,
        mockS3Client,
        IO.pure(true),
        List(testInstance),
        Map(testBucket.name -> testBucket)
      )
      
      _ <- healthCheck.asInstanceOf[HealthCheckImpl[IO]].markStartupComplete
      
      routes = HealthCheckRoutes.routes[IO](healthCheck)
      
      request = Request[IO](Method.GET, uri"/status")
      response <- routes.orNotFound.run(request)
      
      _ <- IO(assertEquals(response.status, Status.Ok))
      
      body <- response.as[Json]
      _ <- IO(assert(body.hcursor.get[Boolean]("schedulerRunning").contains(true)))
      
      // Check health object
      health = body.hcursor.downField("health")
      _ <- IO(assert(health.get[Boolean]("alive").contains(true)))
      _ <- IO(assert(health.get[Boolean]("ready").contains(true)))
      
      // Check instances object
      instances = body.hcursor.downField("instances")
      _ <- IO(assert(instances.downField("test-sonarr").succeeded))
      
      instance = instances.downField("test-sonarr")
      _ <- IO(assertEquals(instance.get[String]("name").toOption, Some("test-sonarr")))
      _ <- IO(assertEquals(instance.get[String]("arrType").toOption, Some("Sonarr")))
      
    yield ()
  }
  
  test("GET /status always returns 200 even when not ready") {
    for
      healthCheck <- HealthCheckImpl.make[IO](
        mockBackupManager,
        mockS3Client,
        IO.pure(false), // Not ready
        List(testInstance),
        Map(testBucket.name -> testBucket)
      )
      
      routes = HealthCheckRoutes.routes[IO](healthCheck)
      
      request = Request[IO](Method.GET, uri"/status")
      response <- routes.orNotFound.run(request)
      
      // Status endpoint always returns 200
      _ <- IO(assertEquals(response.status, Status.Ok))
      
      body <- response.as[Json]
      health = body.hcursor.downField("health")
      _ <- IO(assert(health.get[Boolean]("ready").contains(false)))
      
    yield ()
  }
