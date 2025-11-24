package com.backuparr

import cats.effect.{ExitCode, IO, IOApp, Resource}
import cats.syntax.all.*
import com.backuparr.algebras.*
import com.backuparr.config.{BackuparrConfig, ConfigLoader}
import com.backuparr.http.HealthCheckServer
import com.backuparr.impl.{BackupManagerImpl, HealthCheckImpl, SchedulerImpl, S3ClientAwsSdk}
import org.http4s.ember.client.EmberClientBuilder
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger

import java.nio.file.{Files, Paths}

/**
 * Main application entry point.
 * 
 * This application:
 * 1. Loads configuration from config.yaml
 * 2. Initializes all components (ArrClient, S3Client, BackupManager, Scheduler)
 * 3. Starts the scheduler for automated backups
 * 4. Starts the health check HTTP server
 * 5. Runs until interrupted (SIGTERM/SIGINT)
 * 6. Gracefully shuts down (stops scheduler, completes in-flight backups)
 * 
 * Pure functional implementation using Cats Effect:
 * - All effects wrapped in IO
 * - Resource-based lifecycle management
 * - Graceful shutdown with proper cleanup
 * - Comprehensive logging throughout
 */
object Main extends IOApp:
  
  /**
   * Main application logic.
   * 
   * Returns ExitCode.Success on normal termination,
   * ExitCode.Error on failures.
   */
  override def run(args: List[String]): IO[ExitCode] =
    applicationResource(args).use { _ =>
      // Application runs here until interrupted
      IO.never.as(ExitCode.Success)
    }.handleErrorWith { error =>
      IO.println(s"Application failed: ${error.getMessage}") >>
        IO.pure(ExitCode.Error)
    }
  
  /**
   * Application resource manages the complete lifecycle.
   * 
   * Resources are acquired in order and released in reverse order.
   * This ensures proper cleanup even on errors or cancellation.
   */
  def applicationResource(args: List[String]): Resource[IO, Unit] =
    for
      // Create logger first (needed throughout startup)
      logger <- Resource.eval(Slf4jLogger.create[IO])
      given Logger[IO] = logger
      
      _ <- Resource.eval(logger.info("========================================"))
      _ <- Resource.eval(logger.info("Starting Backuparr"))
      _ <- Resource.eval(logger.info("========================================"))
      
      // Load and validate configuration
      configPath = args.headOption.getOrElse("config.yaml")
      _ <- Resource.eval(logger.info(s"Loading configuration from: $configPath"))
      config <- Resource.eval(loadConfig(configPath))
      _ <- Resource.eval(logger.info(s"Configuration loaded successfully"))
      _ <- Resource.eval(logger.info(s"  - ${config.arrInstances.size} *arr instances configured"))
      _ <- Resource.eval(logger.info(s"  - ${config.s3Buckets.size} S3 buckets configured"))
      _ <- Resource.eval(logger.info(s"  - Max concurrent backups: ${config.backuparr.maxConcurrentBackups}"))
      _ <- Resource.eval(logger.info(s"  - Health check enabled: ${config.backuparr.healthCheck.enabled.getOrElse(true)}"))
      
      // Validate configuration
      _ <- Resource.eval(validateConfig(config))
      
      // Create S3 bucket map for lookups
      s3BucketMap = config.s3Buckets.map(b => b.name -> b).toMap
      
      // Initialize HTTP client (shared across all *arr instances)
      _ <- Resource.eval(logger.info("Initializing HTTP client"))
      httpClient <- EmberClientBuilder.default[IO].build
      
      // Initialize all S3 clients (one per bucket)
      _ <- Resource.eval(logger.info("Initializing S3 clients"))
      s3Clients <- config.s3Buckets.traverse { bucketConfig =>
        for
          _ <- Resource.eval(logger.info(s"  - Creating S3 client for bucket: ${bucketConfig.name}"))
          client <- S3ClientAwsSdk.resource[IO](bucketConfig)
        yield bucketConfig.name -> client
      }.map(_.toMap)
      
      // We need a single S3Client for the backup manager, use the first one
      // (BackupManager will select the correct bucket based on instance config)
      defaultS3Client = s3Clients.values.headOption.getOrElse(
        throw new RuntimeException("No S3 buckets configured")
      )
      
      // Initialize ArrClient
      _ <- Resource.eval(logger.info("Initializing ArrClient"))
      arrClient <- Resource.eval(ArrClient.make[IO](httpClient, logger))
      
      // Initialize RetentionManager
      _ <- Resource.eval(logger.info("Initializing RetentionManager"))
      retentionManager <- Resource.eval(RetentionManager.make[IO](defaultS3Client))
      
      // Initialize BackupManager
      _ <- Resource.eval(logger.info("Initializing BackupManager"))
      backupManager <- Resource.eval(BackupManagerImpl.make[IO](
        arrClient,
        defaultS3Client,
        retentionManager,
        s3BucketMap
      ))
      
      // Initialize Scheduler
      _ <- Resource.eval(logger.info("Initializing Scheduler"))
      scheduler <- Resource.eval(SchedulerImpl.make[IO](
        backupManager,
        config.arrInstances,
        maxConcurrent = config.backuparr.maxConcurrentBackups
      ))
      
      // Initialize HealthCheck
      _ <- Resource.eval(logger.info("Initializing HealthCheck"))
      healthCheck <- Resource.eval(HealthCheck.make[IO](
        backupManager,
        defaultS3Client,
        schedulerRunning = IO.pure(true), // TODO: Track actual scheduler state
        config.arrInstances,
        s3BucketMap
      ))
      
      // Mark startup complete
      _ <- Resource.eval(healthCheck.asInstanceOf[HealthCheckImpl[IO]].markStartupComplete)
      _ <- Resource.eval(logger.info("Startup complete"))
      
      // Start scheduler in background
      _ <- Resource.eval(logger.info("Starting scheduler"))
      schedulerFiber <- Resource.make(
        scheduler.start.start
      )(fiber =>
        logger.info("Stopping scheduler") >>
          scheduler.stop >>
          fiber.cancel >>
          logger.info("Scheduler stopped")
      )
      _ <- Resource.eval(logger.info("Scheduler started"))
      
      // Start health check server if enabled
      _ <- if config.backuparr.healthCheck.enabled.getOrElse(true) then
        for
          _ <- Resource.eval(logger.info(
            s"Starting health check server on ${config.backuparr.healthCheck.host.getOrElse("0.0.0.0")}:${config.backuparr.healthCheck.port.getOrElse(8080)}"
          ))
          server <- HealthCheckServer.resource[IO](
            healthCheck,
            config.backuparr.healthCheck.host.getOrElse("0.0.0.0"),
            config.backuparr.healthCheck.port.getOrElse(8080)
          )
          _ <- Resource.eval(logger.info("Health check server started"))
        yield ()
      else
        Resource.eval(logger.info("Health check server disabled"))
      
      _ <- Resource.eval(logger.info("========================================"))
      _ <- Resource.eval(logger.info("Backuparr is running"))
      _ <- Resource.eval(logger.info("Press Ctrl+C to stop"))
      _ <- Resource.eval(logger.info("========================================"))
      
    yield ()
  
  /**
   * Load configuration from YAML file.
   * 
   * @param path path to config.yaml
   * @return parsed and validated configuration
   */
  def loadConfig(path: String)(using logger: Logger[IO]): IO[BackuparrConfig] =
    for
      // Check if file exists
      configPath <- IO.pure(Paths.get(path))
      exists <- IO.blocking(Files.exists(configPath))
      _ <- if !exists then
        logger.error(s"Configuration file not found: $path") >>
          IO.raiseError(new RuntimeException(s"Configuration file not found: $path"))
      else
        IO.unit
      
      // Load configuration
      configLoader = ConfigLoader.makeWithEnvSubstitution[IO]
      config <- configLoader.loadConfig(path).handleErrorWith { error =>
        logger.error(s"Failed to load configuration: ${error.getMessage}") >>
          IO.raiseError(error)
      }
      
    yield config
  
  /**
   * Validate configuration.
   * 
   * Checks for:
   * - At least one instance configured
   * - At least one S3 bucket configured
   * - All instances reference valid buckets
   * - Valid cron expressions
   * 
   * @param config the configuration to validate
   */
  def validateConfig(config: BackuparrConfig)(using logger: Logger[IO]): IO[Unit] =
    for
      errors <- IO.pure(config.validate)
      
      _ <- if errors.nonEmpty then
        logger.error("Configuration validation failed:") >>
          errors.traverse_(err => logger.error(s"  - $err")) >>
          IO.raiseError(new RuntimeException(s"Configuration validation failed: ${errors.mkString(", ")}"))
      else
        logger.info("Configuration validation passed")
      
      // Additional validation: check temp directory
      tempDir <- IO.pure(Paths.get(config.backuparr.tempDirectory))
      tempDirExists <- IO.blocking(Files.exists(tempDir))
      _ <- if !tempDirExists then
        logger.warn(s"Temp directory does not exist, creating: ${config.backuparr.tempDirectory}") >>
          IO.blocking(Files.createDirectories(tempDir))
      else
        IO.unit
      
      // Validate temp directory is writable
      isWritable <- IO.blocking(Files.isWritable(tempDir))
      _ <- if !isWritable then
        logger.error(s"Temp directory is not writable: ${config.backuparr.tempDirectory}") >>
          IO.raiseError(new RuntimeException(s"Temp directory is not writable: ${config.backuparr.tempDirectory}"))
      else
        IO.unit
      
    yield ()
