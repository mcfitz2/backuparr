# Integration Test Setup

This directory contains the Docker Compose configuration for running integration tests.

## Files

- **docker-compose.yml** - Docker Compose configuration for test infrastructure
  - MinIO (S3-compatible storage)
  - Sonarr (TV shows)
  - Radarr (Movies)
  - Lidarr (Music)
  - Prowlarr (Indexer manager)

## Usage

The integration tests are automatically managed by sbt. Simply run:

```bash
sbt integrationTest
```

This will:
1. Start the Docker Compose test environment
2. Wait for services to be healthy
3. Extract API keys from containers
4. Run the integration tests
5. Clean up the test environment

## Manual Management

If you need to manually manage the test environment:

```bash
# Start
docker-compose -f test-setup/docker-compose.yml up -d

# Check status
docker-compose -f test-setup/docker-compose.yml ps

# View logs
docker-compose -f test-setup/docker-compose.yml logs -f

# Stop
docker-compose -f test-setup/docker-compose.yml down -v
```

## Services

All services run on localhost:

- **MinIO**: http://localhost:9000 (Console: http://localhost:9001)
  - Access Key: `minioadmin`
  - Secret Key: `minioadmin`
- **Sonarr**: http://localhost:8989
- **Radarr**: http://localhost:7878
- **Lidarr**: http://localhost:8686
- **Prowlarr**: http://localhost:9696
The integration tests verify that Backuparr correctly handles URL redirects and includes base paths in API calls.

API keys are automatically extracted by the test suite.
