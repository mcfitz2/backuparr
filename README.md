# Backuparr

> Automated backup management for *arr applications with S3 storage

[![Scala Version](https://img.shields.io/badge/scala-3.3.1-red.svg)](https://www.scala-lang.org/)
[![Cats Effect](https://img.shields.io/badge/cats--effect-3.5.2-blue.svg)](https://typelevel.org/cats-effect/)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![CI/CD](https://github.com/yourusername/backuparr/workflows/CI%2FCD/badge.svg)](https://github.com/yourusername/backuparr/actions)

## Overview

Backuparr is a production-ready Scala 3 application that automates the complete backup lifecycle for *arr applications (Sonarr, Radarr, Lidarr, Readarr, Prowlarr, Whisparr). It creates backups through the native *arr APIs, downloads them, uploads to S3-compatible storage, and applies sophisticated retention policies.

### Key Features

- 🚀 **Multi-instance Support**: Backup multiple *arr instances with independent schedules
- 📦 **S3 Compatible**: Works with AWS S3, MinIO, Backblaze B2, Wasabi, and other S3-compatible services
- ☸️ **Kubernetes Native**: Designed for Kubernetes with health checks, graceful shutdown, and secret management
- ⏰ **Flexible Scheduling**: Cron-based scheduling per instance (daily, hourly, custom)
- 🗑️ **Intelligent Retention**: Grandfather-father-son rotation with keepLast, keepDaily, keepWeekly, keepMonthly, keepYearly
- 🔒 **Secure**: Environment variable substitution for secrets, non-root Docker user
- 🎯 **Pure Functional**: Built with Cats Effect 3, http4s, fs2 - 100% functional programming
- 📊 **Observable**: Health check endpoints for liveness/readiness probes, detailed status reporting
- ✅ **Well Tested**: 51 tests (35 unit + 16 integration) with comprehensive E2E validation
- 📚 **Production Ready**: Graceful shutdown, comprehensive logging, error handling

## Table of Contents

- [Quick Start](#quick-start)
- [Installation](#installation)
  - [Docker](#docker)
  - [Docker Compose](#docker-compose)
  - [From Source](#from-source)
- [Configuration](#configuration)
  - [Basic Example](#basic-example)
  - [Environment Variables](#environment-variables)
  - [Retention Policies](#retention-policies)
  - [Cron Schedules](#cron-schedules)
- [Deployment](#deployment)
  - [Kubernetes](#kubernetes)
  - [Docker](#docker-1)
  - [Standalone](#standalone)
- [Health Checks](#health-checks)
- [Development](#development)
- [Architecture](#architecture)
- [Contributing](#contributing)
- [License](#license)

## Quick Start

### Docker Compose (Recommended for Testing)

1. Clone the repository:
```bash
git clone https://github.com/yourusername/backuparr.git
cd backuparr
```

2. Start the test environment:
```bash
docker-compose up -d
```

This starts MinIO (S3), Sonarr, Radarr, and Lidarr for testing.

3. Get API keys from each *arr service:
```bash
# Sonarr: http://localhost:8989/settings/general
# Radarr: http://localhost:7878/settings/general
# Lidarr: http://localhost:8686/settings/general
```

4. Create `local-setup/config.yaml`:
```bash
cd local-setup
cp config.example.yaml config.yaml
# Edit config.yaml with your API keys
```

5. Build and run Backuparr:
```bash
docker build -t backuparr .
# Uncomment backuparr service in local-setup/docker-compose.yml
cd local-setup
docker-compose up -d backuparr
```

6. Check health:
```bash
curl http://localhost:8080/health/status
```

## Installation

### Docker

Pull the latest image:
```bash
docker pull ghcr.io/yourusername/backuparr:latest
```

Run with a config file:
```bash
docker run -d \
  --name backuparr \
  -v $(pwd)/local-setup/config.yaml:/app/config/config.yaml:ro \
  -v backuparr-tmp:/app/tmp \
  -p 8080:8080 \
  -e SONARR_API_KEY=your-key \
  -e MINIO_ACCESS_KEY=minioadmin \
  -e MINIO_SECRET_KEY=minioadmin \
  ghcr.io/yourusername/backuparr:latest
```

### Docker Compose

See [local-setup/docker-compose.yml](local-setup/docker-compose.yml) for a complete example with MinIO and *arr services.

### From Source

Requirements:
- JDK 21+
- sbt 1.9.0+

```bash
# Clone repository
git clone https://github.com/yourusername/backuparr.git
cd backuparr

# Compile
sbt compile

# Run unit tests
sbt "testOnly -- --exclude-tags=integration"

# Build fat JAR
sbt assembly

# Run
java -jar target/scala-3.3.1/backuparr-assembly-*.jar local-setup/config.yaml
```

## Configuration

### Basic Example

Create `local-setup/config.yaml` based on [local-setup/config.example.yaml](local-setup/config.example.yaml):

```yaml
backuparr:
  maxConcurrentBackups: 3
  tempDirectory: /tmp/backuparr
  healthCheck:
    enabled: true
    host: "0.0.0.0"
    port: 8080
  logging:
    level: INFO

s3Buckets:
  - name: primary
    provider: Minio
    endpoint: http://minio:9000
    region: us-east-1
    bucketName: backups
    accessKeyId: ${MINIO_ACCESS_KEY}
    secretAccessKey: ${MINIO_SECRET_KEY}
    pathStyleAccess: true

instances:
  - name: sonarr
    arrType: Sonarr
    url: http://sonarr:8989
    apiKey: ${SONARR_API_KEY}
    s3Bucket: primary
    s3Prefix: sonarr/
    schedule: "0 2 * * *"  # Daily at 2 AM
    retentionPolicy:
      keepLast: 7
      keepDaily: 14
      keepWeekly: 8
      keepMonthly: 6
```

### Environment Variables

Use `${VAR_NAME}` syntax for sensitive data:

```yaml
apiKey: ${SONARR_API_KEY}
accessKeyId: ${MINIO_ACCESS_KEY}
secretAccessKey: ${MINIO_SECRET_KEY}
```

Set in Docker:
```bash
docker run -e SONARR_API_KEY=abc123 ...
```

Or in Kubernetes Secret:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: backuparr-secrets
type: Opaque
stringData:
  SONARR_API_KEY: "your-api-key"
  MINIO_ACCESS_KEY: "minioadmin"
  MINIO_SECRET_KEY: "minioadmin"
```

### Retention Policies

Retention policies use **union semantics** - a backup is kept if it matches ANY rule:

```yaml
retentionPolicy:
  keepLast: 7        # Always keep 7 most recent backups
  keepDaily: 14      # Keep one backup per day for 14 days
  keepWeekly: 8      # Keep one backup per week for 8 weeks
  keepMonthly: 6     # Keep one backup per month for 6 months
  keepYearly: 2      # Keep one backup per year for 2 years
```

**Examples:**

- Simple: Just `keepLast: 5` - Keep last 5 backups
- Grandfather-Father-Son: All fields - Comprehensive rotation
- Daily only: `keepDaily: 30` - 30 days of daily backups

### Cron Schedules

Format: `minute hour day-of-month month day-of-week`

**Examples:**
```yaml
schedule: "0 2 * * *"        # Daily at 2:00 AM
schedule: "*/15 * * * *"     # Every 15 minutes
schedule: "0 */6 * * *"      # Every 6 hours
schedule: "0 0 * * 0"        # Weekly on Sunday at midnight
schedule: "0 0 1 * *"        # Monthly on the 1st at midnight
schedule: "0 2 * * 1-5"      # Weekdays at 2 AM
```

## Deployment

### Kubernetes

*Kubernetes manifests available in a future update. For now, use Docker or standalone deployment.*

Key considerations:
- Mount config as ConfigMap
- Mount secrets as Secret
- Use liveness/readiness probes
- Ensure temp directory is writable (emptyDir or PVC)

### Docker

**Build:**
```bash
docker build -t backuparr:latest .
```

**Run:**
```bash
docker run -d \
  --name backuparr \
  -v $(pwd)/local-setup/config.yaml:/app/config/config.yaml:ro \
  -v backuparr-tmp:/app/tmp \
  -p 8080:8080 \
  backuparr:latest
```

**Custom Java options:**
```bash
docker run -d \
  -e JAVA_OPTS="-Xmx1g -Xms512m" \
  backuparr:latest
```

### Standalone

```bash
# Build assembly
sbt assembly

# Run with config
java -jar target/scala-3.3.1/backuparr-assembly-*.jar local-setup/config.yaml

# With custom JVM options
java -Xmx1g -Xms512m -jar backuparr-assembly-*.jar local-setup/config.yaml
```

## Health Checks

Backuparr exposes three health check endpoints on port 8080:

### Endpoints

- **`GET /health/live`** - Liveness probe (returns 200 if application is running)
- **`GET /health/ready`** - Readiness probe (returns 200 when ready to accept traffic)
- **`GET /health/status`** - Detailed status with instance information

### Readiness Conditions

Application is ready when:
- ✅ Startup complete
- ✅ At least one instance configured
- ✅ At least one S3 bucket configured
- ✅ Scheduler is running

### Example Responses

**Liveness:**
```json
{
  "alive": true,
  "ready": true,
  "message": "Application is healthy",
  "timestamp": "2025-11-23T22:00:00Z"
}
```

**Status:**
```json
{
  "health": {
    "alive": true,
    "ready": true,
    "message": "Application is healthy",
    "timestamp": "2025-11-23T22:00:00Z"
  },
  "schedulerRunning": true,
  "instances": [
    {
      "name": "sonarr",
      "arrType": "Sonarr",
      "currentStatus": "Completed",
      "lastSuccessfulBackup": "2025-11-23T02:00:00Z",
      "successCount": 7,
      "failureCount": 0
    }
  ]
}
```

### Docker Healthcheck

Included in Dockerfile:
```dockerfile
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -f http://localhost:8080/health/live || exit 1
```

### Kubernetes Probes

```yaml
livenessProbe:
  httpGet:
    path: /health/live
    port: 8080
  initialDelaySeconds: 30
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /health/ready
    port: 8080
  initialDelaySeconds: 10
  periodSeconds: 5
  timeoutSeconds: 3
  failureThreshold: 3
```

## Development

### Project Structure

```
backuparr/
├── src/
│   ├── main/scala/com/backuparr/
│   │   ├── algebras/          # Interfaces/traits
│   │   ├── config/            # Configuration models
│   │   ├── domain/            # Domain models
│   │   ├── http/              # HTTP routes and server
│   │   ├── impl/              # Implementations
│   │   └── Main.scala         # Application entry point
│   └── test/scala/com/backuparr/
│       ├── config/            # Config tests
│       ├── impl/              # Unit tests
│       └── integration/       # Integration tests
├── build.sbt                  # Build configuration
├── docs/                      # Documentation
├── local-setup/               # Local development setup
│   ├── config.example.yaml    # Example configuration
│   ├── config.yaml            # Your configuration (git ignored)
│   ├── docker-compose.yml     # Docker compose for local dev
│   ├── minio-creds.yaml       # MinIO credentials
│   └── s3-credentials.example.yaml
├── test-setup/                # Integration test setup
│   └── docker-compose.yml     # Docker compose for tests
└── Dockerfile                 # Production container image
```

### Running Tests

```bash
# Unit tests only (fast)
sbt "testOnly -- --exclude-tags=integration"

# All tests (integration tests managed by sbt)
sbt integrationTest

# Specific test
sbt "testOnly com.backuparr.impl.CronExpressionSpec"

# With coverage
sbt coverage test coverageReport
```

### Code Style

This project follows functional programming principles:
- Pure functions
- Immutable data structures  
- Effect tracking with `IO`
- Resource management with `Resource`
- Error handling with `Either` and typed errors

See [.github/copilot-instructions.md](.github/copilot-instructions.md) for detailed guidelines.

## Architecture

### Technology Stack

- **Scala 3.3.1** - Modern, type-safe programming language
- **Cats Effect 3.5.2** - Pure functional effects and concurrency
- **http4s 0.23.23** - Functional HTTP client/server
- **fs2 3.9.3** - Functional streaming and scheduling
- **Circe** - JSON encoding/decoding
- **AWS SDK 2.x** - S3 operations
- **log4cats** - Structured logging

### Core Components

1. **Main** - Application entry point with Resource-based lifecycle
2. **ArrClient** - HTTP client for *arr APIs (backup request, status check, download)
3. **S3Client** - S3 operations (upload, delete, list, metadata)
4. **BackupManager** - Orchestrates backup workflow (request → download → upload → retention)
5. **RetentionManager** - Applies retention policies to existing backups
6. **Scheduler** - Cron-based scheduling for automated backups
7. **HealthCheck** - HTTP endpoints for Kubernetes probes

### Backup Workflow

```
1. Scheduler triggers backup based on cron schedule
2. ArrClient requests backup from *arr API
3. ArrClient polls status until backup ready
4. ArrClient downloads backup file to temp directory
5. S3Client uploads backup to S3 bucket
6. RetentionManager applies retention policy
7. S3Client deletes old backups per retention rules
8. Cleanup: Delete temp file
```

### Concurrency Model

- Configurable max concurrent backups (`maxConcurrentBackups`)
- Each backup runs in its own fiber
- Scheduler uses semaphore to limit concurrency
- Graceful shutdown cancels all in-flight backups

## Contributing

Contributions welcome! Please:

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Follow the coding style in `.github/copilot-instructions.md`
4. Add tests for new functionality
5. Ensure all tests pass (`sbt test`)
6. Commit with clear messages
7. Push to your fork
8. Open a Pull Request

### Development Setup

```bash
git clone https://github.com/yourusername/backuparr.git
cd backuparr
sbt compile
docker-compose up -d
sbt "testOnly -- --exclude-tags=integration"
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- Built with [Typelevel](https://typelevel.org/) libraries
- Inspired by the need for reliable *arr backups
- Thanks to the Scala and functional programming communities

---

**Note:** This is a learning project demonstrating production-quality Scala 3 and functional programming. All code is extensively documented for educational purposes.

s3Buckets:
  - name: backups-sonarr
    provider: aws
    region: us-east-1
    bucket: my-backups
    credentialsFile: /secrets/s3-creds.yaml
```

### Running Locally

```bash
# Build the project
sbt compile

# Run unit tests
sbt test

# Run integration tests (fully automated - no manual setup required!)
sbt integrationTest

# Run the application
sbt run
```

### Integration Testing

Backuparr includes comprehensive integration tests that run against real *arr instances. The `integrationTest` sbt task **fully automates** the entire process:

```bash
# Simply run this command - no manual setup required!
sbt integrationTest
```

This single command will:
1. Start docker-compose with fresh Sonarr, Radarr, Lidarr, and MinIO containers
2. Wait for services to be healthy (~30 seconds)
3. **Automatically extract API keys** from each container's config file
4. Run all integration tests with proper configuration
5. Stop and cleanup docker-compose (even if tests fail)

**No manual API key setup needed!** Each test run uses fresh containers with clean data.

**Available sbt tasks:**
- `integrationTest` - Run full integration test suite (automated)
- `dockerComposeUp` - Start test environment manually
- `dockerComposeDown` - Stop test environment
- `getApiKeys` - Extract and display API keys from running containers
- `dockerComposeLogs` - View docker-compose logs
- `dockerComposeWaitHealthy` - Wait for services to be ready
- `integrationTest` - Full integration test cycle with automatic cleanup

For more details, see [INTEGRATION_TESTING.md](INTEGRATION_TESTING.md).

### Docker Deployment

```bash
# Build Docker image
sbt docker:publishLocal

# Run with Docker
docker run -v /path/to/config.yaml:/app/config.yaml backuparr:latest
```

### Kubernetes Deployment

See [kubernetes/](kubernetes/) directory for example manifests.

## Documentation

- [Design Document](DESIGN.md) - Architecture and design decisions
- [Copilot Instructions](.github/copilot-instructions.md) - Development guidelines
- [API Documentation](docs/API.md) - API reference (coming soon)
- [Configuration Guide](docs/CONFIGURATION.md) - Detailed configuration options (coming soon)

## Architecture

Backuparr follows a pure functional architecture using Cats Effect:

```
Application
├── Configuration Manager (YAML + K8s Secrets)
├── Scheduler (Cron-based)
├── Backup Manager (Orchestration)
├── *arr Client (HTTP API)
├── S3 Client (Upload/Download)
└── Retention Manager (Policy enforcement)
```

See [DESIGN.md](DESIGN.md) for detailed architecture documentation.

## Supported *arr Applications

- ✅ Sonarr
- ✅ Radarr
- ✅ Lidarr
- ✅ Prowlarr
- ✅ Readarr
- ✅ Any other *arr app with compatible API

## Supported S3 Providers

- ✅ Amazon S3
- ✅ MinIO
- ✅ Backblaze B2
- ✅ Any S3-compatible storage

## Development

This project is also a learning exercise in functional programming with Scala 3 and Cats Effect. The codebase includes extensive comments and documentation.

### Building from Source

```bash
git clone https://github.com/yourusername/backuparr.git
cd backuparr
sbt compile
```

### Running Tests

```bash
sbt test
```

### Code Style

This project follows pure functional programming principles:
- Immutable data structures
- No side effects (isolated in IO)
- Tagless final for abstractions
- Resource-safe operations

See [Copilot Instructions](.github/copilot-instructions.md) for detailed coding standards.

## Contributing

Contributions are welcome! Please read our contributing guidelines (coming soon) and submit pull requests.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Roadmap

### Current Version (v0.1.0)
- Basic backup functionality
- S3 upload support
- Configurable scheduling
- Retention policies

### Future Enhancements
- Backup verification
- Encryption support
- Prometheus metrics
- Web UI
- Restore functionality
- Multi-region replication

## Support

- 📝 [Report Issues](https://github.com/yourusername/backuparr/issues)
- 💬 [Discussions](https://github.com/yourusername/backuparr/discussions)
- 📖 [Documentation](https://github.com/yourusername/backuparr/wiki)

## Acknowledgments

- Built with [Cats Effect](https://typelevel.org/cats-effect/)
- Inspired by the *arr application ecosystem
- Thanks to the Typelevel community

---

**Made with ❤️ and pure functional programming**
