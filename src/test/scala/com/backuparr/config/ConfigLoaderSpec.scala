package com.backuparr.config

import cats.effect.IO
import munit.CatsEffectSuite
import com.backuparr.domain.{ArrType, S3Provider}

/**
 * Tests for configuration loading and validation.
 * 
 * This uses MUnit with Cats Effect support, which allows us to
 * test IO-based code easily.
 * 
 * Key testing concepts:
 * - CatsEffectSuite provides test helpers for IO
 * - assertEquals checks equality
 * - interceptIO captures exceptions
 * - Tests run in IO for proper effect testing
 */
class ConfigLoaderSpec extends CatsEffectSuite:
  
  test("parse valid BackuparrConfig from YAML"):
    val yaml = """
      |backuparr:
      |  maxConcurrentBackups: 3
      |  tempDirectory: /tmp/backuparr
      |  healthCheck:
      |    enabled: true
      |    host: "0.0.0.0"
      |    port: 8080
      |  logging:
      |    level: INFO
      |
      |arrInstances:
      |  - name: sonarr-test
      |    arrType: sonarr
      |    url: http://sonarr:8989
      |    apiKeyFile: /tmp/test-key
      |    schedule: "0 2 * * *"
      |    s3BucketName: test-bucket
      |    retentionPolicy:
      |      keepLast: 7
      |    enabled: true
      |    timeoutSeconds: 3600
      |
      |s3Buckets:
      |  - name: test-bucket
      |    provider: aws
      |    region: us-east-1
      |    bucket: my-bucket
      |    credentialsFile: /tmp/creds.yaml
      |    pathStyle: false
      |    prefix: backups
      |""".stripMargin
    
    // Parse YAML using circe-yaml
    val result = for
      json <- IO.fromEither(io.circe.yaml.parser.parse(yaml))
      config <- IO.fromEither(json.as[BackuparrConfig])
    yield config
    
    result.map: config =>
      // Check global config
      assertEquals(config.backuparr.maxConcurrentBackups, 3)
      assertEquals(config.backuparr.tempDirectory, "/tmp/backuparr")
      assertEquals(config.backuparr.healthCheck.enabled, Some(true))
      assertEquals(config.backuparr.healthCheck.port, Some(8080))
      
      // Check instance config
      assertEquals(config.arrInstances.size, 1)
      val instance = config.arrInstances.head
      assertEquals(instance.name, "sonarr-test")
      assertEquals(instance.arrType, ArrType.Sonarr)
      assertEquals(instance.url, "http://sonarr:8989")
      assertEquals(instance.apiKeyFile, "/tmp/test-key")
      assertEquals(instance.schedule, "0 2 * * *")
      assertEquals(instance.s3BucketName, "test-bucket")
      
      // Check S3 bucket config
      assertEquals(config.s3Buckets.size, 1)
      val bucket = config.s3Buckets.head
      assertEquals(bucket.name, "test-bucket")
      assertEquals(bucket.provider, S3Provider.AWS)
      assertEquals(bucket.region, "us-east-1")
      assertEquals(bucket.bucket, "my-bucket")
  
  test("validate catches duplicate instance names"):
    val config = BackuparrConfig(
      backuparr = GlobalConfig(
        maxConcurrentBackups = 3,
        tempDirectory = "/tmp",
        healthCheck = HealthCheckConfig(),
        logging = LoggingConfig()
      ),
      arrInstances = List(
        ArrInstanceConfig(
          name = "duplicate",
          arrType = ArrType.Sonarr,
          url = "http://sonarr1:8989",
          apiKeyFile = "/tmp/key1",
          schedule = "0 2 * * *",
          s3BucketName = "bucket1",
          retentionPolicy = RetentionPolicyConfig(keepLast = Some(7))
        ),
        ArrInstanceConfig(
          name = "duplicate",  // Same name!
          arrType = ArrType.Radarr,
          url = "http://radarr:7878",
          apiKeyFile = "/tmp/key2",
          schedule = "0 3 * * *",
          s3BucketName = "bucket2",
          retentionPolicy = RetentionPolicyConfig(keepLast = Some(7))
        )
      ),
      s3Buckets = List()
    )
    
    val errors = config.validate
    assert(errors.exists(_.contains("Duplicate instance names")))
  
  test("validate catches reference to non-existent bucket"):
    val config = BackuparrConfig(
      backuparr = GlobalConfig(),
      arrInstances = List(
        ArrInstanceConfig(
          name = "test",
          arrType = ArrType.Sonarr,
          url = "http://sonarr:8989",
          apiKeyFile = "/tmp/key",
          schedule = "0 2 * * *",
          s3BucketName = "non-existent-bucket",  // This bucket doesn't exist!
          retentionPolicy = RetentionPolicyConfig(keepLast = Some(7))
        )
      ),
      s3Buckets = List()
    )
    
    val errors = config.validate
    assert(errors.exists(_.contains("references non-existent bucket")))
  
  test("ArrType.fromString parses all types"):
    assertEquals(ArrType.fromString("sonarr"), Some(ArrType.Sonarr))
    assertEquals(ArrType.fromString("radarr"), Some(ArrType.Radarr))
    assertEquals(ArrType.fromString("lidarr"), Some(ArrType.Lidarr))
    assertEquals(ArrType.fromString("prowlarr"), Some(ArrType.Prowlarr))
    assertEquals(ArrType.fromString("readarr"), Some(ArrType.Readarr))
    assertEquals(ArrType.fromString("SONARR"), Some(ArrType.Sonarr))  // Case insensitive
    assertEquals(ArrType.fromString("invalid"), None)
  
  test("S3Provider.fromString parses all providers"):
    assertEquals(S3Provider.fromString("aws"), Some(S3Provider.AWS))
    assertEquals(S3Provider.fromString("minio"), Some(S3Provider.MinIO))
    assertEquals(S3Provider.fromString("backblaze"), Some(S3Provider.Backblaze))
    assertEquals(S3Provider.fromString("generic"), Some(S3Provider.Generic))
    assertEquals(S3Provider.fromString("AWS"), Some(S3Provider.AWS))  // Case insensitive
    assertEquals(S3Provider.fromString("invalid"), None)
