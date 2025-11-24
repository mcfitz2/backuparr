# Getting Started with Backuparr

This guide will help you get Backuparr up and running quickly.

## Prerequisites

Choose one of the following deployment methods:

### Option 1: Docker (Recommended)
- Docker 20.10+
- Docker Compose 1.29+ (optional but recommended)

### Option 2: From Source
- JDK 21+
- sbt 1.9.0+
- Scala 3.3.1+ (managed by sbt)

## Quick Start with Docker Compose

This is the fastest way to test Backuparr with all services.

### Step 1: Start the Infrastructure

```bash
# Clone the repository
git clone https://github.com/yourusername/backuparr.git
cd backuparr

# Start MinIO, Sonarr, Radarr, and Lidarr
docker-compose up -d
```

Wait ~30 seconds for services to initialize.

### Step 2: Configure *arr Applications

Access each application and complete the initial setup:

1. **Sonarr**: http://localhost:8989
   - Navigate to Settings → General
   - Copy the API Key
   - Save it as `SONARR_API_KEY`

2. **Radarr**: http://localhost:7878
   - Navigate to Settings → General
   - Copy the API Key
   - Save it as `RADARR_API_KEY`

3. **Lidarr**: http://localhost:8686
   - Navigate to Settings → General
   - Copy the API Key
   - Save it as `LIDARR_API_KEY`

4. **MinIO Console**: http://localhost:9001
   - Login: `minioadmin` / `minioadmin`
   - Verify bucket `backups` exists

### Step 3: Create Configuration

```bash
# Copy the example config
cp config.example.yaml config.yaml

# Edit with your API keys (or use environment variables)
nano config.yaml
```

**Option A: Hardcode API keys in config.yaml** (for testing only):
```yaml
instances:
  - name: sonarr
    apiKey: "paste-your-api-key-here"
```

**Option B: Use environment variables** (recommended):
```yaml
instances:
  - name: sonarr
    apiKey: ${SONARR_API_KEY}
```

Then create a `.env` file:
```bash
SONARR_API_KEY=your-actual-key-here
RADARR_API_KEY=your-actual-key-here
LIDARR_API_KEY=your-actual-key-here
MINIO_ACCESS_KEY=minioadmin
MINIO_SECRET_KEY=minioadmin
```

### Step 4: Build and Run Backuparr

```bash
# Build the Docker image
docker build -t backuparr:latest .

# Uncomment the backuparr service in docker-compose.yml
# Then restart
docker-compose up -d backuparr
```

Or run directly:
```bash
docker run -d \
  --name backuparr \
  --network backuparr_default \
  -v $(pwd)/config.yaml:/app/config/config.yaml:ro \
  -v backuparr-tmp:/app/tmp \
  -p 8080:8080 \
  --env-file .env \
  backuparr:latest
```

### Step 5: Verify It's Working

Check health status:
```bash
curl http://localhost:8080/health/status | jq
```

Expected output:
```json
{
  "health": {
    "alive": true,
    "ready": true,
    "message": "Application is healthy"
  },
  "schedulerRunning": true,
  "instances": [
    {
      "name": "sonarr",
      "arrType": "Sonarr",
      "currentStatus": "Pending"
    }
  ]
}
```

Check logs:
```bash
docker logs backuparr
```

You should see:
```
Starting Backuparr...
Loaded configuration from /app/config/config.yaml
Initializing S3 clients
  - Creating S3 client for bucket: primary
Initializing components
Marking application as ready
Starting scheduler
Starting health check server on 0.0.0.0:8080
Application started successfully
```

### Step 6: Trigger a Test Backup

Backups run automatically based on schedule, but you can test immediately:

```bash
# Wait for next scheduled time, or modify schedule in config.yaml:
# schedule: "*/5 * * * *"  # Every 5 minutes for testing
```

Check backup in MinIO:
1. Open http://localhost:9001
2. Login with `minioadmin` / `minioadmin`
3. Browse bucket `backups`
4. Look for `sonarr/`, `radarr/`, `lidarr/` folders
5. Each should contain `.zip` backup files

## Manual Testing

### Trigger Immediate Backup

Since Backuparr doesn't have a manual trigger API (by design), you can:

1. **Set a frequent schedule** temporarily:
   ```yaml
   schedule: "* * * * *"  # Every minute
   ```

2. **Restart** Backuparr:
   ```bash
   docker-compose restart backuparr
   ```

3. **Wait 1-2 minutes** and check logs:
   ```bash
   docker logs -f backuparr
   ```

4. **Verify backup** in MinIO console

5. **Reset schedule** to normal:
   ```yaml
   schedule: "0 2 * * *"  # Daily at 2 AM
   ```

### Test Retention Policy

1. Create multiple backups (use `* * * * *` schedule)
2. Wait for 10+ backups to accumulate
3. Check that old backups are deleted per retention policy:
   ```yaml
   retentionPolicy:
     keepLast: 5  # Should keep only 5 most recent
   ```

