// Project metadata
ThisBuild / organization := "com.backuparr"
ThisBuild / scalaVersion := "3.3.1"
ThisBuild / version := "0.1.0-SNAPSHOT"

// Compiler options for Scala 3
ThisBuild / scalacOptions ++= Seq(
  "-encoding", "utf8",
  "-feature",
  "-unchecked",
  "-deprecation",
  "-Xfatal-warnings",
  "-language:higherKinds",
  "-explain",
  "-explain-types"
)

// Dependency versions
lazy val catsEffectVersion = "3.5.2"
lazy val catsVersion = "2.10.0"
lazy val http4sVersion = "0.23.23"
lazy val circeVersion = "0.14.6"
lazy val fs2Version = "3.9.3"
lazy val log4catsVersion = "2.6.0"
lazy val logbackVersion = "1.4.11"
lazy val munitVersion = "0.7.29"
lazy val munitCatsEffectVersion = "1.0.7"
lazy val circeYamlVersion = "0.15.1"

lazy val root = (project in file("."))
  .settings(
    name := "backuparr",
    
    // Core dependencies
    libraryDependencies ++= Seq(
      // Cats Effect ecosystem - for pure functional programming
      "org.typelevel" %% "cats-effect" % catsEffectVersion,
      "org.typelevel" %% "cats-core" % catsVersion,
      
      // HTTP client and server using http4s
      "org.http4s" %% "http4s-ember-client" % http4sVersion,
      "org.http4s" %% "http4s-ember-server" % http4sVersion,
      "org.http4s" %% "http4s-circe" % http4sVersion,
      "org.http4s" %% "http4s-dsl" % http4sVersion,
      
      // JSON parsing with Circe
      "io.circe" %% "circe-core" % circeVersion,
      "io.circe" %% "circe-generic" % circeVersion,
      "io.circe" %% "circe-parser" % circeVersion,
      "io.circe" %% "circe-yaml" % circeYamlVersion,
      
      // Logging using log4cats (functional logging for Cats Effect)
      "org.typelevel" %% "log4cats-slf4j" % log4catsVersion,
      "ch.qos.logback" % "logback-classic" % logbackVersion,
      
      // FS2 for streaming operations
      "co.fs2" %% "fs2-core" % fs2Version,
      "co.fs2" %% "fs2-io" % fs2Version,
      
      // AWS SDK v2 for S3 - official Java library, simpler and more reliable than hand-rolled signatures
      "software.amazon.awssdk" % "s3" % "2.39.2",
      
      // Testing dependencies
      "org.scalameta" %% "munit" % munitVersion % Test,
      "org.typelevel" %% "munit-cats-effect-3" % munitCatsEffectVersion % Test,
      "org.typelevel" %% "cats-effect-testing-specs2" % "1.5.0" % Test
    ),
    
    // Assembly configuration for creating fat JAR
    assembly / assemblyJarName := s"${name.value}-assembly-${version.value}.jar",
    assembly / mainClass := Some("com.backuparr.Main"),
    
    // Merge strategy for assembly - handle duplicate files
    assembly / assemblyMergeStrategy := {
      case PathList("META-INF", "MANIFEST.MF") => MergeStrategy.discard
      case PathList("META-INF", "versions", xs @ _*) => MergeStrategy.first
      case PathList("META-INF", xs @ _*) if xs.exists(_.endsWith(".SF")) => MergeStrategy.discard
      case PathList("META-INF", xs @ _*) if xs.exists(_.endsWith(".DSA")) => MergeStrategy.discard
      case PathList("META-INF", xs @ _*) if xs.exists(_.endsWith(".RSA")) => MergeStrategy.discard
      case PathList("META-INF", "services", xs @ _*) => MergeStrategy.concat
      case PathList("META-INF", "io.netty.versions.properties") => MergeStrategy.first
      case PathList("module-info.class") => MergeStrategy.discard
      case PathList("codegen-resources", xs @ _*) => MergeStrategy.first
      case PathList("mime.types") => MergeStrategy.first
      case x if x.endsWith("logback.xml") => MergeStrategy.first
      case x if x.endsWith("logback-test.xml") => MergeStrategy.first
      case x =>
        val oldStrategy = (assembly / assemblyMergeStrategy).value
        oldStrategy(x)
    }
  )

