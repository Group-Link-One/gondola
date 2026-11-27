> **Mirror notice.** This repository is published from an internal repository.
> Each release lands here as a single squashed commit, so upstream history is not
> preserved. Pull requests are welcome: accepted changes are applied upstream and
> come back in the next sync. See [CONTRIBUTING.md](CONTRIBUTING.md).

# Gondola

A centralized deployment orchestration service that manages Git credentials and ArgoCD repository access outside of CI pipelines.

## Overview

Gondola provides a clean separation between CI/CD pipelines and deployment operations. CI pipelines only need to call HTTP endpoints to:

1. Register new builds
2. Promote builds to environments (staging, prod, newprod)
3. Query deployment history and changelogs

The service handles all Git operations, kustomize edits, and maintains a complete audit trail.

Gondola implements a deploy-side workflow we call **Leased-Trunk Flow** — trunk-based development where any branch can temporarily lease a shared environment. See [LEASED-TRUNK.md](./LEASED-TRUNK.md) for the motivation and rules.

## Features

- **Build Registry**: Records all Docker image builds with metadata
- **Promotion API**: Handles kustomize image edits, git commits, and pushes
- **Changelog Extraction**: Walks git history between commits for risk assessment
- **Monorepo Support**: Path-prefix filtering (`source_paths`) and reference files (`context_files`) to scope changelogs and enrich LLM analysis for one app inside a shared repo
- **Experiments**: Builds on non-main branches are auto-flagged as experiments, tracked in a dedicated UI tab, skip LLM analysis, and emit dedicated webhooks when deployed or replaced
- **LLM Integration**: Swappable providers (Cloudflare, Gemini, Ollama, OpenRouter) for automated risk analysis
- **Full Audit Trail**: SQLite database tracks every deployment with before/after states
- **Multi-repo Support**: Manages both ArgoCD manifest repos and application source repos
- **Per-Environment Deployment Config**: Different kustomize overlay strategies per environment from a single repo entry
- **RBAC**: Role-based access control (admin, deployer, viewer) with per-environment permissions
- **Environment Reset Gate**: A deploy that displaces an experiment is parked until your reset runner reports the environment clean, then runs on its own
- **Webhooks**: Outbound notifications for deployment, experiment, and replacement events
- **Orphaned Resource Cleanup**: Soft-deletes apps and repos removed from config (auto-restores when re-added)
- **Deployment Config in API**: Repository API includes per-environment deployment strategies

## Quick Start

### Local Development

```bash
# 1. Start the server with local config
./start-local.sh

# 2. In another terminal, run the integration tests
./test-local.sh
```

This uses:
- Local test repositories (no SSH keys required)
- SQLite database in `./data/`

### Using Docker Compose

```bash
# Copy and edit environment variables
cp .env.example .env

# Start the service
docker-compose up -d

# Check health
curl http://localhost:8080/health
```

## Configuration

### Repository Setup

Gondola manages two types of repos:

- **ArgoCD repos** (`type: argocd`): Manifest repos that receive kustomize edits during deployments
- **Application repos** (`type: application`): Source repos for changelog extraction and deployment config

#### Per-Environment Deployment Config (Required)

**Deployment configuration is required** for application repos. A single repo entry can have different deployment strategies per environment using the `deployments` map:

```yaml
repos:
  app-core-api:
    url: "git@github.com:org/core-api.git"
    type: application
    branch: main
    environments: [staging, prod]
    deployments:
      staging:
        strategy: kustomize
        base_path: custom/core-api
        overlay_pattern: "*"  # Match all directories with kustomization.yaml
      prod:
        strategy: kustomize
        base_path: custom/core-api
        overlays:  # Explicit overlays
          - name: api
            path: overlay-api
          - name: api-pvt
            path: overlay-api-pvt
          - name: crontab
            path: overlay-crontab
```

**Deployment fields:**

| Field | Description | Example |
|-------|-------------|---------|
| `strategy` | Deployment strategy (currently only `kustomize`) | `kustomize` |
| `base_path` | Base directory in ArgoCD repo | `custom/core-api` |
| `overlay_pattern` | Pattern for overlay discovery (`*` = all) | `*`, `overlay-*` |
| `overlays` | Explicit list of overlays (name + path) | `[{name: api, path: overlay-api}]` |

