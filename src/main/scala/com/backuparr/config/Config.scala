package com.backuparr.config

import com.backuparr.domain.{ArrType, S3Provider}
import io.circe.{Decoder, Encoder}
import io.circe.generic.semiauto.*

/**
 * Root configuration for the Backuparr application.
 * 
 * This is loaded from config.yaml and validated at startup.
 * Using case classes for configuration provides:
 * 1. Type safety
 * 2. Immutability
 * 3. Easy validation
 * 4. Clear structure
 */
case class BackuparrConfig(
  /** Global application settings */
  backuparr: GlobalConfig,
  
  /** List of *arr instances to backup */
  arrInstances: List[ArrInstanceConfig],
  
  /** List of S3 bucket configurations */
  s3Buckets: List[S3BucketConfig]
):
  /**
   * Validate the configuration.
   * Returns a list of validation errors, empty if valid.
   */
  def validate: List[String] =
    val errors = scala.collection.mutable.ListBuffer[String]()
    
    // Check for duplicate instance names
    val duplicateInstances = arrInstances.groupBy(_.name).filter(_._2.size > 1).keys
    if duplicateInstances.nonEmpty then
      errors += s"Duplicate instance names: ${duplicateInstances.mkString(", ")}"
    
    // Check for duplicate bucket names
    val duplicateBuckets = s3Buckets.groupBy(_.name).filter(_._2.size > 1).keys
    if duplicateBuckets.nonEmpty then
      errors += s"Duplicate bucket names: ${duplicateBuckets.mkString(", ")}"
    
    // Check that each instance references a valid bucket
    val bucketNames = s3Buckets.map(_.name).toSet
    arrInstances.foreach: instance =>
      if !bucketNames.contains(instance.s3BucketName) then
        errors += s"Instance '${instance.name}' references non-existent bucket '${instance.s3BucketName}'"
    
    // Validate each instance config
    arrInstances.foreach: instance =>
      errors ++= instance.validate.map(err => s"Instance '${instance.name}': $err")
    
    // Validate each bucket config
    s3Buckets.foreach: bucket =>
      errors ++= bucket.validate.map(err => s"Bucket '${bucket.name}': $err")
    
    errors.toList
  
  /**
   * Get an S3 bucket configuration by name.
   */
  def getBucket(name: String): Option[S3BucketConfig] =
    s3Buckets.find(_.name == name)

object BackuparrConfig:
  given Decoder[BackuparrConfig] = deriveDecoder[BackuparrConfig]

/**
 * Global application settings.
 */
case class GlobalConfig(
  /** Maximum number of concurrent backup operations */
  maxConcurrentBackups: Int = 3,
  
  /** Directory for temporary backup file storage */
  tempDirectory: String = "/tmp/backuparr",
  
  /** Health check endpoint configuration */
  healthCheck: HealthCheckConfig = HealthCheckConfig(),
  
  /** Logging configuration */
  logging: LoggingConfig = LoggingConfig()
):
  def validate: List[String] =
    val errors = scala.collection.mutable.ListBuffer[String]()
    
    if maxConcurrentBackups < 1 then
      errors += "maxConcurrentBackups must be at least 1"
    
    if tempDirectory.trim.isEmpty then
      errors += "tempDirectory cannot be empty"
    
    errors.toList

object GlobalConfig:
  given Decoder[GlobalConfig] = deriveDecoder[GlobalConfig]

/**
 * Health check endpoint configuration.
 */
case class HealthCheckConfig(
  /** Whether to enable health check endpoints */
  enabled: Option[Boolean] = Some(true),
  
  /** Host to bind the health check server to */
  host: Option[String] = Some("0.0.0.0"),
  
  /** Port for health check HTTP server */
  port: Option[Int] = Some(8080)
)

object HealthCheckConfig:
  given Decoder[HealthCheckConfig] = deriveDecoder[HealthCheckConfig]

/**
 * Logging configuration.
 */
case class LoggingConfig(
  /** Log level: TRACE, DEBUG, INFO, WARN, ERROR */
  level: String = "INFO"
)

object LoggingConfig:
  given Decoder[LoggingConfig] = deriveDecoder[LoggingConfig]

/**
 * Configuration for a single *arr instance.
 */