// Enable better stack traces for Cats Effect
ThisBuild / Test / fork := true
ThisBuild / Test / javaOptions += "-XX:+UseZGC"

// Integration test configuration using docker-compose
// These tasks manage the docker-compose lifecycle for integration testing
import scala.sys.process._

lazy val dockerComposeFile = "test-setup/docker-compose.yml"

lazy val dockerComposeUp = taskKey[Unit]("Start docker-compose test environment")
lazy val dockerComposeDown = taskKey[Unit]("Stop docker-compose test environment")
lazy val dockerComposeWaitHealthy = taskKey[Unit]("Wait for docker-compose services to be healthy")
lazy val dockerComposeLogs = taskKey[Unit]("Show docker-compose logs")
lazy val getApiKeys = taskKey[Map[String, String]]("Extract API keys from running containers")
lazy val integrationTest = taskKey[Unit]("Run integration tests with docker-compose")

dockerComposeUp := {
  val log = streams.value.log
  log.info("Starting docker-compose test environment...")
  val exitCode = s"docker compose -f $dockerComposeFile up -d".!
  if (exitCode != 0) {
    sys.error("Failed to start docker-compose")
  }
  log.info("Docker-compose started successfully")
}

dockerComposeDown := {
  val log = streams.value.log
  log.info("Stopping docker-compose test environment...")
  val exitCode = s"docker compose -f $dockerComposeFile down".!
  if (exitCode == 0) {
    log.info("Docker-compose stopped successfully")
  } else {
    log.warn(s"Docker-compose down exited with code $exitCode")
  }
}

dockerComposeLogs := {
  val log = streams.value.log
  log.info("Showing docker-compose logs...")
  s"docker compose -f $dockerComposeFile logs --tail=50".!
}

getApiKeys := {
  val log = streams.value.log
  log.info("Extracting API keys from containers...")
  
  def extractApiKey(containerName: String, maxRetries: Int = 10): Option[String] = {
    var attempt = 0
    while (attempt < maxRetries) {
      try {
        // Execute command to read config.xml from container and extract API key
        val result = s"docker exec $containerName cat /config/config.xml".!!
        
        // Parse XML to find <ApiKey>...</ApiKey>
        val apiKeyPattern = "<ApiKey>([a-f0-9]+)</ApiKey>".r
        apiKeyPattern.findFirstMatchIn(result).map(_.group(1)) match {
          case Some(key) => return Some(key)
          case None =>
            attempt += 1
            if (attempt < maxRetries) {
              log.info(s"API key not found in $containerName yet, retrying in 2s... (attempt $attempt/$maxRetries)")
              Thread.sleep(2000)
            }
        }
      } catch {
        case e: Exception =>
          attempt += 1
          if (attempt < maxRetries) {
            log.info(s"Failed to read config from $containerName, retrying in 2s... (attempt $attempt/$maxRetries)")
            Thread.sleep(2000)
          } else {
            log.warn(s"Failed to extract API key from $containerName after $maxRetries attempts: ${e.getMessage}")
          }
      }
    }
    None
  }
  
  val containers = Map(
    "SONARR_API_KEY" -> "backuparr-test-sonarr",
    "RADARR_API_KEY" -> "backuparr-test-radarr",
    "LIDARR_API_KEY" -> "backuparr-test-lidarr",
    "PROWLARR_API_KEY" -> "backuparr-test-prowlarr"
  )
  
  val apiKeys = containers.flatMap { case (envVar, containerName) =>
    log.info(s"Attempting to extract $envVar from $containerName...")
    extractApiKey(containerName) match {
      case Some(key) =>
        log.info(s"✓ Extracted $envVar from $containerName: ${key.take(8)}...")
        Some(envVar -> key)
      case None =>
        log.error(s"✗ Failed to extract API key from $containerName")
        None
    }
  }
  
  if (apiKeys.size != containers.size) {
    sys.error("Failed to extract all API keys from containers")
  }
  
  apiKeys
}

