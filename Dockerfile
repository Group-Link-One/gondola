# Build stage
FROM golang:1.26-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git openssh-client

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application — VERSION is injected via ldflags so the running
# binary reports the same string as the image tag (e.g. "0.1.4"). Set by
# the GitHub Actions workflow; defaults to "dev" for local builds.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo \
    -ldflags="-X main.version=${VERSION}" \
    -o server cmd/server/main.go

# Final stage
FROM alpine:3.24

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    git \
    openssh-client \
    kustomize

WORKDIR /app

# Create necessary directories
RUN mkdir -p /app/data /app/repos /etc/deploy-api

# Copy the binary from builder
COPY --from=builder /app/server /app/server

# Copy migrations
COPY --from=builder /app/internal/store/migrations /app/internal/store/migrations

# Copy web UI (served by the Gin router when STATIC_PATH resolves to /app/web/static)
COPY --from=builder /app/web /app/web

# Create non-root user
RUN adduser -D -g '' deploy-api && \
    chown -R deploy-api:deploy-api /app/data /app/repos /etc/deploy-api

USER deploy-api

# Expose port
EXPOSE 8080

# Volume mounts for data persistence
VOLUME ["/app/data", "/app/repos", "/etc/deploy-api"]

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Run the server
ENTRYPOINT ["/app/server"]
