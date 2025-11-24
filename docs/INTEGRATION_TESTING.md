# Integration Testing Guide

This guide explains how to run integration tests for Backuparr against real *arr instances.

## Quick Start (Automated)

The easiest way to run integration tests is using the provided sbt task, which **automatically extracts API keys** from the running containers:

```bash
# Simply run the integration tests
sbt integrationTest
```

This single command will:
1. Start docker-compose with all test services (Sonarr, Radarr, Lidarr, MinIO)
2. Wait for services to be healthy
3. **Automatically extract API keys from each container's config.xml**
4. Set API keys as environment variables for the tests
5. Run integration tests with the `INTEGRATION` flag enabled
6. Stop and cleanup docker-compose (even if tests fail)

**No manual API key setup required!** Each test run starts with fresh containers and clean data.

## Manual Setup (If You Want More Control)

### 1. Start Test Containers

```bash
docker-compose -f docker-compose.test.yml up -d
```

This starts:
- **Sonarr** on http://localhost:8989
- **Radarr** on http://localhost:7878
- **Lidarr** on http://localhost:8686
- **MinIO** (S3) on http://localhost:9000

Wait for containers to be healthy:
```bash
docker-compose -f docker-compose.test.yml ps
```

All services should show "healthy" status after ~30 seconds.

### 2. Extract API Keys from Containers

You can manually extract the API keys using docker exec:

```bash
# Extract Sonarr API key
docker exec backuparr-test-sonarr cat /config/config.xml | grep -oP '(?<=<ApiKey>)[^<]+'

# Extract Radarr API key
docker exec backuparr-test-radarr cat /config/config.xml | grep -oP '(?<=<ApiKey>)[^<]+'

# Extract Lidarr API key
docker exec backuparr-test-lidarr cat /config/config.xml | grep -oP '(?<=<ApiKey>)[^<]+'
```

Or use the sbt helper task:
```bash
sbt getApiKeys
```

### 3. Set Environment Variables

```bash
export SONARR_API_KEY="<extracted-key>"
export RADARR_API_KEY="<extracted-key>"
export LIDARR_API_KEY="<extracted-key>"
export INTEGRATION="true"
```

### 4. Run Integration Tests Manually

```bash
sbt test
```

Tests tagged with `Integration` will only run when the `INTEGRATION` environment variable is set to `true`.

### 5. Cleanup

When you're done testing:

```bash
docker-compose -f docker-compose.test.yml down
```

## Stateless Testing

Each test run uses **fresh containers with no persistent data**. The docker-compose configuration does not use volumes, ensuring:

- No leftover data between test runs
- Consistent starting state for every test
- No manual cleanup required

When containers start, they initialize with default settings and generate new API keys. The automated sbt task extracts these keys directly from `/config/config.xml` inside each container.

## Test Services

### Sonarr
- **Port**: 8989
- **API**: http://localhost:8989/api/v3
- **Type**: TV Show management
- **Config**: Auto-generated at container startup

### Radarr
- **Port**: 7878
- **API**: http://localhost:7878/api/v3
- **Type**: Movie management
- **Config**: Auto-generated at container startup

### Lidarr
- **Port**: 8686
- **API**: http://localhost:8686/api/v1
- **Type**: Music management
- **Config**: Auto-generated at container startup

### MinIO (S3)
- **Port**: 9000 (API), 9001 (Console)
- **Access Key**: minioadmin
- **Secret Key**: minioadmin
- **Console**: http://localhost:9001

## Integration Test Features

### Test Coverage

The integration test suite covers:

1. ✅ **System Status** - Fetching system information from each *arr instance
2. ✅ **Backup Creation** - Requesting and monitoring backup creation
3. ✅ **Backup Listing** - Retrieving available backups
4. ✅ **Backup Download** - Streaming backup files
5. ✅ **Error Handling** - Invalid API keys, network failures
6. ✅ **Concurrent Operations** - Multiple simultaneous requests

### Test Tags

Tests are tagged with `Integration` to allow selective running:

```bash
# Run only unit tests (skip integration)
sbt test

# Run integration tests (automated)
sbt integrationTest

# Run specific integration test manually
sbt "testOnly com.backuparr.integration.ArrClientIntegrationSpec -- *Sonarr*"
```

### Automatic Cleanup

The sbt `integrationTest` task automatically:
- Starts fresh containers without persistent volumes
- Extracts API keys from running containers
- Runs all integration tests
- Stops and removes containers (even if tests fail)

## Troubleshooting

### "API key not found" Error

If you see this error, it means the API key wasn't automatically extracted. This can happen if:
- Containers aren't fully initialized yet (wait ~30 seconds)
- Docker isn't running
- Container names don't match expectations

**Solution**: Use the automated `sbt integrationTest` task which handles this automatically.

### Containers Won't Start

```bash
# Check logs
docker-compose -f docker-compose.test.yml logs sonarr

# Check container status
docker-compose -f docker-compose.test.yml ps

# Restart containers
docker-compose -f docker-compose.test.yml restart
```

### API Key Extraction Fails

The automated extraction reads `/config/config.xml` from inside each container and parses the `<ApiKey>` XML element using regex. If this fails:

1. Verify containers are running:
   ```bash
   docker ps | grep backuparr-test
   ```

2. Manually check config file exists:
   ```bash
   docker exec backuparr-test-sonarr cat /config/config.xml | grep ApiKey
   ```

3. Container might still be initializing - wait longer and retry

### Services Not Healthy

