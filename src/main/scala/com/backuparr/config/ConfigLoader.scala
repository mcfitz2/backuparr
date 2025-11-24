package com.backuparr.config

import cats.effect.Sync
import cats.syntax.all.*
import io.circe.yaml.parser
import io.circe.{Decoder, DecodingFailure}
import java.nio.file.{Files, Paths}
import scala.io.Source

/**
 * Loads and validates application configuration.
 * 
 * This algebra defines operations for loading configuration from files.
 * Using an algebra allows us to:
 * 1. Test with mock implementations
 * 2. Abstract over the effect type F
 * 3. Separate interface from implementation
 */
trait ConfigLoader[F[_]]:
  /**
   * Load the main application configuration from a YAML file.
   * 
   * @param path Path to the config YAML file
   * @return Validated configuration or error
   */
  def loadConfig(path: String): F[BackuparrConfig]
  
  /**
   * Load S3 credentials from a YAML file.
   * 
   * @param path Path to the credentials YAML file
   * @return S3 credentials or error
   */
  def loadS3Credentials(path: String): F[S3Credentials]

object ConfigLoader:
  
  /**
   * Format a Circe decoding error with more helpful information.
   */
  private def formatDecodingError(err: DecodingFailure): String =
    // DecodingFailure has a CursorHistory that shows the path through the JSON
    val path = err.history.collect {
      case io.circe.CursorOp.DownField(k) => s".$k"
      case io.circe.CursorOp.DownArray => "[*]"
      case io.circe.CursorOp.Field(k) => s".${k}"
      case io.circe.CursorOp.DownN(n) => s"[$n]"
    }.mkString("")
    
    if path.nonEmpty then
      s"${err.message} at path: $path"
    else
      err.message
  
  /**
   * Create a ConfigLoader implementation for any effect type F that has a Sync instance.
   * 
   * Sync is a type class that provides:
   * - delay: Defer side effects into F
   * - raiseError: Lift errors into F
   * - handleErrorWith: Handle errors functionally
   * 
   * The [F[_]: Sync] syntax is a context bound, equivalent to:
   * def make[F[_]](implicit F: Sync[F]): ConfigLoader[F]
   */
  def make[F[_]: Sync]: ConfigLoader[F] = new ConfigLoader[F]:
    
    override def loadConfig(path: String): F[BackuparrConfig] =
      for
        // Read the file content (side effect wrapped in F)
        content <- readFile(path)
        
        // Parse YAML to JSON (pure operation, can fail)
        json <- Sync[F].fromEither(
          parser.parse(content)
            .left.map(err => new RuntimeException(s"Failed to parse YAML: ${err.message}"))
        )
        
        // Decode JSON to BackuparrConfig (pure operation, can fail)
        config <- Sync[F].fromEither(
          json.as[BackuparrConfig]
            .left.map(err => new RuntimeException(s"Failed to decode config: ${formatDecodingError(err)}"))
        )
        
        // Validate the configuration
        _ <- validateConfig(config)
        
      yield config
    
    override def loadS3Credentials(path: String): F[S3Credentials] =
      for
        content <- readFile(path)
        json <- Sync[F].fromEither(
          parser.parse(content)
            .left.map(err => new RuntimeException(s"Failed to parse credentials YAML: ${err.message}"))
        )
        creds <- Sync[F].fromEither(
          json.as[S3Credentials]
            .left.map(err => new RuntimeException(s"Failed to decode credentials: ${formatDecodingError(err)}"))
        )
      yield creds
    
    /**
     * Read a file as a string.
     * 
     * Uses Sync[F].delay to wrap the side-effecting file read operation.
     * This ensures the read doesn't happen until the F is executed.
     */
    private def readFile(path: String): F[String] =
      Sync[F].delay:
        val source = Source.fromFile(path)
        try source.mkString
        finally source.close()
    
    /**
     * Validate configuration and raise an error if invalid.
     * 
     * This demonstrates functional error handling:
     * - We validate and get a list of errors
     * - If there are errors, we raise them as an exception
     * - If no errors, we return unit (success)
     */
    private def validateConfig(config: BackuparrConfig): F[Unit] =
      val errors = config.validate
      if errors.nonEmpty then
        Sync[F].raiseError(
          new RuntimeException(
            s"Configuration validation failed:\n${errors.mkString("\n  - ", "\n  - ", "")}"
          )
        )
      else
        Sync[F].unit // unit is like void, represents "no meaningful value"

  /**
   * Load configuration with environment variable substitution.
   * 
   * Supports ${VAR_NAME} syntax in YAML files.
   * This is useful for injecting values from the environment.
   */
  def makeWithEnvSubstitution[F[_]: Sync]: ConfigLoader[F] = new ConfigLoader[F]:
    private val envVarPattern = """\$\{([^}]+)\}""".r
    
    override def loadConfig(path: String): F[BackuparrConfig] =
      for
        content <- readFile(path)
        substituted <- substituteEnvVars(content)
        json <- Sync[F].fromEither(
          parser.parse(substituted)
            .left.map(err => new RuntimeException(s"Failed to parse YAML: ${err.message}"))
        )
        config <- Sync[F].fromEither(
          json.as[BackuparrConfig]
            .left.map(err => new RuntimeException(s"Failed to decode config: ${formatDecodingError(err)}"))
        )
        _ <- validateConfig(config)
      yield config
    
    override def loadS3Credentials(path: String): F[S3Credentials] =
      for
        content <- readFile(path)
        substituted <- substituteEnvVars(content)
        json <- Sync[F].fromEither(
          parser.parse(substituted)
            .left.map(err => new RuntimeException(s"Failed to parse credentials YAML: ${err.message}"))
        )
        creds <- Sync[F].fromEither(
          json.as[S3Credentials]
            .left.map(err => new RuntimeException(s"Failed to decode credentials: ${formatDecodingError(err)}"))
        )
      yield creds
    
    private def readFile(path: String): F[String] =
      Sync[F].delay:
        val source = Source.fromFile(path)
        try source.mkString
        finally source.close()
    
    private def substituteEnvVars(content: String): F[String] =
      Sync[F].delay:
        envVarPattern.replaceAllIn(content, m =>
          val varName = m.group(1)
          sys.env.getOrElse(varName, 
            throw new RuntimeException(s"Environment variable not found: $varName")
          )
        )
    
    private def validateConfig(config: BackuparrConfig): F[Unit] =
      val errors = config.validate
      if errors.nonEmpty then
        Sync[F].raiseError(
          new RuntimeException(
            s"Configuration validation failed:\n${errors.mkString("\n  - ", "\n  - ", "")}"
          )
        )
      else
        Sync[F].unit
