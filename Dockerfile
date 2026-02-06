# ---- Build stage ----
# Build on native arch for speed; cross-compile the binary for amd64.
FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /backuparr .

# ---- Runtime stage ----
# Runtime must be amd64 for proxmox-backup-client (no arm64 packages).
# Trixie ships postgresql-client-17 which handles PG16+ servers natively.
FROM --platform=linux/amd64 debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates wget gnupg \
        postgresql-client && \
    # Add bookworm repos for PBS client and its libfuse3-3 dependency
    echo "deb http://deb.debian.org/debian bookworm main" \
        > /etc/apt/sources.list.d/bookworm.list && \
    echo "deb http://download.proxmox.com/debian/pbs-client bookworm main" \
        > /etc/apt/sources.list.d/pbs-client.list && \
    wget -qO /etc/apt/trusted.gpg.d/proxmox-release-bookworm.gpg \
        "http://download.proxmox.com/debian/proxmox-release-bookworm.gpg" && \
    apt-get update && \
    apt-get install -y --no-install-recommends proxmox-backup-client && \
    # Cleanup build-only deps and bookworm repo
    rm -f /etc/apt/sources.list.d/bookworm.list && \
    apt-get purge -y wget gnupg && \
    apt-get autoremove -y && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /backuparr /usr/local/bin/backuparr

RUN mkdir -p /config
ENTRYPOINT ["backuparr"]