Wait longer for initialization:
```bash
# Watch container logs
docker-compose -f docker-compose.test.yml logs -f

# Check health status (should show "healthy")
docker-compose -f docker-compose.test.yml ps
```

Each *arr instance takes ~20-30 seconds to complete initialization.

### Port Conflicts

### Port Conflicts

If ports 8989, 7878, 8686, 9000, or 9001 are already in use:
1. Stop conflicting services
2. Or modify `docker-compose.test.yml` to use different ports

### Backup Timeout

If backups timeout during tests:

1. Check container health:
   ```bash
   docker-compose -f docker-compose.test.yml ps
   ```

2. Check container logs for errors:
   ```bash
   docker-compose -f docker-compose.test.yml logs sonarr
   ```

3. Increase timeout in test if instance is slow (modify test code):
   ```scala
   pollUntilComplete(client, config, backupId, maxAttempts = 120) // 4 minutes
   ```

## Writing New Integration Tests

Integration tests are tagged with `Integration` to prevent them from running in CI:

```scala
test("my integration test".tag(Integration)) {
  // Test code here
}
```

Key patterns:

- Use `getInstanceConfig()` to get test configuration
- Tests check `sys.props` (set by sbt task) or `sys.env` (set manually)
- Use `IO.sleep()` with `.timeout()` for async operations
- Always handle potential failures with `.attempt`

Example:
```scala
test("new feature".tag(Integration)) {
  val config = getInstanceConfig("SONARR")
  
  ArrClientImpl.make[IO](config).use { client =>
    for {
      result <- client.someOperation().timeout(30.seconds)
      _ <- IO(assertEquals(result.status, "success"))
    } yield ()
  }
}
```

## CI/CD Integration

Integration tests are **not** run in CI by default because they require Docker and external services. They are meant for local development and manual verification.

To run in CI, you would need to:
1. Start docker-compose in CI environment
2. Use the `sbt integrationTest` task (handles API key extraction automatically)
3. Ensure sufficient resources (2GB+ memory, 2+ CPUs)
4. Allow ~2 minutes for container initialization and test execution

Example GitHub Actions workflow:

```yaml
# .github/workflows/integration-test.yml
jobs:
  integration-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      
      - name: Setup Java
        uses: actions/setup-java@v3
        with:
          java-version: '17'
          distribution: 'temurin'
      
      - name: Run integration tests
        run: sbt integrationTest
```

The `sbt integrationTest` task handles:
- Starting docker-compose
- Waiting for services to be healthy
- Extracting API keys automatically
- Running tests
- Cleaning up containers

## Additional sbt Tasks

Beyond `integrationTest`, you can use individual tasks for debugging:

```bash
# Start containers and wait for health
sbt dockerComposeUp dockerComposeWaitHealthy

# Extract API keys (prints to console)
sbt getApiKeys

# View container logs
sbt dockerComposeLogs

# Cleanup when done
sbt dockerComposeDown
```

These tasks are useful when debugging integration test failures or developing new tests.

## Stopping Test Environment

```bash
# Stop containers
docker-compose -f docker-compose.test.yml stop

# Stop and remove containers (recommended - no volumes used anyway)
docker-compose -f docker-compose.test.yml down

# Force remove everything
docker-compose -f docker-compose.test.yml down --remove-orphans
```

Since containers don't use persistent volumes, `down` is safe and ensures a completely clean state for the next run.

## MinIO S3 Testing

MinIO is included for future S3 integration tests.

Access MinIO Console:
- URL: http://localhost:9001
- Username: `minioadmin`
- Password: `minioadmin`

Create buckets via console or API for S3Client tests.
        env:
          SONARR_API_KEY: ${{ secrets.TEST_SONARR_API_KEY }}
          RADARR_API_KEY: ${{ secrets.TEST_RADARR_API_KEY }}
          LIDARR_API_KEY: ${{ secrets.TEST_LIDARR_API_KEY }}
      
      - name: Cleanup
        if: always()
        run: docker-compose -f docker-compose.test.yml down -v
```

## Test Data

Test containers store data in Docker volumes:
- `backuparr_sonarr-config`
- `backuparr_radarr-config`
- `backuparr_lidarr-config`
- `backuparr_minio-data`

This persists between runs. To reset:
```bash
docker-compose -f docker-compose.test.yml down -v
```

## Performance Notes

- First backup on a fresh instance may take 30-60 seconds
- Subsequent backups are typically faster (10-20 seconds)
- Tests use polling with 2-second intervals
- Default timeout is 60 attempts (2 minutes)

## Writing New Integration Tests

Template for new integration tests:

```scala
test("New feature - description".tag("integration")) {
  for
    config <- getInstanceConfig("sonarr-test", ArrType.Sonarr, "http://localhost:8989", "SONARR_API_KEY")
    client <- makeClient
    
    // Test logic here
    result <- client.someOperation(config)
    
    // Assertions
    _ = assert(result.isValid)
    
  yield ()
}
```

## Best Practices

1. **Always tag integration tests** with `.tag("integration")`
2. **Clean up resources** in tests (temp files, etc.)
3. **Use meaningful assertions** with clear error messages
4. **Poll with timeouts** for async operations
5. **Handle errors explicitly** - test both success and failure cases

## Support

If you encounter issues:

1. Check the [main README](../README.md) for general setup
2. Review [DESIGN.md](../DESIGN.md) for architecture details
3. Open an issue with:
   - Test output
   - Container logs
   - Environment details