dockerComposeWaitHealthy := {
  val log = streams.value.log
  log.info("Waiting for services to be healthy...")
  
  // Wait up to 120 seconds for services to be healthy
  val maxAttempts = 24
  var attempt = 0
  var allHealthy = false
  
  while (attempt < maxAttempts && !allHealthy) {
    attempt += 1
    Thread.sleep(5000) // Wait 5 seconds between checks
    
    val healthStatus = s"docker compose -f $dockerComposeFile ps --format json".!!
    val allRunning = healthStatus.contains("\"State\":\"running\"")
    
    if (allRunning) {
      allHealthy = true
      log.info(s"All services are healthy (attempt $attempt)")
      // Give containers a few more seconds to finish initialization
      log.info("Waiting for containers to finish initialization...")
      Thread.sleep(5000)
    } else {
      log.info(s"Waiting for services to be healthy... (attempt $attempt/$maxAttempts)")
    }
  }
  
  if (!allHealthy) {
    sys.error("Services did not become healthy in time")
  }
}

integrationTest := {
  val log = streams.value.log
  
  try {
    // Start docker-compose
    log.info("=" * 80)
    log.info("Starting docker-compose test environment...")
    log.info("=" * 80)
    val startExitCode = s"docker compose -f $dockerComposeFile up -d".!
    if (startExitCode != 0) {
      sys.error("Failed to start docker-compose")
    }
    log.info("Docker-compose started successfully")
    log.info("")
    
    // Wait for services to be healthy
    log.info("=" * 80)
    log.info("Waiting for services to be healthy...")
    log.info("=" * 80)
    val maxAttempts = 24
    var attempt = 0
    var allHealthy = false
    
    while (attempt < maxAttempts && !allHealthy) {
      attempt += 1
      Thread.sleep(5000)
      
      val healthStatus = s"docker compose -f $dockerComposeFile ps --format json".!!
      val allRunning = healthStatus.contains("\"State\":\"running\"")
      
      if (allRunning) {
        allHealthy = true
        log.info(s"✓ All services are healthy (attempt $attempt)")
        log.info("Waiting for containers to finish initialization...")
        Thread.sleep(20000)  // Increased to 20s to allow Prowlarr config to be written
      } else {
        log.info(s"Waiting... (attempt $attempt/$maxAttempts)")
      }
    }
    
    if (!allHealthy) {
      sys.error("Services did not become healthy in time")
    }
    log.info("")
    
    // Extract API keys from containers
    log.info("=" * 80)
    log.info("Extracting API keys from containers...")
    log.info("=" * 80)
    
    def extractApiKey(containerName: String, maxRetries: Int = 10): Option[String] = {
      var attempt = 0
      while (attempt < maxRetries) {
        try {
          val result = s"docker exec $containerName cat /config/config.xml".!!
          val apiKeyPattern = "<ApiKey>([a-f0-9]+)</ApiKey>".r
          apiKeyPattern.findFirstMatchIn(result).map(_.group(1)) match {
            case Some(key) => return Some(key)
            case None =>
              attempt += 1
              if (attempt < maxRetries) {
                log.info(s"API key not found in $containerName, retrying in 2s... ($attempt/$maxRetries)")
                Thread.sleep(2000)
              }
          }
        } catch {
          case e: Exception =>
            attempt += 1
            if (attempt < maxRetries) {
              log.info(s"Failed to read config from $containerName, retrying in 2s... ($attempt/$maxRetries)")
              Thread.sleep(2000)
            }
        }
      }
      None
    }
    
    val containers = Map(
      "SONARR_API_KEY" -> "backuparr-test-sonarr",
      "RADARR_API_KEY" -> "backuparr-test-radarr",
      "LIDARR_API_KEY" -> "backuparr-test-lidarr",
      "PROWLARR_API_KEY" -> "backuparr-test-prowlarr"
    )
    
    val apiKeys = containers.flatMap { case (envVar, containerName) =>
      extractApiKey(containerName) match {
        case Some(key) =>
          log.info(s"✓ Extracted $envVar: ${key.take(8)}...")
          Some(envVar -> key)
        case None =>
          log.error(s"✗ Failed to extract API key from $containerName")
          None
      }
    }
    
    if (apiKeys.size != containers.size) {
      sys.error("Failed to extract all API keys from containers")
    }
    
    // Create MinIO bucket for S3 tests
    log.info("")
    log.info("=" * 80)
    log.info("Creating MinIO bucket for S3 tests...")
    log.info("=" * 80)
    
    try {
      // Create bucket using aws CLI with MinIO endpoint
      val createBucketCmd = Seq(
        "docker", "run", "--rm", "--network=host",
        "-e", "AWS_ACCESS_KEY_ID=minioadmin",
        "-e", "AWS_SECRET_ACCESS_KEY=minioadmin",
        "amazon/aws-cli", 
        "--endpoint-url=http://localhost:9000",
        "s3", "mb", "s3://backups"
      )
      val createBucketCode = createBucketCmd.!
      if (createBucketCode == 0) {
        log.info("✓ MinIO bucket 'backups' created successfully")
      } else {
        log.warn(s"MinIO bucket creation exited with code $createBucketCode (may already exist)")
      }
    } catch {
      case e: Exception =>
        log.warn(s"Failed to create MinIO bucket, tests may fail: ${e.getMessage}")
    }
    
    // Set API keys and INTEGRATION flag for tests
    apiKeys.foreach { case (key, value) =>
      sys.props(key) = value
    }
    sys.props("INTEGRATION") = "true"
    
    log.info("")
    log.info("=" * 80)
    log.info("Running integration tests with API keys set...")
    log.info(s"INTEGRATION flag: ${sys.props.get("INTEGRATION")}")
    log.info(s"API keys configured: ${apiKeys.keys.mkString(", ")}")
    log.info("=" * 80)
    log.info("")
    
    // Run integration tests using Process API with environment variables
    import scala.sys.process._
    val envVars = apiKeys.toSeq :+ ("INTEGRATION" -> "true")
    val testCommand = Process(Seq("sbt", "testOnly com.backuparr.integration.*"), None, envVars: _*)
    val testExitCode = testCommand.!
    if (testExitCode != 0) {
      sys.error("Integration tests failed")
    }
    
    log.info("")
    log.info("=" * 80)
    log.info("✓ Integration tests completed successfully")
    log.info("=" * 80)
    
  } catch {
    case e: Exception =>
      log.error(s"Error during integration tests: ${e.getMessage}")
      throw e
  } finally {
    // Always cleanup
    log.info("")
    log.info("=" * 80)
    log.info("Cleaning up docker-compose environment...")
    log.info("=" * 80)
    
    val downExitCode = s"docker compose -f $dockerComposeFile down".!
    if (downExitCode == 0) {
      log.info("✓ Docker-compose stopped successfully")
    } else {
      log.warn(s"⚠ Docker-compose down exited with code $downExitCode")
    }
    
    log.info("=" * 80)
  }
}

// Docker settings (optional, for later)
enablePlugins(JavaAppPackaging, DockerPlugin)
dockerBaseImage := "eclipse-temurin:17-jre-jammy"
dockerExposedPorts := Seq(8080)
Docker / packageName := "backuparr"
Docker / version := version.value
