# Changelog

All notable changes to the Backuparr project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added

#### Integration Test Automation (2024-11-23)
- **Automatic API Key Extraction**: Integration tests no longer require manual API key setup
  - Added `getApiKeys` sbt task that extracts API keys from running containers
  - Reads `/config/config.xml` from each container using `docker exec`
  - Parses XML using regex pattern: `<ApiKey>([a-f0-9]+)</ApiKey>`
  - Automatically sets system properties for test execution
  
- **Stateless Container Testing**: Removed all persistent volumes from docker-compose.test.yml
  - Each test run starts with fresh containers and clean data
  - No leftover data between test runs
  - Ensures consistent, reproducible test environment
  - Containers use ephemeral storage only
  
- **Enhanced Integration Test Workflow**:
  - Single command `sbt integrationTest` runs entire test suite
  - Automatically starts docker-compose
  - Waits for services to be healthy
  - Extracts API keys from containers
  - Runs all integration tests
  - Cleans up containers (even on failure)
  
- **Test Configuration Improvements**:
  - Updated `ArrClientIntegrationSpec` to check both `sys.props` (automated) and `sys.env` (manual)
  - Better error messages explaining how to run tests
  - Support for both automated sbt task and manual testing workflows

### Changed

#### Docker Compose Configuration
- Removed volume declarations from docker-compose.test.yml:
  - sonarr-config, sonarr-downloads, sonarr-tv
  - radarr-config, radarr-downloads, radarr-movies
  - lidarr-config, lidarr-downloads, lidarr-music
  - minio-data
  - Removed entire `volumes:` section
  
#### Build Configuration
- Added `getApiKeys` taskKey to build.sbt
- Implemented API key extraction logic in build.sbt
- Modified `integrationTest` task to call `getApiKeys` and set system properties
- Added error handling for missing containers

#### Documentation
- Updated INTEGRATION_TESTING.md:
  - Documented automated API key extraction
  - Explained stateless container approach
  - Simplified quick start instructions
  - Added troubleshooting for API key extraction
  - Updated CI/CD integration examples
  
- Updated README.md:
  - Simplified integration test instructions
  - Removed manual API key setup requirements
  - Highlighted automated workflow

### Technical Details

#### API Key Extraction Implementation
```scala
def extractApiKey(containerName: String): Option[String] = {
  try {
    val result = s"docker exec $containerName cat /config/config.xml".!!
    val apiKeyPattern = "<ApiKey>([a-f0-9]+)</ApiKey>".r
    apiKeyPattern.findFirstMatchIn(result).map(_.group(1))
  } catch {
    case e: Exception =>
      println(s"[warn] Failed to extract API key from $containerName: ${e.getMessage}")
      None
  }
}
```

Container to environment variable mapping:
- `backuparr-test-sonarr` → `SONARR_API_KEY`
- `backuparr-test-radarr` → `RADARR_API_KEY`
- `backuparr-test-lidarr` → `LIDARR_API_KEY`

#### Benefits of Stateless Testing
1. **Reproducibility**: Each test run starts from identical state
2. **No Manual Cleanup**: Containers are ephemeral, no data persists
3. **Faster Setup**: No need to manually configure instances or extract keys
4. **CI-Ready**: Can be easily integrated into CI/CD pipelines
5. **Isolation**: Tests don't interfere with each other through shared state

## Previous Work

### Project Foundation
- Set up Scala 3.3.1 + sbt 1.9.7 build
- Configured Cats Effect 3.5.2, http4s, fs2, Circe dependencies
- Created pure functional domain models with opaque types
- Implemented YAML configuration system with validation
- Defined tagless final algebras for core components

### ArrClient Implementation
- Created HTTP-based ArrClient implementation
- Added retry logic with exponential backoff
- Implemented streaming download support
- Built comprehensive error handling
- Created 6 integration tests covering all major operations

### Integration Test Infrastructure
- Set up docker-compose with linuxserver/*arr images
- Added MinIO for S3 testing
- Created sbt tasks for docker-compose lifecycle management
- Implemented health checks and wait logic
