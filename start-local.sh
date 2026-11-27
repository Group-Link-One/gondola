#!/bin/bash

# Start deploy-api locally with test configuration
# Usage: ./start-local.sh
# Or with env vars: CF_API_KEY=xxx ./start-local.sh

set -e

# Change to script directory
cd "$(dirname "$0")"

# Export config path for local testing
export CONFIG_PATH="${CONFIG_PATH:-./config.local.yaml}"
# Legacy key — only valid on /external/* (CI ingress)
export DEPLOY_API_KEY="${DEPLOY_API_KEY:-test-key-local}"
# Break-glass root key — full /api/v1/* admin access. Required for test-local.sh
# unless you obtain a dp_* token via /api/v1/auth/login.
export DEPLOY_ROOT_KEY="${DEPLOY_ROOT_KEY:-test-root-key-local}"

# Load .env file if it exists
if [ -f .env ]; then
    echo "Loading .env file..."
    export $(grep -v '^#' .env | xargs)
fi

# Show what we're using
echo "Using config: $CONFIG_PATH"

# Ensure data and repos directories exist
mkdir -p data repos

echo "Starting deploy-api..."
echo "  Config: $CONFIG_PATH"
echo "  API Key (legacy, /external only): ${DEPLOY_API_KEY:0:10}..."
echo "  Root Key (break-glass, /api/v1/*): ${DEPLOY_ROOT_KEY:0:12}..."
echo "  CF_API_KEY: ${CF_API_KEY:+set (hidden)}"
echo "  CF_ACCOUNT_ID: ${CF_ACCOUNT_ID:-(not set)}"
echo "  LLM_TYPE: ${LLM_TYPE:-(not set)}"
echo ""

# Build if needed
if [ ! -f /tmp/deploy-api ] || [ cmd/server/main.go -nt /tmp/deploy-api ]; then
    echo "Building..."
    go build -o /tmp/deploy-api ./cmd/server/main.go
fi

# Run
echo "Server starting on http://localhost:8080"
echo "Press Ctrl+C to stop"
echo ""

/tmp/deploy-api
