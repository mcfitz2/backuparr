# ---- Build stage ----
FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /backuparr ./cmd/backuparr

# ---- Runtime stage ----
# Trixie ships postgresql-client-17 which handles PG16+ servers natively.
FROM debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        postgresql-client && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /backuparr /usr/local/bin/backuparr

RUN mkdir -p /config
ENTRYPOINT ["backuparr"]
