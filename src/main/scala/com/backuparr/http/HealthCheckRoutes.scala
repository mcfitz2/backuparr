package com.backuparr.http

import cats.effect.kernel.Async
import cats.syntax.all.*
import com.backuparr.algebras.HealthCheck
import io.circe.{Encoder, Json}
import io.circe.syntax.*
import org.http4s.*
import org.http4s.circe.*
import org.http4s.dsl.Http4sDsl

import java.time.Instant

/**
 * HTTP routes for health check endpoints.
 * 
 * Provides three endpoints:
 * - GET /health - Liveness probe (Kubernetes)
 * - GET /ready - Readiness probe (Kubernetes)
 * - GET /status - Detailed status (monitoring)
 */
object HealthCheckRoutes:
  
  /**
   * Create health check routes.
   * 
   * @param healthCheck the health check implementation
   * @return HTTP routes
   */
  def routes[F[_]: Async](healthCheck: HealthCheck[F]): HttpRoutes[F] =
    val dsl = Http4sDsl[F]
    import dsl.*
    
    HttpRoutes.of[F] {
      
      // Liveness probe - always returns 200 if application is alive
      case GET -> Root / "health" =>
        healthCheck.liveness.flatMap { status =>
          if status.alive then
            Ok(status.asJson)
          else
            ServiceUnavailable(status.asJson)
        }
      
      // Readiness probe - returns 200 if ready, 503 if not ready
      case GET -> Root / "ready" =>
        healthCheck.readiness.flatMap { status =>
          if status.ready then
            Ok(status.asJson)
          else
            ServiceUnavailable(status.asJson)
        }
      
      // Detailed status - always returns 200 with comprehensive metrics
      case GET -> Root / "status" =>
        healthCheck.status.flatMap { appStatus =>
          Ok(appStatus.asJson)
        }
    }
  
  // Circe encoders for JSON serialization
  
  given Encoder[Instant] = Encoder.encodeString.contramap[Instant](_.toString)
  
  given Encoder[com.backuparr.domain.HealthStatus] = Encoder.instance { status =>
    Json.obj(
      "alive" -> status.alive.asJson,
      "ready" -> status.ready.asJson,
      "message" -> status.message.asJson,
      "timestamp" -> status.timestamp.asJson
    )
  }
  
  given Encoder[com.backuparr.domain.BackupError] = Encoder.instance {
    case com.backuparr.domain.BackupError.ArrApiError(message, _) =>
      Json.obj("type" -> "ArrApiError".asJson, "message" -> message.asJson)
    case com.backuparr.domain.BackupError.DownloadError(message, _) =>
      Json.obj("type" -> "DownloadError".asJson, "message" -> message.asJson)
    case com.backuparr.domain.BackupError.S3UploadError(message, _) =>
      Json.obj("type" -> "S3UploadError".asJson, "message" -> message.asJson)
    case com.backuparr.domain.BackupError.RetentionError(message, _) =>
      Json.obj("type" -> "RetentionError".asJson, "message" -> message.asJson)
    case com.backuparr.domain.BackupError.ConfigurationError(message) =>
      Json.obj("type" -> "ConfigurationError".asJson, "message" -> message.asJson)
    case com.backuparr.domain.BackupError.TimeoutError(operation) =>
      Json.obj("type" -> "TimeoutError".asJson, "operation" -> operation.asJson)
    case com.backuparr.domain.BackupError.FileSystemError(message, _) =>
      Json.obj("type" -> "FileSystemError".asJson, "message" -> message.asJson)
  }
  
  given Encoder[com.backuparr.domain.BackupStatus] = Encoder.instance {
    case com.backuparr.domain.BackupStatus.Pending =>
      Json.fromString("Pending")
    case com.backuparr.domain.BackupStatus.Requesting =>
      Json.fromString("Requesting")
    case com.backuparr.domain.BackupStatus.Downloading =>
      Json.fromString("Downloading")
    case com.backuparr.domain.BackupStatus.Uploading =>
      Json.fromString("Uploading")
    case com.backuparr.domain.BackupStatus.ApplyingRetention =>
      Json.fromString("ApplyingRetention")
    case com.backuparr.domain.BackupStatus.Completed(uri, timestamp, size) =>
      Json.obj(
        "status" -> "Completed".asJson,
        "uri" -> uri.value.asJson,
        "timestamp" -> timestamp.asJson,
        "size" -> size.asJson
      )
    case com.backuparr.domain.BackupStatus.Failed(error, timestamp) =>
      Json.obj(
        "status" -> "Failed".asJson,
        "error" -> error.asJson,
        "timestamp" -> timestamp.asJson
      )
  }
  
  given Encoder[com.backuparr.domain.InstanceStatus] = Encoder.instance { status =>
    Json.obj(
      "name" -> status.name.asJson,
      "arrType" -> status.arrType.asJson,
      "currentStatus" -> status.currentStatus.asJson,
      "lastSuccessfulBackup" -> status.lastSuccessfulBackup.asJson,
      "lastFailedBackup" -> status.lastFailedBackup.asJson,
      "successCount" -> status.successCount.asJson,
      "failureCount" -> status.failureCount.asJson
    )
  }
  
  given Encoder[com.backuparr.domain.ApplicationStatus] = Encoder.instance { status =>
    Json.obj(
      "health" -> status.health.asJson,
      "schedulerRunning" -> status.schedulerRunning.asJson,
      "instances" -> status.instances.asJson
    )
  }