case class ArrInstanceConfig(
  /** Unique name for this instance */
  name: String,
  
  /** Type of *arr application */
  arrType: ArrType,
  
  /** Base URL of the *arr instance (e.g., http://sonarr:8989) */
  url: String,
  
  /** Path to file containing API key (mounted as K8s secret) */
  apiKeyFile: String,
  
  /** Cron-like schedule expression for backups */
  schedule: String,
  
  /** Name of the S3 bucket configuration to use */
  s3BucketName: String,
  
  /** Retention policy for this instance's backups */
  retentionPolicy: RetentionPolicyConfig,
  
  /** Whether this instance is enabled for backups */
  enabled: Option[Boolean] = Some(true),
  
  /** Timeout for backup operations (in seconds) */
  timeoutSeconds: Option[Int] = Some(3600)
):
  /**
   * Validate this instance configuration.
   */
  def validate: List[String] =
    val errors = scala.collection.mutable.ListBuffer[String]()
    
    if name.trim.isEmpty then
      errors += "name cannot be empty"
    
    if url.trim.isEmpty then
      errors += "url cannot be empty"
    
    if !url.startsWith("http://") && !url.startsWith("https://") then
      errors += "url must start with http:// or https://"
    
    if apiKeyFile.trim.isEmpty then
      errors += "apiKeyFile must be specified"
    
    if schedule.trim.isEmpty then
      errors += "schedule cannot be empty"
    
    if s3BucketName.trim.isEmpty then
      errors += "s3BucketName cannot be empty"
    
    timeoutSeconds.foreach: t =>
      if t < 60 then errors += "timeoutSeconds must be at least 60"
    
    errors.toList

object ArrInstanceConfig:
  given Decoder[ArrInstanceConfig] = deriveDecoder[ArrInstanceConfig]

/**
 * S3 bucket configuration.
 */
case class S3BucketConfig(
  /** Unique name for this bucket configuration */
  name: String,
  
  /** S3 provider type */
  provider: S3Provider,
  
  /** S3 region */
  region: String,
  
  /** Bucket name */
  bucket: String,
  
  /** Path to file containing S3 credentials */
  credentialsFile: String,
  
  /** Custom endpoint URL (for non-AWS S3) */
  endpoint: Option[String] = None,
  
  /** Whether to use path-style access */
  pathStyle: Boolean = false,
  
  /** Prefix for all backup keys in this bucket */
  prefix: String = "backups"
):
  /**
   * Validate this bucket configuration.
   */
  def validate: List[String] =
    val errors = scala.collection.mutable.ListBuffer[String]()
    
    if name.trim.isEmpty then
      errors += "name cannot be empty"
    
    if region.trim.isEmpty then
      errors += "region cannot be empty"
    
    if bucket.trim.isEmpty then
      errors += "bucket cannot be empty"
    
    if credentialsFile.trim.isEmpty then
      errors += "credentialsFile cannot be empty"
    
    // Validate endpoint URL has a scheme if provided
    endpoint.foreach { ep =>
      if !ep.startsWith("http://") && !ep.startsWith("https://") then
        errors += s"endpoint must start with http:// or https://, got: $ep"
    }
    
    // For non-AWS providers, endpoint might be required
    provider match
      case S3Provider.Backblaze if endpoint.isEmpty =>
        errors += "endpoint required for Backblaze provider"
      case S3Provider.Generic if endpoint.isEmpty =>
        errors += "endpoint required for Generic provider"
      case _ => ()
    
    errors.toList
  
  /**
   * Get the effective endpoint URL for this bucket.
   * Returns the custom endpoint if set, otherwise the provider default.
   */
  def effectiveEndpoint: Option[String] =
    endpoint.orElse(S3Provider.defaultEndpoint(provider, region))

object S3BucketConfig:
  given Decoder[S3BucketConfig] = deriveDecoder[S3BucketConfig]

/**
 * Retention policy configuration.
 * 
 * Multiple retention strategies can be combined.
 * A backup is kept if it matches ANY of the specified policies.
 */
case class RetentionPolicyConfig(
  /** Keep the last N backups */
  keepLast: Option[Int] = None,
  
  /** Keep one backup per day for the last N days */
  keepDaily: Option[Int] = None,
  
  /** Keep one backup per week for the last N weeks */
  keepWeekly: Option[Int] = None,
  
  /** Keep one backup per month for the last N months */
  keepMonthly: Option[Int] = None
):
  /**
   * Validate this retention policy.
   */
  def validate: List[String] =
    val errors = scala.collection.mutable.ListBuffer[String]()
    
    // At least one retention policy must be specified
    if keepLast.isEmpty && keepDaily.isEmpty && keepWeekly.isEmpty && keepMonthly.isEmpty then
      errors += "at least one retention policy must be specified"
    
    // All specified values must be positive
    keepLast.foreach: n =>
      if n < 1 then errors += "keepLast must be at least 1"
    
    keepDaily.foreach: n =>
      if n < 1 then errors += "keepDaily must be at least 1"
    
    keepWeekly.foreach: n =>
      if n < 1 then errors += "keepWeekly must be at least 1"
    
    keepMonthly.foreach: n =>
      if n < 1 then errors += "keepMonthly must be at least 1"
    
    errors.toList

object RetentionPolicyConfig:
  given Decoder[RetentionPolicyConfig] = deriveDecoder[RetentionPolicyConfig]

/**
 * S3 credentials loaded from a separate file.
 * 
 * This is typically mounted from a Kubernetes secret.
 */
case class S3Credentials(
  /** AWS access key ID */
  accessKeyId: String,
  
  /** AWS secret access key */
  secretAccessKey: String
)

object S3Credentials:
  given Decoder[S3Credentials] = deriveDecoder[S3Credentials]