Use either `overlay_pattern` (flexible) or `overlays` (explicit), not both.

### Monorepo Support (multiple apps in one repo)

When several applications share a single git repository, you register **one application repo entry per app** — all pointing at the same URL — and scope each one to its subtree with `source_paths` and (optionally) `context_files`.

```yaml
repos:
  # Same git URL, different apps, different subtrees
  app-api:
    url: "git@github.com:org/monorepo.git"
    type: application
    branch: main
    environments: [staging, prod]
    source_paths:              # Commits/diffs touching only these prefixes are included
      - services/api/
      - libs/shared/           # Shared code counts too
    context_files:             # Always injected into the LLM prompt (capped at 10KB each)
      - services/api/Dockerfile
      - services/api/README.md
    deployments:
      staging:
        strategy: kustomize
        base_path: argocd/api
        overlay_pattern: "*"

  app-worker:
    url: "git@github.com:org/monorepo.git"   # Same repo
    type: application
    branch: main
    environments: [staging, prod]
    source_paths:
      - services/worker/
      - libs/shared/
    deployments:
      staging:
        strategy: kustomize
        base_path: argocd/worker
        overlay_pattern: "*"
```

**How the fields behave:**

| Field | Purpose | Notes |
|---|---|---|
| `source_paths` | Changelog filter — only commits that touch files under these prefixes are included, and only matching files contribute to the diff | Overlapping prefixes between apps are fine (e.g., a shared `libs/` folder appears in both apps' changelogs) |
| `context_files` | Additional files read from repo HEAD and appended to the LLM prompt as reference material | Missing files are silently skipped; each file is truncated at 10KB |

**Per-build override:** a CI pipeline can pass `source_path` in the build request body to override the configured list for a single build (useful when the CI system knows exactly which subtree changed). The override takes precedence over `source_paths` in config.

```bash
curl -X POST https://gondola.internal/external/builds \
  -H "Authorization: Bearer $DEPLOY_API_KEY" \
  -d '{
    "app": "api",
    "source_repo": "app-api",
    "source_path": "services/api/",
    "commit_sha": "abc123",
    "image_tag": "abc123",
    "image_uri": "registry.example.com/api:abc123",
    "branch": "main"
  }'
```

The `GET /api/v1/repos` response exposes `source_paths` and `context_files` for every app repo so the UI and downstream tooling can display them.

### Experiments (non-main branch builds)

Any build whose `branch` differs from the repo's configured `branch` is automatically treated as an **experiment**. Experiments let you deploy a feature branch or a spike to a real environment without polluting the main deployment history.

**Setup:** just set the repo's `branch` to the trunk name. There is no extra flag to enable experiments — it is the default behavior.

```yaml
repos:
  app-api:
    url: "git@github.com:org/api.git"
    type: application
    branch: main              # Anything not on 'main' becomes an experiment build
    environments: [staging, prod]
    deployments:
      staging:
        strategy: kustomize
        base_path: argocd/api
        overlay_pattern: "*"
```

**What changes for experiment builds:**

| Area | Behavior |
|---|---|
| LLM analysis | Skipped. `analysis_status` is set to `completed` immediately — no risk score, no changelog generation |
| Build list | `/api/v1/builds?branch=main` shows main-only; `/api/v1/builds?branch_exclude=main` shows experiments |
| UI | Dedicated **Experiments** tab lists them per-app, separately from main builds |
| Deploy pre-flight | `POST /api/v1/deployments/check` returns `current_is_experiment` and `target_is_experiment` so callers can warn users |
| UI deploy flow | Deploying a main build *on top of* an experiment requires a double-confirmation in the web UI |
| Webhooks | Fires `experiment_deployed` when an experiment is deployed, and `experiment_replaced` when a main-branch build replaces one |

**Registering an experiment build** is the same call as a regular build — just pass the feature branch:

```bash
curl -X POST https://gondola.internal/external/builds \
  -H "Authorization: Bearer $DEPLOY_API_KEY" \
  -d '{
    "app": "api",
    "source_repo": "app-api",
    "branch": "feature/new-billing",
    "commit_sha": "abc123",
    "image_tag": "abc123",
    "image_uri": "registry.example.com/api:abc123"
  }'
```

**Webhook events** (subscribe via `webhook.urls` in config):

| Event | Fires when |
|---|---|
| `deployment_succeeded` | Any deployment completes |
| `experiment_deployed` | A deployment's target build is an experiment |
| `experiment_replaced` | A deployment replaces a currently-deployed experiment with a main-branch build |
| `env_reset_requested` | A deploy was parked by the reset gate (see below) |

### Environment Reset Gate

An experiment leaves state behind: rows in the database, jobs in queues, files in
buckets. Whatever runs there next inherits it. The reset gate turns that cleanup
into a step the system waits for instead of a convention people remember.

With the gate on, deploying a *different* branch over a live experiment does not
touch git. Gondola parks the deploy, records it, and asks your reset runner to
clean the environment. The deploy happens when the runner says it is done.

```yaml
environments:
  staging:
    baseline: prod
    reset:
      enabled: true
      webhook_url: "https://runner.internal/reset-staging"  # falls back to webhook.urls
      timeout: 1800      # seconds before an unanswered reset is failed
      auto_deploy: true  # run the parked deploy when the reset completes
```

**1. The deploy comes back `202` instead of `201`:**

```json
{
  "data": {
    "status": "awaiting_env_reset",
    "reset": { "id": 42, "status": "pending", "previous_branch": "feature/new-billing", "build_id": 87 },
    "auto_deploy": true,
    "reset_status_path": "/api/v1/env-resets/42"
  }
}
```

**2. Your runner receives `env_reset_requested`:**

```json
{
  "reset_id": 42,
  "app_name": "api",
  "environment": "staging",
  "experiment_branch": "feature/new-billing",
  "pending_build_id": 87,
  "pending_image_tag": "main-9f3c1a",
  "callback_url": "https://gondola.internal/external/env-resets/42/status",
  "callback_token": "1f9a...",
  "status_url": "https://gondola.internal/external/env-resets/42",
  "auto_deploy_on_completion": true
}
```

**3. It reports back when the environment is clean:**

```bash
curl -X POST "$CALLBACK_URL" \
  -H "Authorization: Bearer $DEPLOY_API_KEY" \
  -H "X-Reset-Token: $CALLBACK_TOKEN" \
  -d '{"status": "completed", "note": "db restored from baseline snapshot"}'
```

Long resets can post `{"status": "running"}` first; a failure posts
`{"status": "failed", "error": "..."}` and the parked deploy does not run.

**4. Anyone can poll while it works:**

```bash
GET /api/v1/env-resets/42          # or /external/env-resets/42 with the reset token
GET /api/v1/env-resets?active=true # everything currently gated
```

The web UI does exactly this: the deploy button stays parked with the reset's
status and flips to deployed on its own, across page reloads.

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/env-resets` | List resets (`?app=`, `?environment=`, `?status=`, `?active=true`) |
| `GET /api/v1/env-resets/:id` | Poll one reset |
| `POST /api/v1/env-resets/:id/status` | Report `running` / `completed` / `failed` / `canceled` |
| `POST /api/v1/env-resets/:id/cancel` | Abandon the parked deploy |
| `GET`/`POST` `/external/env-resets/:id[/status]` | Same two, for a runner holding the CI key + reset token |

Details worth knowing:

- **Same-branch redeploys are never gated.** The lease holder shipping another
  commit of their own experiment has nothing to tear down.
- **A second deploy ask retargets the open reset** instead of queueing another
  teardown. The newest ask wins the parking slot.
- **An unanswered reset fails after `timeout`**, so a dead runner cannot hold the
  environment hostage.
- **Break-glass:** an admin (or anyone allowed to deploy to that environment) can
  mark a reset completed through `/api/v1/env-resets/:id/status` without the
  token, e.g. after resetting by hand.
- **Without `auto_deploy`**, completing the reset just reopens the gate; deploy
  again when you are ready.

### Environment Baselines

Configure which environment to compare against for changelog extraction:

```yaml
environments:
  staging:
    baseline: prod
  prod:
    baseline: prod
```

### Argo CD Refresh After Push

Optional. When set, Gondola calls the Argo CD API right after it pushes a manifest
change, so Argo picks it up without waiting for its poll interval. It refreshes the
discovered Application and, when the Application has no `syncPolicy.automated`,
also triggers a sync. Apps that are only in `config.yaml` (not discovered) are skipped.

```yaml
argocd_api:
  url: "${ARGOCD_API_URL}"     # e.g. http://argocd-server.argocd.svc
  token: "${ARGOCD_API_TOKEN}" # Argo CD account token with get/sync on applications
```

Both values fall back to the `ARGOCD_API_URL` and `ARGOCD_API_TOKEN` env vars. If either
is empty the feature is off. Failures are logged and never fail the deploy.

### LLM Providers

Gondola supports multiple LLM providers for risk analysis and changelog generation:

```yaml
# Cloudflare AI
llm:
  type: "cloudflare"
  api_key: "${CF_API_KEY}"
  base_url: "${CF_ACCOUNT_ID}"
  model: "@cf/meta/llama-3.1-8b-instruct-fast"

# Google Gemini
llm:
  type: "gemini"
  api_key: "${GEMINI_API_KEY}"
  model: "gemini-3-flash-preview"

# Ollama (local/remote)
llm:
  type: "ollama"
  base_url: "http://localhost:11434"
  model: "llama3.2:1b"

# OpenRouter (multi-model, pay-per-use)
llm:
  type: "openrouter"
  api_key: "${OPENROUTER_API_KEY}"
  model: "meta-llama/llama-3.2-1b-instruct"
```

## API Reference

### Register a Build

```bash
POST /api/v1/builds
Authorization: Bearer $TOKEN

{
  "app": "core-api",
  "source_repo": "app-core-api",
  "image_tag": "abc123def",
  "image_uri": "registry.example.com/core-api:abc123def",
  "commit_sha": "abc123def456...",
  "branch": "main",
  "built_by": "pipeline-1234",
  "source_path": "services/api/"   // optional, monorepo override — takes precedence over config.source_paths
}
```

If `branch` does not match the repo's configured main branch, the build is flagged as an experiment (LLM analysis skipped).

### Deploy a Build

```bash
POST /api/v1/deployments
Authorization: Bearer $TOKEN

{
  "build_id": 42,
  "environment": "staging"
}
```

Returns `201` with the deployment, or `202` with an environment reset to wait on
(see [Environment Reset Gate](#environment-reset-gate)).

### Pre-flight Check

```bash
POST /api/v1/deployments/check
Authorization: Bearer $TOKEN

{
  "build_id": 42,
  "environment": "staging"
}
```

Returns rollback detection and experiment status, plus `reset_required` /
`active_reset_id` when the [reset gate](#environment-reset-gate) would park the
deploy.

### List Deployments

```bash
GET /api/v1/deployments?app=core-api&environment=staging&limit=20
Authorization: Bearer $TOKEN
```

### Current Deployments

```bash
GET /api/v1/deployments/current
Authorization: Bearer $TOKEN
```

### List Builds

```bash
GET /api/v1/builds?app=core-api&branch=main&limit=20
Authorization: Bearer $TOKEN
```

Use `branch_exclude=main` to list only experiment builds (everything *not* on main):

```bash
GET /api/v1/builds?app=core-api&branch_exclude=main&limit=20
```

### Trigger Analysis

```bash
POST /api/v1/builds/:id/analyze
Authorization: Bearer $TOKEN
```

### External Build Endpoint (CI)

Separate endpoint for CI systems, authenticated by API key only:

```bash
POST /external/builds
Authorization: Bearer $DEPLOY_API_KEY

{
  "app": "my-app",
  "source_repo": "app-my-app",
  "branch": "main",
  "commit_sha": "abc123",
  "image_tag": "v1.0.0",
  "image_uri": "registry.com/my-app:v1.0.0",
  "built_by": "ci-pipeline"
}
```

### List Repositories

```bash
GET /api/v1/repos
Authorization: Bearer $TOKEN
```

Response includes `deployments` (per-environment config), plus `source_paths` and `context_files` for monorepo apps:
```json
{
  "data": [{
    "id": "app-core-api",
    "repo_type": "application",
    "source_paths": ["services/api/", "libs/shared/"],
    "context_files": ["services/api/Dockerfile"],
    "deployments": {
      "staging": {
        "strategy": "kustomize",
        "base_path": "custom/core-api",
        "overlay_pattern": "*"
      },
      "prod": {
        "strategy": "kustomize",
        "base_path": "custom/core-api",
        "overlays": [
          {"name": "api", "path": "overlay-api"},
          {"name": "api-pvt", "path": "overlay-api-pvt"}
        ]
      }
    }
  }]
}
```

**Include deleted repos:**
```bash
GET /api/v1/repos?include_deleted=true
```

### Sync Repositories

```bash
POST /api/v1/repos/:id/sync
Authorization: Bearer $TOKEN
```

Trigger git pull for a specific repository.

## CI Integration Example

```bash
# After building and pushing image:
curl -X POST https://gondola.internal/external/builds \
  -H "Authorization: Bearer $DEPLOY_API_KEY" \
  -d "{
    \"app\": \"$CI_PROJECT_NAME\",
    \"source_repo\": \"app-$CI_PROJECT_NAME\",
    \"image_tag\": \"$CI_COMMIT_SHA\",
    \"image_uri\": \"$CI_REGISTRY_IMAGE:$CI_COMMIT_SHA\",
    \"commit_sha\": \"$CI_COMMIT_SHA\",
    \"branch\": \"$CI_COMMIT_BRANCH\",
    \"built_by\": \"$CI_PIPELINE_ID\"
  }"
