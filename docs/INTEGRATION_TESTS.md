# Running Integration Tests

This guide explains how to run the integration tests for Backuparr.

## Prerequisites

1. **Docker** and **Docker Compose** installed
2. **sbt** (Scala Build Tool) installed
3. API keys for *arr instances (obtained after setup)

## Quick Start

### 1. Start Test Infrastructure

```bash
# Start all test containers (Sonarr, Radarr, Lidarr, Prowlarr, MinIO)
docker-compose -f docker-compose.test.yml up -d

# Wait for containers to be healthy (check with)
docker-compose -f docker-compose.test.yml ps
```

### 2. Configure *arr Instances

Each *arr instance needs initial setup to generate an API key:

**Sonarr** (http://localhost:8989):
1. Complete initial setup wizard
2. Go to Settings > General
3. Copy the API Key

**Radarr** (http://localhost:7878):
1. Complete initial setup wizard
2. Go to Settings > General
3. Copy the API Key

**Lidarr** (http://localhost:8686):
1. Complete initial setup wizard
2. Go to Settings > General
3. Copy the API Key

**Prowlarr** (http://localhost:9696):
1. Complete initial setup wizard
2. Go to Settings > General
3. Copy the API Key

### 3. Create MinIO Bucket

```bash
# Create the test bucket in MinIO
docker exec backuparr-test-minio mc mb local/backups
```

### 4. Set API Keys

Export the API keys as environment variables:

```bash
export SONARR_API_KEY="your-sonarr-api-key"
export RADARR_API_KEY="your-radarr-api-key"
export LIDARR_API_KEY="your-lidarr-api-key"
export PROWLARR_API_KEY="your-prowlarr-api-key"
```

Or create a `.env` file:

```bash
# .env file
SONARR_API_KEY=your-sonarr-api-key
RADARR_API_KEY=your-radarr-api-key
LIDARR_API_KEY=your-lidarr-api-key
PROWLARR_API_KEY=your-prowlarr-api-key
```

Then source it:

```bash
source .env
```

### 5. Run Integration Tests

```bash
# Run all integration tests
sbt integrationTest

# Or run specific test suite
sbt "testOnly com.backuparr.integration.BackupManagerIntegrationSpec"
```

## Test Suites

### ArrClient Integration Tests (6 tests)
Tests backup operations against real *arr instances:
- Request backup and check status (Sonarr, Radarr, Lidarr, Prowlarr, Readarr, Whisparr)
- Download backup files
- Verify backup completion

### S3Client Integration Tests (6 tests)
Tests S3 operations against MinIO:
- Upload files
- List objects
- Download files
- Delete objects
- Verify object existence
- Handle errors gracefully

### BackupManager Integration Tests (7 tests)
Tests complete backup workflow:
- **Sonarr complete workflow**: Request → Download → Upload → Retention
- **Radarr complete workflow**: Full backup cycle
- **Lidarr complete workflow**: Full backup cycle
- **Prowlarr complete workflow**: Full backup cycle
- **Retention policy**: Create 5 backups, verify only 3 kept (keepLast = 3)
- **Concurrent backups**: Multiple instances backed up in parallel
- **Status tracking**: Verify status updates during backup

### E2E Integration Tests (3 comprehensive tests)
Tests the complete system end-to-end:
- **Multiple instances with retention**: 
  - Creates 5 backups for Sonarr (keepLast=3), Radarr (keepLast=2), Lidarr (keepLast=4)
  - Verifies retention policies applied correctly
  - Validates backup metadata (instance-name, arr-type)
  - Tests adding more backups maintains retention limits
  - Verifies status tracking across all instances
  - Tests concurrent backup execution
  
- **Scheduler-driven backups**:
  - Simulates scheduler orchestrating backups
  - Tests scheduler start/stop lifecycle
  - Verifies retention works with scheduled backups
  - Tests concurrent scheduling of multiple instances
  
- **Different retention policies**:
  - Tests keepLast-only policy
  - Tests combined policies (keepLast + keepDaily)
  - Verifies policy union semantics (keeps backups matching ANY policy)

## Automated Setup (for CI/CD)

The sbt task `integrationTest` automatically:
1. Starts docker-compose if needed
2. Extracts API keys from running containers
3. Creates MinIO bucket
4. Runs all integration tests
5. Cleans up on failure

```bash
# One command to run everything
sbt integrationTest
```

## Cleanup

```bash
# Stop and remove containers
docker-compose -f docker-compose.test.yml down

# Remove volumes (to start fresh)
docker-compose -f docker-compose.test.yml down -v
```

## Troubleshooting

### "Connection refused" errors
- Verify containers are running: `docker-compose -f docker-compose.test.yml ps`
- Check health status: All services should show "healthy"
- Restart containers if needed: `docker-compose -f docker-compose.test.yml restart`

### "API key not found" errors
- Ensure environment variables are set: `echo $SONARR_API_KEY`
- Verify API keys are correct (check in web UI)
- Re-export variables in current shell

### "Bucket does not exist" errors
- Create MinIO bucket: `docker exec backuparr-test-minio mc mb local/backups`
- Verify bucket exists: `docker exec backuparr-test-minio mc ls local/`

### Tests timeout
- Increase timeout in tests (currently 60 seconds)
- Check container logs: `docker logs backuparr-test-sonarr`
- Verify containers have enough resources

## MinIO Web UI

Access MinIO console for debugging:
- URL: http://localhost:9001
- Username: `minioadmin`
- Password: `minioadmin`

You can browse uploaded backups, check metadata, and manually delete objects.

## Running Specific Tests

```bash
# Run only BackupManager tests
sbt "testOnly com.backuparr.integration.BackupManagerIntegrationSpec"

# Run only S3Client tests
sbt "testOnly com.backuparr.integration.S3ClientIntegrationSpec"

# Run only ArrClient tests
sbt "testOnly com.backuparr.integration.ArrClientIntegrationSpec"

# Run only E2E tests (full system)
sbt "testOnly com.backuparr.integration.E2EIntegrationSpec"

# Run a specific test within a suite
sbt "testOnly com.backuparr.integration.BackupManagerIntegrationSpec -- -z 'complete backup workflow'"

# Run a specific E2E test
sbt "testOnly com.backuparr.integration.E2EIntegrationSpec -- -z 'Multiple instances'"
```

## Test Coverage

Current integration test coverage:

- ✅ **ArrClient**: All 6 *arr types (Sonarr, Radarr, Lidarr, Prowlarr, Readarr, Whisparr)
- ✅ **S3Client**: All core operations (upload, download, list, delete)
- ✅ **BackupManager**: Complete workflow for 4 *arr types
- ✅ **RetentionManager**: Policy evaluation and application
- ✅ **Scheduler**: Lifecycle management (start/stop)
- ✅ **E2E System**: Multi-instance backups with retention policies
- ✅ **Concurrent Operations**: Multiple backups in parallel
- ✅ **Error Handling**: Graceful failure scenarios

**Total: 16 integration tests covering end-to-end workflows**
- 6 ArrClient tests
- 6 S3Client tests  
- 7 BackupManager tests
- 3 E2E system tests
