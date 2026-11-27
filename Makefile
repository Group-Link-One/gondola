.PHONY: all build run run-local test test-local test-api clean setup setup-kustomize stop logs version release release-patch release-minor release-major

# Default configuration
CONFIG_PATH ?= ./config.local.yaml
SERVER_PORT ?= 8080
API_KEY ?= test-key-local
API_URL = http://localhost:$(SERVER_PORT)

# =============================================================================
# Build
# =============================================================================

all: build

build:
	go build -o bin/server ./cmd/server/main.go
	@echo "Built: bin/server"

clean:
	rm -rf bin/
	rm -rf repos/
	@echo "Cleaned build artifacts"

# =============================================================================
# Setup
# =============================================================================

setup: setup-kustomize
	@echo "Setup complete!"

setup-kustomize:
	@if [ ! -f data/kustomize ]; then \
		echo "Downloading kustomize..."; \
		mkdir -p data; \
		curl -s "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash -s -- 5.4.0 /tmp/; \
		mv /tmp/kustomize data/kustomize; \
		echo "Kustomize installed to data/kustomize"; \
	else \
		echo "Kustomize already installed at data/kustomize"; \
	fi

# =============================================================================
# Run
# =============================================================================

run: build setup-kustomize
	CONFIG_PATH=$(CONFIG_PATH) ./bin/server

run-local: build setup-kustomize
	@echo "Starting server with local config..."
	@mkdir -p logs
	@CONFIG_PATH=./config.local.yaml ./bin/server > logs/server.log 2>&1 &
	@sleep 2
	@echo "Server started at $(API_URL)"
	@echo "Logs: tail -f logs/server.log"

run-dev:
	@echo "Starting server in dev mode (auto-restart)..."
	@which air > /dev/null 2>&1 || go install github.com/air-verse/air@latest
	CONFIG_PATH=$(CONFIG_PATH) air

stop:
	@pkill -f "bin/server" 2>/dev/null || true
	@echo "Server stopped"
	@echo "Logs were saved to: logs/server.log"

logs:
	@tail -f logs/server.log 2>/dev/null || echo "No log file found. Start server with: make run-local"

# =============================================================================
# Health & Status
# =============================================================================

.check-server:
	@curl -s $(API_URL)/health > /dev/null 2>&1 || { \
		echo "Server not running. Start it with: make run-local"; \
		exit 1; \
	}

status: .check-server
	@echo "Server status:"
	@curl -s $(API_URL)/health | jq .

# =============================================================================
# Testing
# =============================================================================

test: test-unit

test-unit:
	go test -v -race -short ./...

test-integration:
	go test -v -race -count=1 -tags=integration ./...

test-local: .check-server
	@echo "Running local integration tests..."
	./test-local.sh

test-api: .check-server
	@echo "=== Testing API Endpoints ==="
	@echo ""
	@echo "1. Health check:"
	@curl -s $(API_URL)/health | jq .
	@echo ""
	@echo "2. List builds:"
	@curl -s $(API_URL)/api/v1/builds -H "Authorization: Bearer $(API_KEY)" | jq .
	@echo ""
	@echo "3. List apps:"
	@curl -s $(API_URL)/api/v1/apps -H "Authorization: Bearer $(API_KEY)" | jq .
	@echo ""
	@echo "4. List repos:"
	@curl -s $(API_URL)/api/v1/repos -H "Authorization: Bearer $(API_KEY)" | jq .

# Quick smoke test
test-smoke: .check-server
	@echo "Running smoke tests..."
	@curl -sf $(API_URL)/health > /dev/null && echo "✅ Health check passed" || echo "❌ Health check failed"
	@curl -sf $(API_URL)/api/v1/builds -H "Authorization: Bearer $(API_KEY)" > /dev/null && echo "✅ Builds endpoint passed" || echo "❌ Builds endpoint failed"
	@curl -sf $(API_URL)/api/v1/apps -H "Authorization: Bearer $(API_KEY)" > /dev/null && echo "✅ Apps endpoint passed" || echo "❌ Apps endpoint failed"
	@echo "Smoke tests complete!"

# =============================================================================
# Development Helpers
# =============================================================================

fmt:
	go fmt ./...

lint:
	@which golangci-lint > /dev/null 2>&1 || go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	golangci-lint run

deps:
	go mod download
	go mod tidy

# Database operations
db-reset:
	@rm -f data/deploy.db
	@echo "Database reset (will be recreated on next server start)"

db-shell:
	@sqlite3 data/deploy.db

# Check what's in the cloned repos
show-repo:
	@echo "Contents of repos/argocd-staging/apps:"
	@ls -la repos/argocd-staging/apps/ 2>/dev/null || echo "No repos cloned yet"

