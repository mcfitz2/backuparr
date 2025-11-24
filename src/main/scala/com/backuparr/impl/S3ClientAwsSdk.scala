package com.backuparr.impl

import cats.effect.{Async, Resource}
import cats.syntax.all.*
import com.backuparr.algebras.S3Client
import com.backuparr.config.{S3BucketConfig, S3Credentials}
import com.backuparr.domain.{S3Object, S3Provider, S3Uri}
import io.circe.yaml.parser
import org.typelevel.log4cats.Logger
import org.typelevel.log4cats.slf4j.Slf4jLogger
import software.amazon.awssdk.auth.credentials.{AwsBasicCredentials, StaticCredentialsProvider}
import software.amazon.awssdk.core.async.AsyncRequestBody
import software.amazon.awssdk.regions.Region
import software.amazon.awssdk.services.s3.S3AsyncClient
import software.amazon.awssdk.services.s3.model.*

import java.net.URI
import java.nio.file.{Files as JFiles, Path, Paths}
import scala.jdk.CollectionConverters.*
import scala.jdk.FutureConverters.*

/**
 * Implementation of S3Client using the official AWS SDK for Java v2.
 * 
 * This implementation:
 * - Uses battle-tested AWS SDK for all S3 operations
 * - Automatically handles AWS Signature V4 authentication
 * - Supports multiple S3 providers (AWS, MinIO, Backblaze)
 * - Uses async operations wrapped in Cats Effect
 * 
 * Benefits over hand-rolled implementation:
 * - No manual signature calculation
 * - Well-tested by AWS and millions of users
 * - Better error handling and edge case coverage
 * - Easier to maintain
 * 
 * @param s3Client AWS SDK S3 async client
 * @param bucket S3 bucket configuration
 */
class S3ClientAwsSdk[F[_]: Async](
  s3Client: S3AsyncClient,
  bucket: S3BucketConfig
) extends S3Client[F]:
  
  private val logger: Logger[F] = Slf4jLogger.getLogger[F]
  
  /**
   * Upload a file to S3 using AWS SDK's PutObjectRequest.
   * 
   * The AWS SDK handles:
   * - AWS Signature V4 calculation
   * - Content-Length header
   * - Streaming the file content
   * - Retry logic
   */
  override def uploadFile(
    bucket: S3BucketConfig,
    key: String,
    source: Path,
    metadata: Map[String, String] = Map.empty
  ): F[S3Uri] =
    // Compute the full S3 key by prepending the bucket prefix (internal detail)
    val s3Key = if bucket.prefix.isEmpty then key else s"${bucket.prefix}/$key"
    
    for
      _ <- logger.info(s"Uploading ${source.getFileName} to s3://${bucket.bucket}/$key")
      _ <- logger.debug(s"  S3 key (with prefix): $s3Key")
      
      // Build the PutObjectRequest
      putRequest <- Async[F].delay:
        PutObjectRequest.builder()
          .bucket(bucket.bucket)
          .key(s3Key)
          .metadata(metadata.asJava)
          .build()
      
      // Create async request body from file
      requestBody <- Async[F].delay(AsyncRequestBody.fromFile(source))
      
      // Execute the upload
      _ <- Async[F].fromCompletableFuture:
        Async[F].delay(s3Client.putObject(putRequest, requestBody))
      
      _ <- logger.info(s"Successfully uploaded to s3://${bucket.bucket}/$key")
    yield
      // Return the S3 URI using the caller's key (without internal prefix)
      S3Uri(s"s3://${bucket.bucket}/$key")
  
  /**
   * List objects in the S3 bucket with an optional prefix filter.
   * 
   * Uses AWS SDK's ListObjectsV2Request which handles pagination automatically.
   */
  override def listObjects(
    bucket: S3BucketConfig,
    prefix: String
  ): F[List[S3Object]] =
    // Compute the full prefix (bucket prefix + filter prefix)
    val fullPrefix = if bucket.prefix.isEmpty then prefix 
                     else if prefix.isEmpty then bucket.prefix
                     else s"${bucket.prefix}/$prefix"
    
    for
      _ <- logger.debug(s"Listing objects with prefix='$prefix', fullPrefix='$fullPrefix' in bucket=${bucket.bucket}")
      
      // Build the ListObjectsV2Request
      listRequest <- Async[F].delay:
        if fullPrefix.isEmpty then
          ListObjectsV2Request.builder()
            .bucket(bucket.bucket)
            .build()
        else
          ListObjectsV2Request.builder()
            .bucket(bucket.bucket)
            .prefix(fullPrefix)
            .build()
      
      // Execute the list operation
      response <- Async[F].fromCompletableFuture:
        Async[F].delay(s3Client.listObjectsV2(listRequest))
      
      _ <- logger.info(s"Listed ${response.contents().size()} objects with fullPrefix='$fullPrefix'")
    yield
      // Convert AWS SDK objects to our domain model
      // Strip the bucket prefix from keys so they're relative paths
      response.contents().asScala.toList.map: s3Object =>
        val fullKey = s3Object.key()
        val relativeKey = if bucket.prefix.nonEmpty && fullKey.startsWith(s"${bucket.prefix}/") then
          fullKey.stripPrefix(s"${bucket.prefix}/")
        else
          fullKey
          
        S3Object(
          key = relativeKey,
          size = s3Object.size(),
          lastModified = s3Object.lastModified(),
          metadata = Map.empty  // AWS SDK doesn't return metadata in list, only in HEAD
        )
  
  /**
   * Delete an object from S3.
   * 
   * Uses AWS SDK's DeleteObjectRequest.
   */
  override def deleteObject(
    bucket: S3BucketConfig,
    key: String
  ): F[Unit] =
    // Compute the full S3 key by prepending the bucket prefix (internal detail)
    val s3Key = if bucket.prefix.isEmpty then key else s"${bucket.prefix}/$key"
    
    for
      _ <- logger.info(s"Deleting s3://${bucket.bucket}/$key")
      _ <- logger.debug(s"  S3 key (with prefix): $s3Key")
      
      // Build the DeleteObjectRequest
      deleteRequest <- Async[F].delay:
        DeleteObjectRequest.builder()
          .bucket(bucket.bucket)
          .key(s3Key)
          .build()
      
      // Execute the delete
      _ <- Async[F].fromCompletableFuture:
        Async[F].delay(s3Client.deleteObject(deleteRequest))
      
      _ <- logger.info(s"Successfully deleted s3://${bucket.bucket}/$key")
    yield ()
  
  /**
   * Check if an object exists in S3.
   * 
   * Uses AWS SDK's HeadObjectRequest which returns 404 if not found.
   */
  override def objectExists(
    bucket: S3BucketConfig,
    key: String
  ): F[Boolean] =
    // Compute the full S3 key by prepending the bucket prefix (internal detail)
    val s3Key = if bucket.prefix.isEmpty then key else s"${bucket.prefix}/$key"
    
    for
      // Build the HeadObjectRequest
      headRequest <- Async[F].delay:
        HeadObjectRequest.builder()
          .bucket(bucket.bucket)
          .key(s3Key)
          .build()
      
      // Try to execute the head request
      exists <- Async[F].fromCompletableFuture:
          Async[F].delay(s3Client.headObject(headRequest))
        .as(true).recover:
          case _: NoSuchKeyException => false
          case _: software.amazon.awssdk.services.s3.model.S3Exception => false
    yield exists
  
  /**
   * Get metadata for an object without downloading it.
   * 
   * Uses AWS SDK's HeadObjectRequest to retrieve headers.
   */
  override def getObjectMetadata(
    bucket: S3BucketConfig,
    key: String
  ): F[S3Object] =
    // Compute the full S3 key by prepending the bucket prefix (internal detail)
    val s3Key = if bucket.prefix.isEmpty then key else s"${bucket.prefix}/$key"
    
    for
      // Build the HeadObjectRequest
      headRequest <- Async[F].delay:
        HeadObjectRequest.builder()
          .bucket(bucket.bucket)
          .key(s3Key)
          .build()
      
      // Execute the head request
      response <- Async[F].fromCompletableFuture:
        Async[F].delay(s3Client.headObject(headRequest))
    yield
      // Build S3Object from response - use the caller's key (without internal prefix)
      S3Object(
        key = key,
        size = response.contentLength(),
        lastModified = response.lastModified(),
        metadata = Option(response.metadata()).map(_.asScala.toMap).getOrElse(Map.empty)
      )

