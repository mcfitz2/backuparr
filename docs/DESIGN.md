# Backuparr Design Document

## Table of Contents
1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Core Components](#core-components)
4. [Data Flow](#data-flow)
5. [Configuration](#configuration)
6. [APIs and Interfaces](#apis-and-interfaces)
7. [Error Handling](#error-handling)
8. [Concurrency Model](#concurrency-model)
9. [S3 Integration](#s3-integration)
10. [Future Enhancements](#future-enhancements)

## Overview

Backuparr is a Scala 3 application designed to automate backups of *arr applications (Sonarr, Radarr, Lidarr, Prowlarr, etc.) to S3-compatible storage. The application runs as a stateless service in Kubernetes and orchestrates the backup lifecycle: triggering backups, downloading them, and uploading to S3.

### Key Features
- Multi-instance support (backup multiple *arr instances)
- Multiple S3 bucket support with different credentials
- S3-compatible services support (AWS S3, MinIO, Backblaze B2)
- Configurable backup scheduling
- Configurable retention policies
- YAML-based configuration
- Kubernetes-native (secrets, health checks, graceful shutdown)
- Pure functional design with Cats Effect

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Backuparr Application                   │
│                                                              │
│  ┌────────────────┐  ┌──────────────┐  ┌─────────────────┐ │
│  │   Scheduler    │  │   Backup     │  │   S3 Uploader   │ │
│  │   Component    │→ │   Manager    │→ │   Component     │ │
│  └────────────────┘  └──────────────┘  └─────────────────┘ │
│           ↑                  ↑                    ↑          │
│           │                  │                    │          │
│  ┌────────┴──────────────────┴────────────────────┴───────┐ │
│  │              Configuration Manager                     │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
         │                      │                      │
         ↓                      ↓                      ↓
┌────────────────┐    ┌──────────────────┐    ┌──────────────┐
│ YAML Config    │    │  *arr APIs       │    │ S3 Storage   │
│ + K8s Secrets  │    │ (Sonarr, etc.)   │    │ (AWS/MinIO)  │
└────────────────┘    └──────────────────┘    └──────────────┘
```

### Layer Architecture

1. **Main Application Layer**: Entry point, dependency wiring, graceful shutdown
2. **Orchestration Layer**: Scheduler, backup coordination
3. **Service Layer**: *arr client, S3 client, backup management
4. **Domain Layer**: Pure business logic, domain models
5. **Infrastructure Layer**: HTTP clients, file I/O, configuration loading

## Core Components

### 1. Configuration Manager

**Purpose**: Load and parse configuration from YAML files and mounted secrets.

**Key Classes**:
- `Config`: Root configuration case class
- `ArrInstanceConfig`: Configuration for a single *arr instance
- `S3BucketConfig`: Configuration for an S3 bucket
- `BackupScheduleConfig`: Cron-like schedule configuration
- `RetentionConfig`: Retention policy configuration
- `ConfigLoader`: Loads and validates configuration

**Responsibilities**:
- Parse YAML configuration file
- Load S3 credentials from separate files (K8s secrets)
- Validate configuration at startup
- Provide typed, immutable configuration to other components

### 2. Scheduler Component

**Purpose**: Trigger backups based on configured schedules.

**Key Classes**:
- `BackupScheduler`: Main scheduler algebra
- `CronScheduler`: Cron-based scheduling implementation
- `ScheduleParser`: Parse schedule expressions

**Responsibilities**:
- Parse cron-like schedule expressions
- Calculate next backup time
- Trigger backup operations at scheduled times
- Support concurrent scheduling for multiple instances
- Graceful shutdown with in-flight backup completion

### 3. *arr Client Service

**Purpose**: Interface with *arr application APIs.

**Key Classes**:
- `ArrClient[F[_]]`: Algebra for *arr API operations
- `ArrClientImpl`: HTTP-based implementation
- `ArrBackupRequest`: Request models
- `ArrBackupResponse`: Response models
- `ArrApiError`: Error ADT for API failures

**Responsibilities**:
- Trigger backup via *arr API
- Poll for backup completion
- Download backup file
- Handle API authentication
- Retry logic with exponential backoff
- Proper error handling and reporting

**API Endpoints Used**:
```
POST /api/v3/system/backup
GET  /api/v3/system/backup
GET  /api/v3/system/backup/{id}/download
```

### 4. Backup Manager

**Purpose**: Orchestrate the backup lifecycle.

**Key Classes**:
- `BackupManager[F[_]]`: Main backup orchestration algebra
- `BackupJob`: Represents a backup job
- `BackupStatus`: ADT for backup states (Pending, InProgress, Completed, Failed)
- `BackupMetadata`: Metadata about a backup

**Responsibilities**:
- Coordinate backup creation, download, and upload
- Track backup state
- Handle temporary storage of backup files
- Clean up after successful upload
- Implement retry logic for failed operations
- Apply retention policies

### 5. S3 Uploader Component

**Purpose**: Upload backups to S3-compatible storage.

**Key Classes**:
- `S3Client[F[_]]`: Algebra for S3 operations
- `S3ClientImpl`: Implementation using AWS SDK or http4s
- `S3Upload`: Upload operation model
- `S3Config`: S3 endpoint and credential configuration

**Responsibilities**:
- Upload files to S3 with streaming
- Support multiple S3 backends (AWS, MinIO, B2)
- Handle multipart uploads for large files
- Verify upload success
- List existing backups
- Delete old backups per retention policy
- Support custom endpoints for S3-compatible services

### 6. Retention Manager

**Purpose**: Manage backup retention policies.

**Key Classes**:
- `RetentionManager[F[_]]`: Retention policy algebra
- `RetentionPolicy`: ADT for different retention strategies
  - `KeepLast(n: Int)`: Keep last N backups
  - `KeepDaily(days: Int)`: Keep daily backups for N days
  - `KeepWeekly(weeks: Int)`: Keep weekly backups for N weeks
  - `KeepMonthly(months: Int)`: Keep monthly backups for N months
  - `Composite`: Combine multiple policies

**Responsibilities**:
- Evaluate retention policies
- Identify backups to delete
- Execute deletion through S3 client
- Log retention actions

### 7. Health Check Service

**Purpose**: Provide Kubernetes health and readiness endpoints.

**Key Classes**:
- `HealthCheck[F[_]]`: Health check algebra
- `HealthStatus`: Health status model

**Responsibilities**:
- Liveness probe (is the app running?)
- Readiness probe (can the app handle requests?)
- Check connectivity to *arr instances
- Check S3 connectivity
- Expose metrics

## Data Flow

### Backup Creation Flow

```
1. Scheduler triggers backup for instance X
   └─> BackupManager.createBackup(instance: ArrInstanceConfig)
       
2. BackupManager requests backup from *arr instance
   └─> ArrClient.requestBackup(instance)
       └─> POST /api/v3/system/backup
       └─> Returns backupId
       
3. BackupManager polls for backup completion
   └─> ArrClient.getBackupStatus(backupId)
       └─> GET /api/v3/system/backup
       └─> Retry until status is "completed"
       
4. BackupManager downloads backup file
   └─> ArrClient.downloadBackup(backupId)
       └─> GET /api/v3/system/backup/{id}/download
       └─> Stream to temporary location
       
5. BackupManager uploads to S3
   └─> S3Client.uploadBackup(file, bucket, key)
       └─> Stream upload to S3
       └─> Returns S3 URI
       
6. BackupManager applies retention policy
   └─> RetentionManager.applyPolicy(bucket, policy)
       └─> Lists existing backups
       └─> Identifies backups to delete
       └─> S3Client.deleteBackup(key) for each
       
7. BackupManager cleans up temporary file
   └─> Delete local backup file
   
8. BackupManager records success
   └─> Log backup completion with metadata
```

### Concurrent Operations

- Multiple *arr instances can be backed up concurrently
- Each instance has its own schedule
- Backups are rate-limited to avoid overwhelming systems
- Use `Semaphore` to limit concurrent operations
- Use `Queue` for backup task management

## Configuration

### Main Configuration File (`config.yaml`)

```yaml
# Global settings
backuparr:
  # Maximum concurrent backups
  maxConcurrentBackups: 3
  
  # Temporary storage for downloads
  tempDirectory: /tmp/backuparr
  
  # Health check settings
  healthCheck:
    enabled: true
    port: 8080
    
  # Logging
  logging:
    level: INFO

# *arr instance configurations
arrInstances:
  - name: sonarr-main
    type: sonarr
    url: http://sonarr.default.svc.cluster.local:8989
    apiKey: ${SONARR_API_KEY}  # Can reference env vars
    schedule: "0 2 * * *"  # Daily at 2 AM
    s3Bucket: backups-sonarr
    retentionPolicy:
      keepLast: 7
      keepDaily: 30
      keepWeekly: 12
      keepMonthly: 12
    
  - name: radarr-main
    type: radarr
    url: http://radarr.default.svc.cluster.local:7878
    apiKeyFile: /secrets/radarr/api-key  # Or read from file
    schedule: "0 3 * * *"  # Daily at 3 AM
    s3Bucket: backups-radarr
    retentionPolicy:
      keepLast: 7

# S3 bucket configurations
s3Buckets:
  - name: backups-sonarr
    provider: aws  # aws, minio, backblaze
    region: us-east-1
    bucket: my-sonarr-backups
    credentialsFile: /secrets/s3/sonarr-creds.yaml
    # Optional: for non-AWS S3
    endpoint: null
    pathStyle: false
    
  - name: backups-radarr
    provider: backblaze
    region: us-west-002
    bucket: my-radarr-backups
    credentialsFile: /secrets/s3/radarr-creds.yaml
    endpoint: https://s3.us-west-002.backblazeb2.com
    pathStyle: false
```

### S3 Credentials File (`/secrets/s3/sonarr-creds.yaml`)

```yaml
accessKeyId: AKIAIOSFODNN7EXAMPLE
secretAccessKey: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

### Configuration Models

```scala
case class BackuparrConfig(
  maxConcurrentBackups: Int,
  tempDirectory: String,
  healthCheck: HealthCheckConfig,
  logging: LoggingConfig,
  arrInstances: List[ArrInstanceConfig],
  s3Buckets: List[S3BucketConfig]
)

case class ArrInstanceConfig(
  name: String,
  arrType: ArrType,  // Sonarr, Radarr, Lidarr, etc.
  url: String,
  apiKey: Option[String],
  apiKeyFile: Option[String],
  schedule: String,
  s3BucketName: String,
  retentionPolicy: RetentionPolicyConfig,
  enabled: Boolean = true
)

case class S3BucketConfig(
  name: String,
  provider: S3Provider,  // AWS, MinIO, Backblaze
  region: String,
  bucket: String,
  credentialsFile: String,
  endpoint: Option[String],
  pathStyle: Boolean
)

case class RetentionPolicyConfig(
  keepLast: Option[Int],
  keepDaily: Option[Int],
  keepWeekly: Option[Int],
  keepMonthly: Option[Int]
)
```

## APIs and Interfaces

### Understanding Algebras and Tagless Final

**What is an Algebra?**

In functional programming, an "algebra" is a trait (interface) that defines a set of operations without specifying how they're implemented. The term comes from abstract algebra in mathematics. In our code, algebras are traits parameterized by an effect type `F[_]`.

**Why Use Algebras?**

1. **Abstraction**: We can write code that works with any effect type (IO, Future, etc.)
2. **Testability**: Easy to create mock implementations for testing
3. **Composability**: Algebras can be combined and composed
4. **Separation of Concerns**: Interface (what) is separate from implementation (how)

**The `F[_]` Type Parameter**

The `F[_]` is a higher-kinded type parameter:
- `F` is a type constructor (it takes a type to produce a type)
- `F[_]` means "F needs one type parameter to be complete"
- Examples: `IO[_]`, `Option[_]`, `List[_]`

When we write `F[BackupId]`:
- If `F` is `IO`, we get `IO[BackupId]` (an effect that produces a BackupId)
- If `F` is `Option`, we get `Option[BackupId]` (a value that might be a BackupId)

**Tagless Final Pattern**

This pattern uses:
1. **Algebras**: Traits with `F[_]` type parameter
2. **Constraints**: Type class requirements (e.g., `F[_]: Async`)
3. **Interpreters**: Concrete implementations for specific `F` types

**Example Walkthrough**

```scala
// 1. Define the algebra (interface)
trait ArrClient[F[_]]:
  def requestBackup(instance: ArrInstanceConfig): F[BackupId]
  
// 2. Create an interpreter (implementation) for IO
class ArrClientImpl[F[_]: Async](httpClient: Client[F]) extends ArrClient[F]:
  def requestBackup(instance: ArrInstanceConfig): F[BackupId] =
    // Implementation using F (which is IO in production)
    httpClient.expect[BackupResponse](request).map(_.id)

// 3. Use it polymorphically
def backupAll[F[_]: Async](
  client: ArrClient[F],
  instances: List[ArrInstanceConfig]
): F[List[BackupId]] =
  instances.traverse(client.requestBackup)
  
// 4. At the application edge, choose concrete type
val client: ArrClient[IO] = new ArrClientImpl[IO](httpClient)
val result: IO[List[BackupId]] = backupAll[IO](client, instances)
```

**Benefits for Learning**

- Code is more explicit about effects (you see `F[_]` everywhere effects happen)
- Forces you to think about effect composition
- Makes testing easier (can use `Id` or test-specific effects)
- Teaches important FP concepts used throughout Cats Effect ecosystem

**In This Project**

Every major service is an algebra:
- `ArrClient[F[_]]`: Defines *arr API operations
- `S3Client[F[_]]`: Defines S3 operations
- `BackupManager[F[_]]`: Defines backup orchestration
- `Scheduler[F[_]]`: Defines scheduling operations

This means we can:
- Test with mock implementations
- Swap implementations (e.g., in-memory S3 for testing)
- Compose operations generically
- Have compile-time guarantees about effects

### Main Algebras (Tagless Final)

```scala
/** Algebra for interacting with *arr applications */
trait ArrClient[F[_]]:
  /** Request a backup from the *arr instance */
  def requestBackup(instance: ArrInstanceConfig): F[BackupId]
  
  /** Check the status of a backup */
  def getBackupStatus(instance: ArrInstanceConfig, backupId: BackupId): F[BackupStatus]
  
  /** Download a completed backup */
  def downloadBackup(
    instance: ArrInstanceConfig, 
    backupId: BackupId, 
    destination: Path
  ): F[Path]

/** Algebra for S3 operations */
trait S3Client[F[_]]:
  /** Upload a file to S3 */
  def uploadFile(
    bucket: S3BucketConfig,
    key: String,
    source: Path,
    metadata: Map[String, String]
  ): F[S3Uri]
  
  /** List backups in a bucket */
  def listBackups(bucket: S3BucketConfig, prefix: String): F[List[S3Object]]
  
  /** Delete a backup from S3 */
  def deleteBackup(bucket: S3BucketConfig, key: String): F[Unit]

/** Algebra for backup orchestration */
trait BackupManager[F[_]]:
  /** Execute a complete backup for an instance */
  def executeBackup(instance: ArrInstanceConfig): F[BackupResult]
  
  /** Get current backup status */
  def getStatus: F[Map[String, BackupStatus]]

/** Algebra for scheduling */
trait Scheduler[F[_]]:
  /** Start the scheduler */
  def start: F[Unit]
  
  /** Stop the scheduler gracefully */
  def stop: F[Unit]

/** Algebra for retention management */
trait RetentionManager[F[_]]:
  /** Apply retention policy to a bucket */
  def applyRetention(
    bucket: S3BucketConfig,
    policy: RetentionPolicyConfig,
    instanceName: String
  ): F[RetentionResult]
```

### Domain Models

```scala
/** Represents a backup ID from the *arr API */
opaque type BackupId = String

/** The type of *arr application */
enum ArrType:
  case Sonarr, Radarr, Lidarr, Prowlarr, Readarr

/** Status of a backup operation */
enum BackupStatus:
  case Pending
  case Requesting
  case Downloading
  case Uploading
  case ApplyingRetention
  case Completed(uri: S3Uri, timestamp: Instant)
  case Failed(error: BackupError, timestamp: Instant)

/** Errors that can occur during backup */
enum BackupError:
  case ArrApiError(message: String, cause: Option[Throwable])
  case DownloadError(message: String, cause: Option[Throwable])
  case S3UploadError(message: String, cause: Option[Throwable])
  case RetentionError(message: String, cause: Option[Throwable])
  case ConfigurationError(message: String)
  case TimeoutError(operation: String)

/** Result of a backup operation */
case class BackupResult(
  instanceName: String,
  status: BackupStatus,
  startTime: Instant,
  endTime: Instant,
  backupSize: Option[Long],
  s3Uri: Option[S3Uri]
)

/** Result of retention policy application */
case class RetentionResult(
  kept: List[S3Object],
  deleted: List[S3Object],
  errors: List[BackupError]
)

/** S3 object metadata */
case class S3Object(
  key: String,
  size: Long,
  lastModified: Instant,
  metadata: Map[String, String]
)
```

## Error Handling

### Error Strategy

1. **Typed Errors**: Use ADTs for domain errors
2. **Error Recovery**: Retry transient failures with exponential backoff
3. **Error Reporting**: Log all errors with context
4. **Graceful Degradation**: Failed backup of one instance doesn't stop others
5. **Error Propagation**: Use `Either`, `EitherT`, or `IO.raiseError`

### Retry Logic

```scala
trait RetryPolicy:
  def maxAttempts: Int
  def initialDelay: FiniteDuration
  def maxDelay: FiniteDuration
  def backoffMultiplier: Double

object DefaultRetryPolicy extends RetryPolicy:
  val maxAttempts = 3
  val initialDelay = 1.second
  val maxDelay = 30.seconds
  val backoffMultiplier = 2.0
```

### Circuit Breaker

- Implement circuit breaker for *arr API calls
- Prevent cascading failures
- Use Cats Effect's built-in or a library like resilience4s

## Concurrency Model

### Thread Pools

1. **Compute Pool**: CPU-bound operations (default Cats Effect compute pool)
2. **Blocking Pool**: Blocking I/O operations (file operations)
3. **HTTP Client Pool**: HTTP requests (built into http4s)

### Concurrent Operations

- Use `Semaphore` to limit concurrent backups
- Use `Ref` for mutable state (backup status tracking)
- Use `Queue` for work distribution
- Use `Deferred` for coordination
- Proper cancellation handling

### Resource Management

```scala
def makeApp: Resource[IO, Unit] =
  for
    config <- Resource.eval(ConfigLoader.load)
    httpClient <- makeHttpClient
    s3Client <- S3Client.make(httpClient)
    arrClient <- ArrClient.make(httpClient)
    backupManager <- BackupManager.make(arrClient, s3Client, config)
    scheduler <- Scheduler.make(backupManager, config)
    healthCheck <- HealthCheck.make(backupManager, config)
    _ <- scheduler.start.background
    _ <- healthCheck.serve.background
  yield ()
```

## S3 Integration

### S3 SDK Options

1. **AWS SDK for Java 2.x**: Official SDK, heavyweight
2. **http4s + S3 REST API**: Lightweight, pure FP
3. **fs2-aws**: fs2-based AWS integrations

**Recommendation**: Use http4s with S3 REST API for full control and FP alignment.

### S3 Upload Strategy

- Use multipart upload for files > 5GB
- Stream uploads to avoid loading entire file in memory
- Calculate MD5/SHA256 for integrity verification
- Set appropriate metadata (backup date, instance name, version)

### S3 Key Structure

```
backups/{instanceName}/{year}/{month}/{day}/{timestamp}_{instanceName}_backup.zip

Example:
backups/sonarr-main/2025/11/23/20251123T020000Z_sonarr-main_backup.zip
```

### Supporting Multiple S3 Providers

```scala
enum S3Provider:
  case AWS
  case MinIO
  case Backblaze
  case Generic

object S3Provider:
  def endpointFor(provider: S3Provider, region: String): Option[String] =
    provider match
      case AWS => None  // Use default AWS endpoints
      case MinIO => Some("http://minio.default.svc.cluster.local:9000")
      case Backblaze => Some(s"https://s3.$region.backblazeb2.com")
      case Generic => None  // User provides custom endpoint
```

## Future Enhancements

### Phase 2
- Backup verification (download and verify before deleting)
- Backup encryption before S3 upload
- Prometheus metrics export
- Webhook notifications on backup completion/failure
- Backup restore functionality
- Database backup for *arr instances (not just app backup)

### Phase 3
- Web UI for configuration and monitoring
- Support for other backup destinations (Google Cloud Storage, Azure Blob)
- Incremental backups
- Backup deduplication
- Multi-region replication

### Phase 4
- Event-driven backups (trigger on specific events)
- Backup comparison and diff
- Automated restore testing
- Backup catalog and search

## Implementation Plan

### Phase 1: Foundation (Weeks 1-2)
1. Project setup (sbt, dependencies)
2. Configuration loading (YAML parsing, validation)
3. Domain models and error types
4. Basic logging setup

### Phase 2: Core Services (Weeks 3-4)
1. *arr client implementation
2. S3 client implementation
3. Backup manager implementation
4. Unit tests for core services

### Phase 3: Orchestration (Weeks 5-6)
1. Scheduler implementation
2. Retention manager implementation
3. Integration tests
4. Error handling and retry logic

### Phase 4: Production Ready (Weeks 7-8)
1. Health check endpoints
2. Graceful shutdown
3. Docker image creation
4. Kubernetes manifests
5. Documentation and examples
6. End-to-end testing

## Dependencies

### Core Dependencies
```scala
libraryDependencies ++= Seq(
  // Cats Effect ecosystem
  "org.typelevel" %% "cats-effect" % "3.5.2",
  "org.typelevel" %% "cats-core" % "2.10.0",
  
  // HTTP client
  "org.http4s" %% "http4s-ember-client" % "0.23.23",
  "org.http4s" %% "http4s-circe" % "0.23.23",
  "org.http4s" %% "http4s-ember-server" % "0.23.23",  // For health checks
  
  // JSON
  "io.circe" %% "circe-core" % "0.14.6",
  "io.circe" %% "circe-generic" % "0.14.6",
  "io.circe" %% "circe-parser" % "0.14.6",
  "io.circe" %% "circe-yaml" % "0.15.1",
  
  // Logging
  "org.typelevel" %% "log4cats-slf4j" % "2.6.0",
  "ch.qos.logback" % "logback-classic" % "1.4.11",
  
  // FS2 for streaming
  "co.fs2" %% "fs2-core" % "3.9.3",
  "co.fs2" %% "fs2-io" % "3.9.3",
  
  // Configuration
  "com.github.pureconfig" %% "pureconfig-core" % "0.17.4",
  
  // S3 (AWS SDK or pure Scala implementation)
  "com.github.j5ik2o" %% "reactive-aws-s3-core" % "1.2.6",
  
  // Cron parsing
  "com.github.alonsodomin.cron4s" %% "cron4s-core" % "0.6.1",
  
  // Testing
  "org.typelevel" %% "munit-cats-effect-3" % "1.0.7" % Test,
  "org.scalameta" %% "munit" % "0.7.29" % Test,
  "org.typelevel" %% "scalacheck-effect-munit" % "1.0.4" % Test
)
```

## Conclusion

This design provides a solid foundation for building Backuparr as a production-ready, pure functional Scala application. The architecture is modular, testable, and follows FP best practices. The use of tagless final allows for easy testing and multiple implementations. The integration with Kubernetes ensures cloud-native deployment, while the extensive configuration options provide flexibility for various use cases.
