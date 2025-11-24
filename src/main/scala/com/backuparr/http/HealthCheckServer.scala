package com.backuparr.http

import cats.effect.kernel.{Async, Resource}
import cats.syntax.all.*
import com.backuparr.algebras.HealthCheck
import com.comcast.ip4s.{Host, Port}
import fs2.io.net.Network
import org.http4s.ember.server.EmberServerBuilder
import org.http4s.server.Server
import org.typelevel.log4cats.Logger

/**
 * HTTP server for health check endpoints.
 * 
 * Provides a lightweight HTTP server that serves health check endpoints
 * for Kubernetes liveness/readiness probes and monitoring.
 */
object HealthCheckServer:
  
  /**
   * Create and start the health check HTTP server.
   * 
   * The server runs on the specified host and port, serving health check routes.
   * It will run until the returned Resource is released.
   * 
   * @param healthCheck the health check implementation
   * @param host the host to bind to (default: 0.0.0.0)
   * @param port the port to listen on (default: 8080)
   * @param logger logger for server events
   * @return Resource managing the HTTP server lifecycle
   */
  def resource[F[_]: Async: Network](
    healthCheck: HealthCheck[F],
    host: String = "0.0.0.0",
    port: Int = 8080
  )(using logger: Logger[F]): Resource[F, Server] =
    for
      // Parse host and port
      hostParsed <- Resource.eval(
        Async[F].fromOption(
          Host.fromString(host),
          new IllegalArgumentException(s"Invalid host: $host")
        )
      )
      portParsed <- Resource.eval(
        Async[F].fromOption(
          Port.fromInt(port),
          new IllegalArgumentException(s"Invalid port: $port")
        )
      )
      
      // Create routes
      routes = HealthCheckRoutes.routes[F](healthCheck)
      
      // Build and start server
      _ <- Resource.eval(logger.info(s"Starting health check server on $host:$port"))
      server <- EmberServerBuilder
        .default[F]
        .withHost(hostParsed)
        .withPort(portParsed)
        .withHttpApp(routes.orNotFound)
        .build
      _ <- Resource.eval(logger.info(s"Health check server started successfully"))
      
    yield server