object S3ClientAwsSdk:
  
  /**
   * Create an S3ClientAwsSdk wrapped in a Resource.
   * 
   * The Resource handles:
   * - Loading S3 credentials from file
   * - Creating and configuring the AWS SDK S3AsyncClient
   * - Properly closing the client when done
   */
  def resource[F[_]: Async](bucket: S3BucketConfig): Resource[F, S3Client[F]] =
    for
      // Load credentials
      credsPath <- Resource.eval(Async[F].delay(Paths.get(bucket.credentialsFile)))
      creds <- Resource.eval(loadCredentials[F](credsPath))
      
      // Create AWS credentials provider
      awsCreds = AwsBasicCredentials.create(creds.accessKeyId, creds.secretAccessKey)
      credsProvider = StaticCredentialsProvider.create(awsCreds)
      
      // Build S3AsyncClient with custom endpoint if needed
      s3Client <- Resource.fromAutoCloseable {
        Async[F].delay {
          val builder = S3AsyncClient.builder()
            .credentialsProvider(credsProvider)
            .region(Region.US_EAST_1) // MinIO doesn't care about region
          
          // Set custom endpoint for non-AWS providers
          bucket.provider match
            case S3Provider.MinIO | S3Provider.Backblaze | S3Provider.Generic =>
              bucket.endpoint.foreach(ep => builder.endpointOverride(URI.create(ep)))
              // Force path-style access for MinIO/Backblaze and generic providers
              builder.forcePathStyle(true)
            case S3Provider.AWS =>
              // AWS uses virtual-hosted-style by default
              ()
          
          builder.build()
        }
      }
      
      // Create the S3Client implementation
      client = new S3ClientAwsSdk[F](s3Client, bucket)
    yield client
  
  /**
   * Load S3 credentials from a YAML file.
   * 
   * The file format is:
   * ```yaml
   * access_key: "your-access-key"
   * secret_key: "your-secret-key"
   * ```
   */
  private def loadCredentials[F[_]: Async](credentialsFile: Path): F[S3Credentials] =
    for
      // Read the credentials file
      content <- Async[F].delay(JFiles.readString(credentialsFile))
      
      // Parse YAML
      json <- Async[F].fromEither(parser.parse(content))
      
      // Decode to S3Credentials
      creds <- Async[F].fromEither(json.as[S3Credentials])
    yield creds
