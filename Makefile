.PHONY: generate generate-sonarr generate-radarr build clean test-integration test-containers-up test-containers-down tools test-s3 test-s3-up test-s3-down test-unit

GOBIN := $(shell go env GOPATH)/bin

# Generate all API clients
generate: generate-sonarr generate-radarr

# Generate Sonarr API client
generate-sonarr:
	@echo "Generating Sonarr API client..."
	cd sonarr && go generate

# Generate Radarr API client
generate-radarr:
	@echo "Generating Radarr API client..."
	cd radarr && go generate

# Build the application
build:
	go build -o backuparr .

# Clean build artifacts
clean:
	rm -f backuparr

# Run the application
run: build
	./backuparr

# Tidy dependencies
tidy:
	go mod tidy

# Install required tools
tools:
	@echo "Installing gotestfmt..."
	go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@latest

# Start test containers (PostgreSQL + Sonarr/Radarr instances)
test-containers-up:
	@echo "Starting test containers..."
	cd integration-tests && docker compose up -d
	@echo "Waiting for containers to be healthy (up to 2 minutes)..."
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12; do \
		HEALTHY=$$(docker inspect --format='{{.State.Health.Status}}' sonarr-sqlite sonarr-postgres radarr-sqlite radarr-postgres 2>/dev/null | grep -c healthy); \
		if [ "$$HEALTHY" -eq 4 ]; then \
			echo "All containers are healthy!"; \
			break; \
		fi; \
		echo "Waiting... ($$HEALTHY/4 healthy)"; \
		sleep 10; \
	done
	@docker ps --format "table {{.Names}}\t{{.Status}}" | grep -E "(sonarr|radarr)"

# Stop test containers and remove volumes
test-containers-down:
	@echo "Stopping test containers and removing volumes..."
	cd integration-tests && docker compose down -v

# Setup test containers (create required directories after containers are healthy)
test-containers-setup:
	@echo "Setting up container directories..."
	@# Create media directories in all containers
	docker exec sonarr-sqlite mkdir -p /tv && docker exec sonarr-sqlite chmod 777 /tv
	docker exec sonarr-postgres mkdir -p /tv && docker exec sonarr-postgres chmod 777 /tv
	docker exec radarr-sqlite mkdir -p /movies && docker exec radarr-sqlite chmod 777 /movies
	docker exec radarr-postgres mkdir -p /movies && docker exec radarr-postgres chmod 777 /movies
	@echo "Container setup complete!"

# Run integration tests (clears volumes, starts fresh containers, sets up directories)
test-integration: test-containers-down test-containers-up test-containers-setup
	@echo "Running integration tests..."
	set -o pipefail; INTEGRATION_TEST=1 go test -json -v ./integration-tests/... -timeout 30m 2>&1 | $(GOBIN)/gotestfmt

# Run only validation tests (backup/restore with data verification)
test-validation: test-containers-down test-containers-up test-containers-setup
	@echo "Running validation tests..."
	set -o pipefail; INTEGRATION_TEST=1 go test -json -v -run "TestRestoreValidation" ./integration-tests/... -timeout 30m 2>&1 | $(GOBIN)/gotestfmt

# Run quick tests (skip validation tests which are slower)
test-quick: test-containers-down test-containers-up test-containers-setup
	@echo "Running quick integration tests..."
	set -o pipefail; INTEGRATION_TEST=1 go test -json -v -run "^Test(Backup|Client|Restore[^V])" ./integration-tests/... -timeout 15m 2>&1 | $(GOBIN)/gotestfmt

# Run unit tests (no containers needed)
test-unit:
	@echo "Running unit tests..."
	set -o pipefail; go test -json -v ./storage/... 2>&1 | $(GOBIN)/gotestfmt

# Start MinIO container for S3 tests
test-s3-up:
	@echo "Starting MinIO container..."
	@docker run -d --name minio-test \
		-p 9000:9000 -p 9001:9001 \
		-e MINIO_ROOT_USER=minioadmin \
		-e MINIO_ROOT_PASSWORD=minioadmin \
		minio/minio server /data --console-address ":9001"
	@echo "Waiting for MinIO to be ready..."
	@for i in 1 2 3 4 5 6; do \
		if curl -sf http://localhost:9000/minio/health/ready > /dev/null 2>&1; then \
			echo "MinIO is ready!"; \
			break; \
		fi; \
		echo "Waiting..."; \
		sleep 2; \
	done

# Stop MinIO container
test-s3-down:
	@echo "Stopping MinIO container..."
	-@docker rm -f minio-test 2>/dev/null

# Run S3 integration tests (starts MinIO, runs tests, stops MinIO)
test-s3: test-s3-down test-s3-up
	@echo "Running S3 integration tests..."
	set -o pipefail; S3_TEST=1 go test -json -v ./storage/s3/... -timeout 5m 2>&1 | $(GOBIN)/gotestfmt
	@$(MAKE) test-s3-down
