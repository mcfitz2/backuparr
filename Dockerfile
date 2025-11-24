# Multi-stage Dockerfile for Backuparr
# Stage 1: Build the application with sbt
FROM sbtscala/scala-sbt:eclipse-temurin-jammy-21.0.2_13_1.10.0_3.4.2 AS builder

# Set working directory
WORKDIR /build

# Set JVM memory options for sbt to prevent OOM during build
# Using conservative memory limits that work on systems with 4GB+ RAM
ENV SBT_OPTS="-Xmx1536m -Xms512m -XX:+UseG1GC -XX:ReservedCodeCacheSize=128m -XX:MaxMetaspaceSize=512m"

# Copy build files
COPY project/ project/
COPY build.sbt .

# Download dependencies (cached layer)
# Use -J options to limit JVM memory for this specific command
RUN sbt -J-Xmx1024m update

# Copy source code
COPY src/ src/

# Build fat JAR with sbt assembly
# Split into compile and assembly to reduce memory pressure
RUN sbt -J-Xmx1536m compile && \
    sbt -J-Xmx1536m assembly

# Verify the JAR was created
RUN ls -lh target/scala-3.3.1/ && \
    test -f target/scala-3.3.1/backuparr-assembly-*.jar

# Stage 2: Runtime with minimal distroless image
FROM eclipse-temurin:21-jre-jammy

# Install minimal runtime dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    curl \
    ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user for running the application
RUN groupadd -r backuparr --gid=1000 && \
    useradd -r -g backuparr --uid=1000 --home-dir=/app --shell=/bin/bash backuparr

# Set working directory
WORKDIR /app

# Create necessary directories
RUN mkdir -p /app/config /app/tmp && \
    chown -R backuparr:backuparr /app

# Copy the JAR from builder stage
COPY --from=builder /build/target/scala-3.3.1/backuparr-assembly-*.jar /app/backuparr.jar

# Verify JAR
RUN ls -lh /app/backuparr.jar

# Switch to non-root user
USER backuparr

# Expose health check port
EXPOSE 8080

# Set default environment variables
ENV CONFIG_PATH=/app/config/config.yaml \
    TEMP_DIRECTORY=/app/tmp \
    JAVA_OPTS="-Xmx512m -Xms256m -XX:+UseG1GC -XX:MaxGCPauseMillis=100"

# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -f http://localhost:8080/health/live || exit 1

# Run the application
# Pass config file path as first argument, default to /app/config/config.yaml
ENTRYPOINT ["sh", "-c", "java $JAVA_OPTS -jar /app/backuparr.jar ${CONFIG_PATH:-/app/config/config.yaml}"]
