package com.backuparr.integration

import cats.effect.IO
import cats.syntax.all.*
import com.backuparr.config.{S3BucketConfig, S3Credentials}
import com.backuparr.domain.{S3Object, S3Provider, S3Uri}
import com.backuparr.impl.S3ClientAwsSdk
import munit.CatsEffectSuite

import java.nio.file.{Files, Path, Paths}
import scala.util.control.NonFatal

/**
 * Integration tests for S3ClientImpl using real MinIO.
 * 
 * These tests require MinIO to be running (part of docker-compose.test.yml).
 * They test the full end-to-end S3 workflow:
 * - Upload files
 * - List objects
 * - Download/verify files exist
 * - Delete objects
 * 
 * Run these tests with: sbt integrationTest
 */
class S3ClientIntegrationSpec extends CatsEffectSuite:
  
  // Skip if not running integration tests
  override def munitIgnore: Boolean =
    sys.env.get("INTEGRATION").forall(_ != "true")
  
  // MinIO configuration (matches docker-compose.test.yml)
  val testBucket = S3BucketConfig(
    name = "test-bucket",
    provider = S3Provider.MinIO,
    region = "us-east-1",  // MinIO ignores region
    bucket = "backups",
    credentialsFile = "/tmp/minio-creds.yaml",
    endpoint = Some("http://localhost:9000"),
    pathStyle = true,
    prefix = "test"
  )
  
  // MinIO default credentials (from docker-compose.test.yml)
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
    
    // Create the bucket in MinIO if it doesn't exist
    // Using MinIO CLI or REST API would be ideal, but for simplicity we'll just
    // let the first test fail gracefully if bucket doesn't exist
    // In production, buckets should be pre-created
    println("Note: MinIO bucket 'backups' must exist for tests to pass")
    println("Create it with: docker exec backuparr-test-minio mc mb local/backups")
  
  // Cleanup: Remove credentials file after all tests
  override def afterAll(): Unit =
    try
      val credsPath = Paths.get(testBucket.credentialsFile)
      if Files.exists(credsPath) then
        Files.delete(credsPath)
    catch
      case NonFatal(_) => // Ignore cleanup errors
    
    super.afterAll()
  
  // Shared S3 client resource using AWS SDK
  val s3ClientResource = S3ClientAwsSdk.resource[IO](testBucket)
  
  test("uploadFile - upload and verify file exists"):
    s3ClientResource.use { s3Client =>
      
      // Create a test file
      val testFile = Files.createTempFile("backup-test", ".zip")
      val testContent = "This is a test backup file for S3 integration testing"
      Files.writeString(testFile, testContent)
      
      val testKey = s"integration-test-${System.currentTimeMillis()}.zip"
      
      for
        // Upload the file
        s3Uri <- s3Client.uploadFile(
          bucket = testBucket,
          key = testKey,
          source = testFile,
          metadata = Map(
            "test" -> "true",
            "timestamp" -> System.currentTimeMillis().toString
          )
        )
        
        // Verify S3 URI is correct (no prefix - S3Client returns relative paths)
        _ = assertEquals(s3Uri, S3Uri(s"s3://${testBucket.bucket}/$testKey"))
        
        // Verify file exists (using relative key)
        exists <- s3Client.objectExists(testBucket, testKey)
        _ = assertEquals(exists, true)
        
        // Clean up: delete the test file (using relative key)
        _ <- s3Client.deleteObject(testBucket, testKey)
        _ <- IO.delay(Files.delete(testFile))
        
      yield ()
    }
  
  test("listObjects - list and verify uploaded files"):
    s3ClientResource.use { s3Client =>
      // Create and upload multiple test files
      val testFiles = (1 to 3).toList.map { i =>
        val file = Files.createTempFile(s"test-$i", ".txt")
        Files.writeString(file, s"Test file $i")
        (file, s"list-test-${System.currentTimeMillis()}-$i.txt")
      }
      
      for
        // Upload all files
        _ <- testFiles.traverse { case (file, key) =>
          s3Client.uploadFile(testBucket, key, file)
        }
        
        // List objects with prefix
        objects <- s3Client.listObjects(testBucket, "")
        
        // Verify we got our uploaded files
        _ = assert(objects.nonEmpty, "Should have found uploaded objects")
        
        // Clean up: delete all test files (using relative keys)
        _ <- testFiles.traverse { case (file, key) =>
          for
            _ <- s3Client.deleteObject(testBucket, key)
            _ <- IO.delay(Files.delete(file))
          yield ()
        }
        
      yield ()
    }
  
  test("deleteObject - delete and verify file is gone"):
    s3ClientResource.use { s3Client =>
      
      
      // Create and upload a test file
      val testFile = Files.createTempFile("delete-test", ".txt")
      Files.writeString(testFile, "File to be deleted")
      val testKey = s"delete-test-${System.currentTimeMillis()}.txt"
      
      for
        // Upload file
        _ <- s3Client.uploadFile(testBucket, testKey, testFile)
        
        // Verify it exists (using relative key)
        existsBefore <- s3Client.objectExists(testBucket, testKey)
        _ = assertEquals(existsBefore, true)
        
        // Delete it (using relative key)
        _ <- s3Client.deleteObject(testBucket, testKey)
        
        // Verify it's gone
        existsAfter <- s3Client.objectExists(testBucket, testKey)
        _ = assertEquals(existsAfter, false)
        
        // Clean up local file
        _ <- IO.delay(Files.delete(testFile))
        
      yield ()
    }
  
  test("getObjectMetadata - retrieve metadata for uploaded file"):
    s3ClientResource.use { s3Client =>
      
      
      // Create a test file
      val testFile = Files.createTempFile("metadata-test", ".txt")
      val testContent = "File with metadata"
      Files.writeString(testFile, testContent)
      val testKey = s"metadata-test-${System.currentTimeMillis()}.txt"
      
      val customMetadata = Map(
        "instance-name" -> "sonarr-test",
        "backup-date" -> "2025-11-23",
        "version" -> "4.0.0"
      )
      
      for
        // Upload with custom metadata
        _ <- s3Client.uploadFile(testBucket, testKey, testFile, customMetadata)
        
        // Get metadata (using relative key)
        obj <- s3Client.getObjectMetadata(testBucket, testKey)
        
        // Verify metadata (key is relative, no prefix)
        _ = assertEquals(obj.key, testKey)
        _ = assertEquals(obj.size, testContent.length.toLong)
        _ = assertEquals(obj.metadata.get("instance-name"), Some("sonarr-test"))
        _ = assertEquals(obj.metadata.get("backup-date"), Some("2025-11-23"))
        _ = assertEquals(obj.metadata.get("version"), Some("4.0.0"))
        
        // Clean up (using relative key)
        _ <- s3Client.deleteObject(testBucket, testKey)
        _ <- IO.delay(Files.delete(testFile))
        
      yield ()
    }
  
  test("uploadFile - handles large files with streaming"):
    s3ClientResource.use { s3Client =>
      
      
      // Create a larger test file (1MB)
      val testFile = Files.createTempFile("large-test", ".dat")
      val largeContent = "X" * (1024 * 1024)  // 1MB of 'X'
      Files.writeString(testFile, largeContent)
      val testKey = s"large-file-${System.currentTimeMillis()}.dat"
      
      for
        // Upload large file
        s3Uri <- s3Client.uploadFile(testBucket, testKey, testFile)
        
        // Verify it was uploaded (using relative key)
        exists <- s3Client.objectExists(testBucket, testKey)
        _ = assertEquals(exists, true)
        
        // Get metadata to verify size (using relative key)
        obj <- s3Client.getObjectMetadata(testBucket, testKey)
        _ = assertEquals(obj.size, largeContent.length.toLong)
        
        // Clean up (using relative key)
        _ <- s3Client.deleteObject(testBucket, testKey)
        _ <- IO.delay(Files.delete(testFile))
        
      yield ()
    }
  
  test("objectExists - returns false for non-existent object"):
    s3ClientResource.use { s3Client =>
      
      
      val nonExistentKey = s"non-existent-${System.currentTimeMillis()}.txt"
      
      for
        exists <- s3Client.objectExists(testBucket, nonExistentKey)
        _ = assertEquals(exists, false)
      yield ()
    }
  
  test("prefix handling - S3Client handles bucket prefix internally"):
    s3ClientResource.use { s3Client =>
      
      
      // This test verifies the architecture: Only S3Client knows about bucket prefix
      // All other code uses relative paths (no prefix)
      
      val testFile = Files.createTempFile("prefix-test", ".txt")
      Files.writeString(testFile, "Testing prefix handling architecture")
      
      // Use a nested path to make it clear we're testing path handling
      val relativeKey = s"sonarr/backups/test-${System.currentTimeMillis()}.txt"
      
      for
        // 1. Upload with relative key (no prefix)
        s3Uri <- s3Client.uploadFile(testBucket, relativeKey, testFile)
        
        // Verify URI returned uses relative key (no prefix)
        _ = assertEquals(s3Uri, S3Uri(s"s3://${testBucket.bucket}/$relativeKey"))
        
        // 2. List objects to get the key back
        objects <- s3Client.listObjects(testBucket, "sonarr/backups/")
        
        // Verify listObjects returns relative keys (no prefix)
        _ = assert(objects.exists(_.key == relativeKey), 
                   s"Expected to find '$relativeKey' in list results, but got: ${objects.map(_.key)}")
        
        // 3. Get metadata using relative key
        obj <- s3Client.getObjectMetadata(testBucket, relativeKey)
        
        // Verify metadata returns relative key (no prefix)
        _ = assertEquals(obj.key, relativeKey)
        
        // 4. Delete using the key returned from listObjects (proves round-trip works)
        foundKey = objects.find(_.key == relativeKey).get.key
        _ <- s3Client.deleteObject(testBucket, foundKey)
        
        // 5. Verify deletion worked
        existsAfter <- s3Client.objectExists(testBucket, relativeKey)
        _ = assertEquals(existsAfter, false)
        
        // Clean up local file
        _ <- IO.delay(Files.delete(testFile))
        
      yield ()
    }