## Production Deployment

### Docker in Production

1. **Use a `.env` file** for secrets:
   ```bash
   # .env
   SONARR_API_KEY=secret
   RADARR_API_KEY=secret
   MINIO_ACCESS_KEY=secret
   MINIO_SECRET_KEY=secret
   ```

2. **Run with env file**:
   ```bash
   docker run -d \
     --name backuparr \
     -v $(pwd)/config.yaml:/app/config/config.yaml:ro \
     -v /var/backuparr/tmp:/app/tmp \
     -p 8080:8080 \
     --env-file .env \
     --restart unless-stopped \
     backuparr:latest
   ```

3. **Set up monitoring**:
   - Configure health check endpoint in your monitoring system
   - Alert on `/health/ready` returning non-200
   - Monitor logs for errors

### From Source

```bash
# Build assembly JAR
sbt assembly

# Run
java -jar target/scala-3.3.1/backuparr-assembly-*.jar config.yaml
```

With custom JVM options:
```bash
java -Xmx1g -Xms512m \
  -XX:+UseG1GC \
  -jar backuparr-assembly-*.jar \
  config.yaml
```

## Common Issues

### "Could not detect API version"

**Symptom**: Logs show `ArrApiError(Could not detect API version (tried v3 and v1))`

**Solutions**:
1. Verify *arr application is running: `curl http://sonarr:8989/api/v3/system/status?apikey=YOUR_KEY`
2. Check API key is correct
3. Ensure URL is accessible from Backuparr container
4. Check firewall/network settings

### "Missing required field" in config

**Symptom**: `DecodingFailure at .backuparr.host: Missing required field`

**Solution**: Ensure all required fields are present in config.yaml:
```yaml
backuparr:
  healthCheck:
    enabled: true
    host: "0.0.0.0"  # This field is required
    port: 8080
```

### Backups not running

**Symptom**: No backups appear in S3

**Checks**:
1. Verify scheduler is running: `curl http://localhost:8080/health/status`
2. Check schedule syntax: Must be valid cron expression
3. Ensure current time matches schedule
4. Check logs for errors: `docker logs backuparr`

### Permission denied on temp directory

**Symptom**: `java.nio.file.AccessDeniedException: /app/tmp`

**Solution**: Ensure temp directory is writable:
```bash
# For Docker volume
docker run -v backuparr-tmp:/app/tmp ...

# For host mount
mkdir -p /var/backuparr/tmp
chmod 777 /var/backuparr/tmp  # Or chown to container user
docker run -v /var/backuparr/tmp:/app/tmp ...
```

## Next Steps

1. **Configure retention policies** to match your needs
2. **Set production schedules** (daily at off-peak hours)
3. **Monitor health checks** for failures
4. **Test restore** by downloading a backup from S3 and restoring to *arr
5. **Set up alerts** for backup failures
6. **Document your configuration** for team members

## Advanced Configuration

### Multiple S3 Buckets

You can use different buckets for different instances:

```yaml
s3Buckets:
  - name: production-backups
    provider: Aws
    region: us-east-1
    bucketName: prod-arr-backups
    accessKeyId: ${AWS_ACCESS_KEY_ID}
    secretAccessKey: ${AWS_SECRET_ACCESS_KEY}

  - name: archive-backups
    provider: Backblaze
    region: us-west-002
    bucketName: arr-archives
    endpoint: https://s3.us-west-002.backblazeb2.com
    accessKeyId: ${B2_KEY_ID}
    secretAccessKey: ${B2_APP_KEY}

instances:
  - name: sonarr
    s3Bucket: production-backups  # Uses AWS
  
  - name: old-radarr
    s3Bucket: archive-backups     # Uses Backblaze
```

### Complex Retention Policies

```yaml
retentionPolicy:
  # Keep last 7 backups (always)
  keepLast: 7
  
  # Keep one backup per day for 30 days
  keepDaily: 30
  
  # Keep one backup per week for 12 weeks
  keepWeekly: 12
  
  # Keep one backup per month for 12 months
  keepMonthly: 12
  
  # Keep one backup per year for 3 years
  keepYearly: 3
```

This creates a "grandfather-father-son" rotation scheme.

## Support

- 📝 [Report Issues](https://github.com/yourusername/backuparr/issues)
- 💬 [Discussions](https://github.com/yourusername/backuparr/discussions)
- 📖 [Full Documentation](README.md)

## Success Indicators

You'll know Backuparr is working correctly when:

✅ Health check returns `ready: true`  
✅ Logs show "Starting backup for [instance]"  
✅ S3 bucket contains `.zip` files  
✅ Backup files have correct timestamps  
✅ Old backups are deleted per retention policy  
✅ No errors in logs  

Happy backing up! 🚀
