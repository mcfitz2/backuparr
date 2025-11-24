# Local Development Setup

This directory contains configuration files and Docker Compose setup for local development and testing.

## Files

- **config.example.yaml** - Example configuration template
- **config.yaml** - Your actual configuration (git ignored, create from example)
- **docker-compose.yml** - Docker Compose for local development
- **minio-creds.yaml** - MinIO S3 credentials (git ignored)
- **s3-credentials.example.yaml** - Example S3 credentials template
- **\*-api-key** - API key files (git ignored, one per *arr instance)

## Quick Start

1. Copy example files to create your configuration:
   ```bash
   cd local-setup
   cp config.example.yaml config.yaml
   cp s3-credentials.example.yaml minio-creds.yaml
   ```

2. Start the development environment:
   ```bash
   docker-compose up -d
   ```

3. Get API keys from each *arr service and save to files:
   - Sonarr: http://localhost:8989/settings/general
     ```bash
     echo "YOUR_SONARR_API_KEY" > sonarr-api-key
     ```
   - Radarr: http://localhost:7878/settings/general
     ```bash
     echo "YOUR_RADARR_API_KEY" > radarr-api-key
     ```
   - Lidarr: http://localhost:8686/settings/general
     ```bash
     echo "YOUR_LIDARR_API_KEY" > lidarr-api-key
     ```

4. Update `config.yaml` with your API keys

5. Build and run Backuparr:
   ```bash
   cd ..
   docker build -t backuparr .
   cd local-setup
   docker-compose up -d backuparr
   ```

## Services

The docker-compose.yml includes:

- **MinIO** - S3-compatible object storage
  - Web Console: http://localhost:9001
  - API: http://localhost:9000
  - Default credentials: `minioadmin` / `minioadmin`
  
- **Sonarr** - TV show management
  - URL: http://localhost:8989
  
- **Radarr** - Movie management
  - URL: http://localhost:7878
  
- **Lidarr** - Music management
  - URL: http://localhost:8686

- **Backuparr** - The backup service (commented out by default)
  - URL: http://localhost:8080
  - Health: http://localhost:8080/health/live

## Configuration

Edit `config.yaml` to customize:

- Backup schedules (cron expressions)
- Retention policies
- Instance configurations
- S3 settings

See `config.example.yaml` for all available options and documentation.

## Stopping

```bash
docker-compose down
```

To also remove volumes:
```bash
docker-compose down -v
```