# =============================================================================
# Docker
# =============================================================================

docker-build:
	docker build -t deploy-api:latest .

docker-run:
	docker run -p $(SERVER_PORT):8080 \
		-v $(PWD)/config.local.yaml:/app/config.yaml \
		-v $(PWD)/data:/app/data \
		-e CONFIG_PATH=/app/config.yaml \
		deploy-api:latest

# =============================================================================
# Release (git tag driven — pushing a v* tag triggers the build workflow)
# =============================================================================

# Latest v* tag, or v0.0.0 if none
CURRENT_VERSION := $(shell git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || echo v0.0.0)

version:
	@echo "Current version: $(CURRENT_VERSION)"

# Create and push an annotated tag. Usage: make release VERSION=v1.2.3
release:
	@if [ -z "$(VERSION)" ]; then \
		echo "Usage: make release VERSION=v1.2.3"; \
		exit 1; \
	fi
	@echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { \
		echo "Error: VERSION must match vX.Y.Z (got: $(VERSION))"; exit 1; \
	}
	@if ! git diff-index --quiet HEAD --; then \
		echo "Error: working tree is dirty. Commit or stash changes first."; exit 1; \
	fi
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then \
		echo "Error: tag $(VERSION) already exists."; exit 1; \
	fi
	@BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "main" ]; then \
		echo "Warning: releasing from branch '$$BRANCH' (not main)."; \
		read -p "Continue? [y/N] " ans; [ "$$ans" = "y" ] || exit 1; \
	fi
	@echo "Tagging $(VERSION) at $$(git rev-parse --short HEAD)"
	git tag -a "$(VERSION)" -m "Release $(VERSION)"
	git push origin "$(VERSION)"
	@echo ""
	@echo "Pushed $(VERSION). The tag triggers the build-and-push workflow."

# Auto-bump helpers (based on latest v* tag)
release-patch:
	@V=$$(echo $(CURRENT_VERSION) | sed 's/^v//' | awk -F. '{printf "v%d.%d.%d", $$1, $$2, $$3+1}'); \
	$(MAKE) release VERSION=$$V

release-minor:
	@V=$$(echo $(CURRENT_VERSION) | sed 's/^v//' | awk -F. '{printf "v%d.%d.0", $$1, $$2+1}'); \
	$(MAKE) release VERSION=$$V

release-major:
	@V=$$(echo $(CURRENT_VERSION) | sed 's/^v//' | awk -F. '{printf "v%d.0.0", $$1+1}'); \
	$(MAKE) release VERSION=$$V

# =============================================================================
# Default target
# =============================================================================

.DEFAULT_GOAL := help

help:
	@echo "Deploy API - Available Commands:"
	@echo ""
	@echo "Setup:"
	@echo "  make setup           - Download kustomize and setup environment"
	@echo "  make setup-kustomize - Download kustomize binary to ./data/"
	@echo "  make deps            - Download Go dependencies"
	@echo ""
	@echo "Build & Run:"
	@echo "  make build           - Build the server binary"
	@echo "  make run             - Build and run server with default config"
	@echo "  make run-local       - Build and run with local config (background)"
	@echo "  make run-dev         - Run with air (auto-restart on changes)"
	@echo "  make stop            - Stop the running server"
	@echo "  make logs            - Tail server logs"
	@echo ""
	@echo "Testing:"
	@echo "  make test            - Run unit tests"
	@echo "  make test-local      - Run local integration tests (requires server)"
	@echo "  make test-api        - Quick API endpoint tests"
	@echo "  make test-smoke      - Quick smoke tests"
	@echo ""
	@echo "Development:"
	@echo "  make fmt             - Format Go code"
	@echo "  make lint            - Run linter"
	@echo "  make clean           - Clean build artifacts"
	@echo "  make db-reset        - Reset the database"
	@echo "  make show-repo       - Show cloned repo contents"
	@echo ""
	@echo "Docker:"
	@echo "  make docker-build    - Build Docker image"
	@echo "  make docker-run      - Run with Docker"
	@echo ""
	@echo "Release (pushes a v* tag, which triggers the GitHub Actions build):"
	@echo "  make version         - Show latest v* tag"
	@echo "  make release VERSION=v1.2.3 - Tag and push a specific version"
	@echo "  make release-patch   - Bump patch (v1.2.3 -> v1.2.4)"
	@echo "  make release-minor   - Bump minor (v1.2.3 -> v1.3.0)"
	@echo "  make release-major   - Bump major (v1.2.3 -> v2.0.0)"