```

## Web UI

Gondola includes a built-in web interface at `http://localhost:8080/`:

### Features

- **Dashboard**: View all apps with current deployments and recent builds
- **Builds**: Browse build history with LLM risk analysis
- **Deployments**: Track deployment history and changelogs
- **Repositories**: Manage repos with deployment configuration viewer
  - **Show Deleted**: Checkbox to view soft-deleted repos
  - **Deployment Config**: Expandable panel showing strategy, base_path, overlays per environment
- **Experiments**: Track non-main branch builds
- **Admin Panel**: User management (admin only), webhook status, system config

### RBAC in UI

| Role | Capabilities |
|------|-------------|
| `admin` | Full access including user management |
| `deployer` | Can deploy to allowed environments, sync repos |
| `viewer` | Read-only access, no deployment buttons |

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────────┐
│   CI/CD     │────>│   Gondola   │────>│  ArgoCD Repos   │
│  Pipeline   │     │             │     │  (kustomize)    │
└─────────────┘     └──────┬──────┘     └─────────────────┘
                           │
                    ┌──────┴──────┐
                    │   SQLite    │
                    │  (audit)    │
                    └──────┬──────┘
                           │
                    ┌──────┴──────┐
                    │  App Repos  │
                    │ (changelog) │
                    └─────────────┘
```

## Development

```bash
# Run all tests
go test ./...

# Run with local config
CONFIG_PATH=./config.local.yaml go run cmd/server/main.go

# Integration tests
./test-local.sh

# Build binary
go build -o gondola cmd/server/main.go
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `DEPLOY_API_KEY` | API key for authentication | Yes |
| `CONFIG_PATH` | Path to config file | No (default: `./config.yaml`) |
| `CF_API_KEY` | Cloudflare API token | For Cloudflare LLM |
| `CF_ACCOUNT_ID` | Cloudflare account ID | For Cloudflare LLM |
| `GEMINI_API_KEY` | Google Gemini API key | For Gemini LLM |
| `OLLAMA_HOST` | Ollama server URL | For Ollama LLM |
| `OPENROUTER_API_KEY` | OpenRouter API key | For OpenRouter LLM |
| `SSH_KEY_PATH` | Path to SSH key for git | For remote repos |
| `ARGOCD_API_URL` | Argo CD API base URL for refresh after push | No (feature off when empty) |
| `ARGOCD_API_TOKEN` | Argo CD API bearer token | No (feature off when empty) |

## License

MIT
