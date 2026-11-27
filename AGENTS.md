# Deploy API - Agent Guide

This document provides essential information for AI coding agents working on the Deploy API project.

## Project Overview

Deploy API is a centralized deployment API service written in Go that manages Git credentials and ArgoCD repository access outside of CI pipelines. It provides a clean separation between CI/CD pipelines and deployment operations by exposing HTTP endpoints for:

- Registering new Docker image builds
- Promoting builds to environments (staging, prod, newprod)
- Querying deployment history and changelogs
- Automated risk analysis via LLM integration

The service handles all Git operations, Kustomize edits, and maintains a complete audit trail in SQLite.

## Technology Stack

- **Language**: Go 1.24
- **Web Framework**: [Gin](https://github.com/gin-gonic/gin) (HTTP router)
- **Database**: SQLite with [modernc.org/sqlite](https://modernc.org/sqlite) (pure Go driver)
- **Git Operations**: [go-git](https://github.com/go-git/go-git/v5) (pure Go Git library)
- **Configuration**: YAML with environment variable substitution
- **Containerization**: Docker with multi-stage builds

## Project Structure

```
.
├── cmd/server/main.go          # Application entry point
├── internal/
│   ├── api/                    # HTTP handlers and routing
│   │   ├── router_gin.go       # Route configuration
│   │   ├── middleware_gin.go   # Authentication, RBAC, request ID
│   │   ├── *_handler_gin.go    # Endpoint handlers (builds, deployments, auth, memory, analysis, etc.)
│   │   └── handlers_test.go    # Handler tests
│   ├── config/                 # Configuration management
│   │   └── config.go           # YAML parsing with env substitution
│   ├── domain/                 # Domain models and interfaces
│   │   ├── build.go            # Build entity and repository interface
│   │   ├── deployment.go       # Deployment entity and repository interface
│   │   ├── repo.go             # Repository configuration entity
│   │   ├── app.go              # Application registry entity
│   │   ├── changelog.go        # Changelog models
│   │   ├── memory.go           # Repository memory storage
│   │   └── user.go             # User, Token, RBAC models and helpers
│   ├── git/                    # Git operations
│   │   ├── manager.go          # Repository cloning, pulling, committing
│   │   ├── changelog.go        # Changelog extraction between commits
│   │   └── kustomize.go        # Kustomize image editing
│   ├── llm/                    # LLM provider integrations
│   │   ├── provider.go         # Provider interface
│   │   ├── analyzer.go         # Changelog analysis orchestration
│   │   ├── cloudflare.go       # Cloudflare AI provider
│   │   ├── gemini.go           # Google Gemini provider
│   │   ├── ollama.go           # Ollama provider
│   │   └── openrouter.go       # OpenRouter provider
│   └── store/                  # Data persistence
│       ├── sqlite.go           # Database connection and migrations
│       ├── *_store.go          # Store implementations
│       └── migrations/         # SQL migration files
├── migrations/                 # Root-level migrations (copied to image)
├── repos/                      # Cloned git repositories (gitignored)
├── data/                       # SQLite database files (gitignored)
├── config.yaml                 # Production configuration template
├── config.local.yaml           # Local development configuration
├── docker-compose.yml          # Docker Compose setup
├── Dockerfile                  # Multi-stage Docker build
├── start-local.sh              # Local development server script
├── test-local.sh               # Integration test script
└── README.md                   # User-facing documentation
```

## Build and Run Commands

### Local Development

```bash
# Start with local config (uses test repos, no SSH needed)
./start-local.sh

# Or manually:
CONFIG_PATH=./config.local.yaml DEPLOY_API_KEY=test-key-local go run cmd/server/main.go

# Run integration tests against local server
./test-local.sh

# Run unit tests
go test ./...

# Build binary
go build -o deploy-api cmd/server/main.go
```

### Docker

```bash
# Copy environment template and configure
cp .env.example .env
# Edit .env with your API keys

# Build and run with Docker Compose
docker-compose up --build

# Or build image directly
docker build -t deploy-api .
```

## Configuration

Configuration uses YAML with environment variable substitution via `${VAR}` syntax.

**`${VAR:-default}` is not supported.** `Load()` uses `os.ExpandEnv`, which reads
`VAR:-default` as the whole variable name, so such a line silently resolves to
the empty string *even when `VAR` is set*. An unset `${VAR}` already yields empty,
so write the plain form and handle the default in Go.

### Key Configuration Files

- `config.yaml` - Production template with environment variable placeholders
- `config.local.yaml` - Local development with file:// repos (no SSH required)
- `.env` - Environment variables loaded automatically (gitignored)

### Configuration Sections

| Section | Purpose |
|---------|---------|
| `server` | HTTP port and API key |
| `database` | SQLite database path |
| `git` | SSH key path, author name/email |
| `kustomize` | Path to kustomize binary |
| `llm` | LLM provider configuration (type, api_key, model, prompts) |
| `llm.prompts` | Customizable system prompts for risk analysis and changelog generation |
| `repos` | Repository definitions (ArgoCD and application repos) |
| `webhook` | Webhook notification URLs, headers, timeouts (see [Webhooks](#webhooks)) |
| `environments` | Per-environment config (baseline for comparison, reset gate) |
| `argocd_api` | Optional Argo CD API `url`/`token` (env `ARGOCD_API_URL`/`ARGOCD_API_TOKEN`) to refresh, and sync when not auto-sync, discovered Applications after a push |

### Repository Types

- **argocd**: Manifest repos that receive Kustomize edits (staging, prod, newprod)
- **application**: Source repos for changelog extraction and deployment targets

### Deployment Configuration

Each `application` repo should have a `deployment` (or `deployments`) section. When absent, the [ArgoCD Application auto-discovery](#argocd-application-auto-discovery) feature provides a fallback by parsing manifests in `argocd`-typed repos.

```yaml
repos:
  app-core-api-prod:
    url: "git@github.com:org/core-api.git"
    type: application
    branch: main
    environments: [prod]
    deployment:              # REQUIRED - no auto-discovery
      strategy: kustomize    # Currently only "kustomize" is supported
      base_path: custom/core-api
      # Option 1: Explicit overlays (recommended for prod)
      overlays:
        - name: api
          path: overlay-api
        - name: api-pvt
          path: overlay-api-pvt
        - name: crontab
          path: overlay-crontab
      # Option 2: Pattern-based discovery
      # overlay_pattern: "overlay-*"
      # Option 3: Pattern "*" includes base_path itself (for flat staging structure)
      # overlay_pattern: "*"
```

**Resolution order in `ResolveOverlayDirs()`:**
1. If `overlays` list is set → use explicit paths (validate all exist)
2. Else if `overlay_pattern` is set → glob match, filter to valid kustomize dirs
   - Special case: pattern `"*"` also includes `base_path` itself
3. Else → treat `base_path` as single overlay directory

**Pattern examples:**

| Pattern | Use Case | Example Structure |
|---------|----------|-------------------|
| Explicit `overlays` | Fixed multi-component apps | `overlay-api/`, `overlay-api-pvt/`, `overlay-crontab/` |
| `overlay_pattern: "*"` | Flat staging structure | `kustomization.yaml` directly in `base_path` |
| `overlay_pattern: "overlay-*"` | Dynamic discovery | Auto-finds all `overlay-*` directories |
| No pattern/overlays | Simple single-overlay | Just `base_path` with `kustomization.yaml` |

### ArgoCD Application Auto-Discovery

The discovery scanner (`internal/discovery/argocd.go`) walks every `argocd`-typed repo and parses `kind: Application` manifests to synthesize `DeploymentConfig` on demand. This removes the need to declare every app in `config.yaml`.

**Resolution priority in `PerformDeployment`:**
1. `repos[source_repo].deployments[env]` — per-env config in `config.yaml` (preferred)
2. `repos[source_repo].deployment` — legacy single config in `config.yaml`
3. Discovered config from ArgoCD Application manifest (fallback)
4. → 400 "deployment config is required"

**Scan timing:**
- Initial scan after the startup git sync completes
- Periodic refresh every `discovery.refresh_interval` seconds (default `3600`; set to `-1` to disable)
- Manual `POST /api/v1/admin/repos/discover` (admin-only, bounded by `discovery.request_timeout`, default `60s`)

**Recognized annotations** (must be exact strings — no case-folding):

| Annotation | Effect |
|---|---|
| `gondola.io/skip: "true"` | Skip this Application entirely |
| `gondola.io/strategy: "kustomize"` | Optional; only `kustomize` is supported (other values cause skip) |
| `gondola.io/app-name` | Override `metadata.name` for the registry key (lets Gondola's app name diverge from the ArgoCD name; trimmed of whitespace) |
| `gondola.io/base-path` | Override `spec.source.path` for Gondola's `BasePath` (decouples ArgoCD's leaf watch path from the parent dir Gondola uses to resolve overlays) |
| `gondola.io/overlay-pattern: "overlay-*"` | Glob for overlays (same semantics as `overlay_pattern` in YAML) |
| `gondola.io/overlays: \|` | YAML-encoded list of `{name, path}` overlays |

By default `base_path` is taken from `spec.source.path`. Set `gondola.io/base-path` when ArgoCD points at a leaf overlay (e.g. `custom/core-api/overlay-api/`) but Gondola needs the parent (`custom/core-api/`) to resolve sibling overlays.

**Cross-env consistency:** the registry key is `(app_name, environment)`. If the same logical app exists in both staging and prod, the **same `gondola.io/app-name` value must be set on both Applications** (or both must rely on `metadata.name`). Otherwise the discovery lookup will succeed in one env and fail with `no deployment config for app=X env=Y` in the other. Auto-deploy to staging in particular silently skips when this happens.

**Example — multi-overlay app where ArgoCD watches one overlay:**

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: core-api-prod                    # ArgoCD-side name
  annotations:
    gondola.io/app-name: "core-api"      # Gondola-side name (matches build/image)
    gondola.io/base-path: "custom/core-api/"   # Gondola joins overlays here
    gondola.io/overlays: |
      - { name: api, path: overlay-api }
      - { name: api-pvt, path: overlay-api-pvt }
      - { name: crontab, path: overlay-crontab }
spec:
  source:
    path: custom/core-api/overlay-api/   # ArgoCD watches the leaf
```

**What gets skipped (visible at `GET /api/v1/admin/repos/discovered` under `skipped`):**
- Multi-source apps (`spec.sources` plural — typical Helm + values overlay pattern)
- Helm apps (`spec.source.chart` set, or `spec.source.helm` block present)
- Missing or escaped `spec.source.path` (path-traversal attempts logged as `path-escapes-repo`)
- Missing `kustomization.yaml` in `base_path` when no overlay annotations are set
- Duplicate `(app_name, environment)` across argocd repos — first wins, second listed in `skipped`

**Two surfaces, two views:**
- `GET /api/v1/apps` — apps that have been *built* (rows in the `apps` SQLite table); add `?include_deleted=true` to also list soft-deleted ones. `DELETE /api/v1/apps/:name` and `POST /api/v1/apps/:name/restore` are admin-only and toggle `deleted_at`.
- `GET /api/v1/admin/repos/discovered` — apps that exist in ArgoCD manifests *right now*

Discovered apps are not pre-seeded into the `apps` table; they appear there only after the first build registration.

**Configurable in `config.yaml`:**
```yaml
git:
  repos_dir: "./repos"   # base directory where repos are cloned
discovery:
  refresh_interval: 3600 # seconds; 0 = use default, -1 = disable
  request_timeout: 60    # seconds; bounds POST /admin/repos/discover
```

## Code Style Guidelines

### Go Conventions

- Follow standard Go formatting (`go fmt`)
- Use `gofmt` and `goimports` for imports
- Package names are lowercase without underscores
- Interface names end in `-er` (e.g., `Provider`, `Repository`)
- Constructor functions are named `New<Type>`

### Project Conventions

- **Domain-driven structure**: Business logic in `internal/domain/`, infrastructure in `internal/store/`, `internal/git/`, etc.
- **Interface segregation**: Repository interfaces defined in domain, implemented in store
- **Dependency injection**: All dependencies passed via `RouterConfig` to handlers
- **Error handling**: Wrap errors with context using `fmt.Errorf("...: %w", err)`

### Naming

- Database fields use `snake_case` with struct tags
- JSON fields use `snake_case`
- Environment constants: `EnvStaging`, `EnvProd`, `EnvNewProd`
- Status constants: `StatusPending`, `StatusPushed`, `StatusFailed`
- Role constants: `RoleAdmin`, `RoleDeployer`, `RoleViewer`

## Testing Strategy

### Test Types

1. **Unit Tests** (`*_test.go` alongside source files)
   - Handler tests using `httptest`
   - Store tests using temporary databases
   - No external dependencies (mocked or using in-memory SQLite)

2. **Integration Tests** (`test-local.sh`)
   - End-to-end API testing against running server
   - Uses local git repositories (see [Local Development Setup](#local-development-setup))
   - Tests full flow: build registration → promotion → verification

### Test Setup Pattern

```go
func setupTestServer(t *testing.T) (*chi.Mux, *store.DB, func()) {
    tmpDir := t.TempDir()
    dbPath := filepath.Join(tmpDir, "test.db")
    
    db, err := store.NewDB(dbPath)
    // ... error handling ...
    
    db.Migrate()
    
    // Initialize stores...
    
    cleanup := func() {
        db.Close()
        os.RemoveAll(tmpDir)
    }
    
    return router, db, cleanup
}
```

### Running Tests

```bash
# All tests
go test ./...

# Verbose output
go test -v ./...

# Specific package
go test ./internal/api/...

# With coverage
go test -cover ./...
```

## Database and Migrations

### Schema Management

- Migrations are SQL files in `internal/store/migrations/`
- Embedded into binary using `//go:embed` directive
- Migration tracking in `schema_migrations` table
- Migrations run automatically on startup

### Adding Migrations

1. Create file `internal/store/migrations/NNN_feature.sql` (next sequential number, currently 003+)
2. Use `IF NOT EXISTS` for idempotent operations
3. Include indexes for query optimization
4. Test rollback scenarios manually (SQLite doesn't support transactional DDL well)

### Key Tables

- `repos` - Git repository configurations
- `builds` - Docker image build records
- `deployments` - Deployment history with changelog
- `apps` - Application registry (derived/cached view)
- `repo_memory` / `repo_memory_versions` - Repository-specific project knowledge
- `users` - API users with role (`admin`, `deployer`, `viewer`) and `allowed_environments` (JSON array)
- `tokens` - API access tokens (hashed, per-user)
- `webhooks` - Outbound webhook event queue
- `env_resets` - Environment reset gate: one row per parked deploy (see [Environment Reset Gate](#environment-reset-gate))
- `app_groups` / `user_app_groups` - Named app-name pattern sets and their membership (see [App Scope](#app-scope-per-app-deploy-permission))
- `memory_suggestions` - LLM-proposed memory edits awaiting human review (see [Repository Memory](#repository-memory))

### Migrations

- `001_init.sql` - Consolidated schema (all tables, indexes, triggers, views, default admin user)
- `002_user_roles.sql` - Adds `role` and `allowed_environments` columns to users table
- `008_env_resets.sql` - Adds the `env_resets` table and the `env_reset_requested` webhook event type
- `009_app_scopes.sql` - Adds `app_groups`, `user_app_groups`, and `users.allowed_apps` / `users.denied_apps`
- `010_memory_suggestions.sql` - Adds `memory_suggestions` plus the partial unique index that dedupes still-pending proposals

## Git Operations

### Repository Management

The `git.Manager` handles:
- Cloning/pulling repositories on startup
- Serialized access per repository (mutex per repoID)
- Automatic detection of local vs SSH repos
- Committing and pushing changes to ArgoCD repos

### Local vs Remote Repos

- **Local repos**: File paths like `/path/to/repo` or `file://` URLs
  - No authentication required
  - No push operations (silently skipped)
  - Used for testing
  
- **Remote repos**: SSH URLs like `git@github.com:org/repo.git`
  - Requires SSH key at `git.ssh_key_path`
  - Full push/pull operations

### Kustomize Integration

The `KustomizeIntegration` handles kustomize operations:
- `SetImage()` - Updates image tags in kustomization.yaml files
- `ResolveOverlayDirs()` - Resolves overlay directories from deployment config (**required**)
- `Build()` - Runs `kustomize build` for validation

**Key method - `ResolveOverlayDirs()`:**
```go
func (k *KustomizeIntegration) ResolveOverlayDirs(
    argoRepoPath string, 
    deployConfig *config.DeploymentConfig,
) ([]string, error)
```

This method is called during deployment and:
1. Validates `deployConfig` is not nil (returns error otherwise)
2. Checks `deployConfig.Strategy == "kustomize"`
3. Resolves overlays based on config (explicit list, pattern, or base_path)
4. Validates all directories contain valid `kustomization.yaml` files

**Legacy methods (deprecated, for reference only):**
- `FindKustomizeDir()` - Auto-discovery for single overlay
- `FindAllKustomizeDirs()` - Auto-discovery for multiple overlays

These are kept for backwards compatibility but **should not be used** - always configure explicit deployment config.

## LLM Integration

### Supported Providers

- **Cloudflare AI**: Default, uses Llama models, requires `CF_API_KEY` and `CF_ACCOUNT_ID`
- **Google Gemini**: Uses Gemini Flash models, requires `GEMINI_API_KEY`
- **Ollama**: Local/remote Ollama server, configurable via `OLLAMA_HOST`
- **OpenRouter**: Access to multiple models, pay-per-use

### Configuration

Set in `config.yaml` or environment:
```yaml
llm:
  type: "cloudflare"  # or "gemini", "ollama", "openrouter"
  api_key: "${CF_API_KEY}"
  base_url: "${CF_ACCOUNT_ID}"
  model: "@cf/meta/llama-3.1-8b-instruct-fast"
  prompts:
    analysis_system: |
      You are a deployment risk analyzer for our microservices platform.
      Focus especially on: database migrations, breaking API changes,
      and changes to shared libraries. Use Portuguese for the summary.
    changelog_system: |
      You are a changelog generator. Group changes by feature area.
      Always write in Portuguese.
```

### Configurable Prompts

LLM system prompts are split into two parts:
- **Configurable part** (`llm.prompts.analysis_system` / `llm.prompts.changelog_system`): The "what to analyze" instructions. If omitted, defaults are used (defined as `DefaultAnalysisSystemPrompt` and `DefaultChangelogSystemPrompt` in `internal/llm/analyzer.go`).
- **Hardcoded guardrail** (always appended by the analyzer): Structural output format requirements (JSON schema for analysis, plain-text markdown for changelog). These cannot be overridden via config to prevent broken parsing.

Write prompts that pin an output contract rather than a topic list: small models reproduce a shape they have been shown and invent one otherwise: heading depth, bullet marker, reference form, and whether a `# Changelog - <app>` title appears at all drifted build to build before they did.

### What the model actually sees

`buildBasePrompt` writes memory, context files, then a **"Build metadata (context only, do not restate in the output)"** block. That label is load-bearing. It previously read `Environment: %s` while carrying `changelog.App`, which the git extractor sets to the **repo id**, so the model echoed the repo id back as an "Environment" heading in the changelog. `resolveAnalysisInput` now sets `App`, `SourceRepo` and `SourcePath` on the changelog explicitly, so app, repo and monorepo sub-path are three labelled fields rather than one ambiguous one.

`calculateDiff` returns real `object.Patch` output with hunks, and file stats counted from that patch. Until this was fixed it emitted only `diff --git` / `---` / `+++` header lines with no hunk bodies, and a summary whose insertion and deletion counts were **fixed constants** (10 for an added file, 5/2 for a modified one) presented to the model as fact. Every analysis in production up to that point was therefore inferred from commit subjects and filenames alone, which is why they read as generic and why risk levels moved between runs. `maxRawDiffBytes` (100 KB) bounds the patch; the prompt builder truncates again to fit the context window.

The "Heuristic Risk Indicators" block (`internal/git/risk_indicators.go`) is matched on **changed file paths**, scoped to the app's source paths, never on diff text: the old version grepped the whole diff for words like `schema` or `.json`, so migration and config fired on nearly every build. It flags migrations, contract changes (DTOs, controllers, routes, handlers, proto/OpenAPI/event schemas) and config changes (`.env*`, config files, k8s/kustomize/helm), lists the matched files, and reports environment variables that added lines newly read (`process.env`, `configService.get`, `os.Getenv`). Test files never count. Both the file list and the env vars come from the net from..to tree diff in `calculateDiff`, not from per-commit file lists, so a merge or an add-then-revert inside the range is not flagged; env vars are scanned on every patch, including those past `maxRawDiffBytes`. When the analysis falls back to a changelog without repo access, the indicators carry `unavailable: true` and the prompt says they were not computed instead of printing `false` for each.

### Usage

LLM is used for:
- **Changelog analysis**: Risk assessment of commits between versions
- **Changelog generation**: Structured human-readable changelog from git changes
- **Memory updates**: Memory edits proposed in the same response as the risk verdict, queued for human review (see [Repository Memory](#repository-memory))

## Repository Memory

Free-text project context, one document per **repo id** (not per app: one repo
can build several apps). `Scheduler.loadMemoryForBuild` reads it by
`build.source_repo` and `buildBasePrompt` prepends it under
`## Project Memory / Instructions` to **both** the risk analysis and the
changelog prompt. It is the only per-repo knob on LLM output that does not
require editing `config.yaml` and restarting: conventions, which paths are
high-risk, what a migration means in this repo, what noise to ignore.

The Memory tab exists so the prompts can actually be tuned without a deploy.

### How memory gets written

Two ways, and both end in the same place: an ordinary `repo_memory` version.

1. **By hand**, in the Memory tab.
2. **Proposed by the model, accepted by a human.** The analysis call already
   receives the current memory; its response schema now also carries
   `memory_updates`, so one prompt returns the risk verdict *and* any durable
   facts worth recording. No second LLM call, no extra cost per build.

**Nothing is ever applied automatically.** Proposals land in `memory_suggestions`
as `pending` and wait for someone to accept, edit or reject them. This is
deliberate: memory feeds the prompt of every later analysis, so an unreviewed
auto-write is a feedback loop that drifts with nothing to stop it. Accepting
writes a normal version, which means an accepted suggestion is diffable in the
history list and revertible with the same rollback as a hand edit.

Guards on the model's side, all in `sanitizeMemoryUpdates`:

- unknown actions and empty content are dropped (empty content is meaningful
  only as "remove this whole section");
- at most `maxMemoryUpdatesPerBuild` (5) entries per analysis, so a model that
  ignores "an empty list is the expected answer" cannot bury the reviewer;
- a malformed `memory_updates` entry **never** costs us the risk analysis that
  came back in the same object.

And on the queue's side: a partial unique index on
`(repo_id, action, content) WHERE status = 'pending'` means the same proposal
arriving on build after build occupies one row, not fifty.

`domain.ApplyMemoryUpdate` is the apply logic, and it never errors. An update
that matches nothing is a no-op reported as "already reflected in memory"
rather than a failed click. A `replace` whose section is missing **appends**
instead of dropping the text, because silently discarding what a human just
approved is the worse failure.

### Storage

`repo_memory` holds the current document; `repo_memory_versions` holds every
save. **History is append-only**: `MemoryStore.Save` takes the next version as
`MAX(version) + 1` over the *versions* table, not over the current row, so a
version number is never reused even after the current row is deleted. A rollback
is therefore a new version carrying old content, not a rewind, and
`DELETE /memory/:repo_id` clears only the current row: the text stays
recoverable through the history list.

`change_summary` is what the version list shows. The caller may send one
(`change_summary` in the save body, "what changed" in the UI); otherwise it
falls back to `Updated to version N`, and a rollback writes
`Rolled back to version N`.

Memory versions also feed the Activity tab: `ActivityStore` unions
`repo_memory_versions` in as `source: "memory"`, so an edit is visible in the
same feed as builds and deploys.

### Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/memory` | any user | All repos that currently have memory |
| `GET` | `/api/v1/memory/:repo_id` | any user | Current document |
| `GET` | `/api/v1/memory/:repo_id/versions` | any user | History (`?limit=`, default 50) |
| `POST` | `/api/v1/memory/:repo_id` | deployer+ | Save a new version |
| `DELETE` | `/api/v1/memory/:repo_id` | deployer+ | Clear the current document, keep history |
| `POST` | `/api/v1/memory/:repo_id/versions/:version/rollback` | deployer+ | Append that version's content as the new current |
| `GET` | `/api/v1/memory-suggestions` | any user | Proposals (`?repo=`, `?status=`, default `pending`, `all` for every status) |
| `POST` | `/api/v1/memory-suggestions/:id/accept` | deployer+ | Apply and save a new version; body may carry edited `content` |
| `POST` | `/api/v1/memory-suggestions/:id/reject` | deployer+ | Discard the proposal, memory untouched |

Suggestions sit on their own `/memory-suggestions` prefix rather than under
`/memory/:repo_id/...` so a literal path segment never has to coexist with the
repo wildcard. Each pending row is returned with a `preview` of the memory it
would produce, and the list carries `pending_by_repo` for the tab badge.
Accept and reject are idempotent-by-refusal: a second call gets `409`, so a
double click cannot apply twice.

Reads are open to viewers; the writes sit behind `RequireRole(RoleDeployer)`.
There is deliberately no per-app scope check here: memory is keyed by repo, and
a repo can serve apps a given deployer is not scoped to.

A repo with no memory yet answers **200 with `version: 0` and empty content**,
not 404 — the editor opens on that instead of special-casing "never written".
All responses are wrapped in `data`.

## External Analysis (re-analyzing the backlog)

The analysis pipeline is also exposed as data, so a stronger model driven from
outside (Claude Code as a proxy, say) can re-analyze builds without
reimplementing anything. Three moves: **pull the backlog, pull one build's
material and prompt, push the result back.**

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/analysis/queue` | any user | Which builds want analysis (`?status=`, `?app=`, `?repo=`, `?limit=`, `?after_id=`) |
| `GET` | `/api/v1/analysis/builds/:id/input` | any user | Everything for one build, incl. the exact prompt (`?prompt=false` to omit) |
| `POST` | `/api/v1/analysis/builds/:id/result` | deployer+ | Store an analysis computed elsewhere |
| `POST` | `/api/v1/analysis/reanalyze` | deployer+ | Re-queue a batch through the **built-in** analyzer |

**`?status=`** takes a real `analysis_status` (`pending`, `in_progress`,
`completed`, `failed`) plus three synthetic values: `needed` (pending or failed,
the default), `never` (no stored `llm_analysis` at all) and `all`. The
distinction matters: `analysis_status` is `NOT NULL DEFAULT 'pending'` with a
four-value CHECK, so it **cannot** express "never analyzed" — a fresh build and
a build queued right now both read `pending`. `never` tests `llm_analysis`
instead, and is what a backfill actually wants.

**Paging is `after_id`, not a time cursor**, and results come back id-ascending.
This listing exists to be walked to completion by a script, and SQLite's
lexicographic DATETIME comparison has broken cursor pagination here repeatedly
(see [Troubleshooting](#common-issues)). An integer id has none of that.

### The prompt is served, not described

`GET /analysis/builds/:id/input` returns `prompt.system` and `prompt.user`: the
**exact** strings `AnalyzeChangelog` would send. Both come from
`Scheduler.resolveAnalysisInput` and `ChangelogAnalyzer.AnalysisPrompt`, which
are the same functions the worker calls, so a proxy cannot drift from the
in-process analyzer as prompts change.

This works **even when no LLM provider is configured**. That is the normal case
for the proxy path (you are replacing the provider), so `promptRenderer()` builds
a provider-less analyzer purely to render. Rendering never touches the provider.

### Pushing results

The body mirrors the JSON in `AnalysisGuardrail`, plus optional `changelog_text`
and `analyzed_by`. `risk_level` and `risk_score` are validated; `memory_updates`
go through the same sanitizer and land in the same review queue as the worker's,
attributed to that build.

**It deliberately does not fire the analysis webhook.** This is the bulk path:
re-analyzing hundreds of builds must not replay hundreds of notifications for deploys
that shipped months ago. If you ever want the webhook here, make it opt-in per
request, never the default.

The SHA range is re-resolved through the scheduler on submit, so a pushed result
is indistinguishable from a locally computed one on every read path.

```bash
# 1. what needs analyzing
curl -s -H "$AUTH" "$API/api/v1/analysis/queue?status=never&limit=50"

# 2. the material and the prompt for one build
curl -s -H "$AUTH" "$API/api/v1/analysis/builds/1/input" | jq -r '.data.prompt.user'

# 3. hand the result back
curl -s -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"risk_level":"high","risk_score":74,"summary":"...","analyzed_by":"claude-code",
       "memory_updates":[{"action":"replace","section":"Conventions",
                          "content":"- migrations run in CI","reason":"..."}]}' \
  "$API/api/v1/analysis/builds/1/result"
```

`POST /analysis/reanalyze` is the other direction: it re-queues builds through
the **internal** analyzer (`{"build_ids":[...]}` or a `status`/`app`/`repo`
filter), bounded to 200 per call so one request cannot swamp the worker pool.

## Webhooks

Five-plus-one outbound event types, all queued into the `webhooks` table and delivered by a **single** goroutine, serially, in `(created_at, id)` order. Nothing is sent inline.

| Event | Emitted from |
|---|---|
| `analysis_completed` | `scheduler.finishBuildAnalysis` |
| `analysis_failed` | `scheduler.finishBuildAnalysis` (any non-`completed` terminal state) |
| `deployment_succeeded` | `PerformDeployment`, always on success |
| `deployment_failed` | `PerformDeployment`, when git ops error |
| `experiment_deployed` / `experiment_replaced` | `PerformDeployment`, before the succeeded event |
| `env_reset_requested` | `PerformDeployment`, when a deploy is parked by the reset gate |

### One message per build, in a fixed order

Build registration used to submit the analysis task and the auto-deploy task as **siblings** into the same 3-worker pool. They raced: whichever finished first was delivered first, so the same pipeline emitted `deployment_succeeded` before `analysis_completed` on one build and the reverse on the next. Worse, a deploy that won the race read its `risk_level` / `risk_score` / `summary` / `concerns` / `changelog_text` off a build row analysis had not written yet, and shipped all five **empty**.

Auto-deploy is now **chained onto** the analysis task instead of racing it:

- `RegisterBuild` sets `Task.ChainAutoDeployEnv` on the analysis task rather than submitting a second task (`builds_handler_gin.go`). It only submits an auto-deploy directly when no analysis will run (experiment builds) or when the analysis submit was rejected.
- `processBuildAnalysis` defers `finishBuildAnalysis`, so the chained deploy and the webhook happen on **every** exit path — including the panic unwind, which leaves the row on `in_progress` and is therefore treated as a failure.
- `finishBuildAnalysis` **withholds** `analysis_completed` when a deploy is chained, because `deployment_succeeded` already carries the same five fields. One message, not two.
- `analysis_failed` is **never** withheld: no deployment payload can express "the LLM never ran". The deploy still proceeds.
- If the chained deploy is then skipped (an experiment occupies staging) or cannot be queued, the withheld `analysis_completed` is released instead — a build must never go silent.

Both terminal deployment payloads carry `analysis_status`, so a receiver can tell "no risk data because analysis failed" from "no risk data because none was requested".

**When touching this**: the invariant is *exactly one webhook per build on the happy path, and never zero*. `internal/worker/analysis_webhook_test.go` asserts it by reading the queue rows directly, since that queue is the delivery order.

## Environment Reset Gate

An experiment leaves state behind (db rows, queue contents, cached blobs). The
gate makes that teardown an explicit, audited step instead of a Slack message
and a confirm dialog: the deploy that would displace the experiment is **parked**
until a reset runner reports the environment clean.

Enable per environment (off by default):

```yaml
environments:
  staging:
    baseline: prod
    reset:
      enabled: true
      webhook_url: "https://runner.internal/reset-staging"  # falls back to webhook.urls
      timeout: 1800      # seconds; an unanswered reset is failed after this
      auto_deploy: true  # run the parked deploy when the reset completes
```

### Flow

1. `POST /api/v1/deployments` for an env whose live build is an experiment on a
   **different** branch than the one being deployed.
2. `PerformDeployment` opens an `env_resets` row (`pending`) **before creating
   the deployment row**, so a parked deploy leaves nothing pending behind, and
   returns `202` with the reset. No git operation runs.
3. An `env_reset_requested` webhook goes out carrying `callback_url`,
   `callback_token`, `status_url`, and both the experiment being torn down and
   the build waiting to land.
4. The runner optionally reports `running`, then `completed` (or `failed`).
5. On `completed`, if `auto_deploy` is on, the parked deploy runs on a
   background goroutine and its result lands in `deployment_id` / `deploy_error`
   on the reset row. From here it is an ordinary deployment: the usual
   `deployment_succeeded` / `deployment_failed` webhooks fire.

### State machine

`pending → running → completed | failed`, plus `pending|running → canceled`
(operator gave up on the parked deploy). Terminal rows never transition again:
a duplicate callback gets `409` with the current row, so a retrying runner can
see where it landed instead of re-triggering a deploy.

### When the gate closes

`envResetRequired` in `deployments_handler_gin.go`:

- the env's live deployment is an experiment build (branch != source repo's
  configured branch), **and**
- the build being deployed is on a different branch (same-branch redeploy is
  lease renewal, nothing to tear down), **and**
- no `completed` reset for that `(app, env)` finished *after* that experiment
  deployment landed.

A second deploy ask while a reset is open **retargets** the open row (newest ask
wins the parking slot) rather than queueing a second teardown. A partial unique
index enforces one open reset per `(app_name, environment)`.

Resets that nobody answers are failed lazily on the read paths
(`EnvResetStore.ExpireStale`) rather than by a ticker, so a stuck runner cannot
block the environment forever.

### Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/env-resets` | user token | List resets (`?app=`, `?environment=`, `?status=`, `?active=true`) |
| `GET` | `/api/v1/env-resets/:id` | user token or reset token | "Is it finished?" poll |
| `POST` | `/api/v1/env-resets/:id/status` | user token or reset token | Report `running` / `completed` / `failed` / `canceled` |
| `POST` | `/api/v1/env-resets/:id/cancel` | user token or reset token | Abandon the parked deploy |
| `GET` | `/external/env-resets/:id` | CI key **and** reset token | Same poll, for the runner |
| `POST` | `/external/env-resets/:id/status` | CI key **and** reset token | Same callback, for the runner |

The per-reset `token` is generated on creation, goes out **once** in the webhook
payload, and is never serialized in an API response (`json:"-"`). Without it, a
caller needs a session that may deploy to that environment (`CanDeployToEnv`) —
which is also the break-glass path: an admin can mark a reset completed by hand.

```bash
curl -X POST https://deploy-api.example.com/external/env-resets/42/status \
  -H "Authorization: Bearer ${DEPLOY_API_KEY}" \
  -H "X-Reset-Token: ${CALLBACK_TOKEN}" \
  -d '{"status": "completed", "note": "db restored from baseline snapshot"}'
```

### Writing a reset runner

A runner is any HTTP service that accepts the `env_reset_requested` payload. It
must not answer synchronously: webhook delivery times out at 30s and retries 3
times, while restoring a database usually takes minutes. Answer `202`
immediately, run the reset off-request, and report out-of-band to
`callback_url` with `X-Reset-Token` plus the CI key as `Authorization`.

- `running` is advisory: send it once, fire-and-forget, so an unreachable
  Gondola cannot delay the reset. Retry the terminal report with backoff.
- A `4xx` on a callback means the reset is already terminal: log it, do not retry.
- Run one reset at a time. Answer `202` to a redelivery of the reset already
  running and `409` to a different one while busy; `409` is not 2xx, so Gondola
  retries it later.
- Expose the reset route on its own listener when the runner also serves other,
  more sensitive routes, so a leaked token reaches nothing else.

### Notes for whoever touches this next

- `PerformDeployment` signals parking with `*resetPendingError`, which unwraps to
  `domain.ErrDeployParked`. The auto-deploy worker checks that sentinel so a
  parked build is logged as waiting, not as a deploy failure. Any new caller of
  `PerformDeployment` must do the same.
- `POST /deployments` answers `202` (not 201, not an error) when parked; the body
  carries `status: "awaiting_env_reset"`, the reset, and `auto_deploy`.
- The UI parks the deploy button (`state.deployStatus[key].phase === 'reset'`)
  and polls `GET /env-resets/:id`; `resumeEnvResetWatches()` re-attaches after a
  page reload so a parked deploy never looks like it vanished.

## Security Considerations

### Authentication

Three principals, checked in order by middleware (`TokenAuth` → `APIKeyAuth`):

1. **User token** (`dp_*`): looked up by hash in `tokens` table. Sets full user context (ID, role, allowed envs, username) in Gin context. The day-to-day path for everything on `/api/v1/*`.
2. **Root key** (`DEPLOY_ROOT_KEY`): break-glass. Valid on any path, sets a synthetic admin context (no real user record) and logs each use. Use this only for bootstrap or recovery — it has full `/api/v1/*` admin access and is not auditable to a real user.
3. **CI API key** (`DEPLOY_API_KEY`): scoped to `/external/*` only. Rejected with 401 on `/api/v1/*`. No user context; intended for firewall-isolated CI build registration.

The root key and the CI key must not be set to the same value (config rejects this at load time).

All endpoints except `/health`, `/ready`, `/api/v1/auth/login`, and static files require authentication.

### Role-Based Access Control (RBAC)

Three roles with hierarchical permissions (`admin` > `deployer` > `viewer`):

| Role | Builds | Deploy | Deploy Envs | Deploy Apps | User Management |
|------|--------|--------|-------------|-------------|-----------------|
| `admin` | create/read | all envs | all | all | full (CRUD) |
| `deployer` | create/read | allowed envs | per `allowed_environments` | per [app scope](#app-scope-per-app-deploy-permission) | none |
| `viewer` | read-only | none | none | none | none |

A deploy is allowed only when **both** halves pass: the environment
(`CanDeployToEnv`) and the app name (`CanDeployToApp`).

**Key implementation details:**

- **Domain model** (`internal/domain/user.go`): `User.Role` string field, `User.AllowedEnvironments` JSON array, helper methods `CanDeployTo(env)`, `CanDeployApp(app)`, `AppScope()` and `CanWrite()`
- **Middleware** (`internal/api/middleware_gin.go`):
  - `TokenAuth` sets `user_role`, `user_allowed_envs` and `user_app_scope` in Gin context
  - `RequireRole(minRole)` — route-level middleware, returns 403 if user's role level is insufficient
  - `CanDeployToEnv(c, env)` — handler-level helper for env-specific permission checks
  - `CanDeployToApp(c, app)` — handler-level helper for app-name permission checks
- **Root key** sets a synthetic admin context (`user_role = "admin"`), so it passes `RequireRole("admin")`, `CanDeployToEnv` and `CanDeployToApp` for anything. **CI API key never reaches RBAC** — it's rejected at the middleware layer on `/api/v1/*`.
- **No-context = deny**: `RequireRole`, `CanDeployToEnv` and `CanDeployToApp` all deny if no role context is set. This is defensive — under the auth model above, no `/api/v1/*` request reaches RBAC without a role.
- **Enforcement points**:
  - `CreateBuild`: viewers get 403
  - `CreateDeployment`: checks `CanDeployToEnv` for the target environment **and** `CanDeployToApp` for the build's app
  - `EnvResetsHandler.authorize`: a session (rather than the per-reset token) must pass both checks for the reset's `(app, environment)`
  - `/api/v1/auth/users/*` and `/api/v1/auth/groups/*`: admin-only via `RequireRole("admin")`
  - `DELETE /api/v1/apps/:name`: admin-only
  - Memory writes (`POST`/`DELETE /api/v1/memory/*`): `RequireRole("deployer")`; reads stay open to viewers
  - `UpdateUser`: rejects self-downgrade (admin cannot strip their own admin role; recovery via another admin or root-key/SQLite break-glass)
- **Database**: `users.role` column (`CHECK IN ('admin','deployer','viewer')`, default `'admin'`), `users.allowed_environments` / `users.allowed_apps` / `users.denied_apps` nullable JSON arrays, plus `app_groups` and `user_app_groups`

### App Scope (per-app deploy permission)

Environments alone could not express "alice owns the backend services she
maintains, but not `core-api`". App scope adds a second axis: a set of
regexes matched against the **app name**.

An **app group** is a named pattern list (`backend`, `frontend`, …) that several
deployers share. A user's effective scope is:

```
allow = union(patterns of every group the user is in) + user.allowed_apps
deny  = user.denied_apps
```

`AppScope.Permits(app)` then applies, in this order:

1. a `deny` match refuses, whatever the groups grant — this is the exception mechanism;
2. an **empty** `allow` means every app, so existing deployers keep working unchanged;
3. otherwise the app must match at least one `allow` pattern.

**Patterns match the whole app name.** `domain.compilePattern` wraps each one as
`^(?:P)$`, so `core-api` does not also grant `core-api-worker`; write
`core-api.*` when you mean the prefix. Patterns are compiled (and cached) on
first use and **validated at write time** — a group or user update carrying an
uncompilable regex is rejected with 400 rather than silently denying every deploy.

Admins are never restricted by patterns; viewers never pass regardless of them.

Worked example, the case this exists for:

```
group "backend"  patterns: ["billing-.*", "notifier", "queue-.*"]
user  alice      groups: [backend]  allowed_apps: ["search-api"]  denied_apps: ["core-api"]
                 allowed_environments: ["prod", "staging"]
```

`billing-worker` and `search-api` deploy; `core-api` and `reports-api` return
403 `you don't have permission to deploy <app>`.

The Admin tab's scope editor previews the result live: it recomputes the matched
app list from `state.apps` on every keystroke, so an admin sees exactly which
apps a pattern set grants before saving. The JS mirrors the Go matcher
(`userAppScope` / `scopePermits` in `web/static/app.js`) and uses the same
anchoring — but it only decides whether to *draw* the deploy button. The server
is the authority.

### App Group Endpoints (Admin Only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/auth/groups` | List groups with patterns and member counts |
| `POST` | `/api/v1/auth/groups` | Create a group (`name`, `description`, `patterns`) |
| `PUT` | `/api/v1/auth/groups/:id` | Rename a group or rewrite its patterns |
| `DELETE` | `/api/v1/auth/groups/:id` | Delete a group; **409 while it still has members** |

Deleting a group with members is refused (409). Rule 2 above means a deployer
left with no allow patterns may deploy **everything**, so cascading the delete
would silently widen access; detaching members first makes that an explicit
choice, visible in the scope editor's preview.

Foreign keys are off in this database (see `sqlite.go`), so `AppGroupStore.Delete`
and `UserStore.Delete` clear `user_app_groups` by hand. A new membership table
join must do the same.

### User Management Endpoints (Admin Only)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/auth/users` | List all users |
| `POST` | `/api/v1/auth/users` | Create user with role, `allowed_environments`, `allowed_apps`, `denied_apps`, `group_ids` |
| `PUT` | `/api/v1/auth/users/:id` | Update role, `allowed_environments`, `allowed_apps`, `denied_apps`, `group_ids`, password |
| `DELETE` | `/api/v1/auth/users/:id` | Delete user (cannot delete self) |

### User Attribution

- **Builds**: `built_by` combines the request body value with the authenticated username. If body provides `"ci-pipeline"` and user is `"admin"`, result is `"ci-pipeline (via admin)"`. If body is empty, uses the username directly.
- **Deployments**: `deployed_by` is set from the authenticated username in `TokenAuth` middleware.
- **Memory**: `updated_by` prefers the authenticated username if not provided in the request body.

### Git Security

- **SSH Keys**: Mount as Docker volume, never commit to repository
- **Local repos**: Use for testing only, no authentication required
- **Repository isolation**: Each repo has its own mutex to prevent race conditions

### Secrets Management

Files that must never be committed (see `.gitignore`):
- `.env` - Environment variables with secrets
- `config.local.yaml` - Local config with file paths
- `config.prod.yaml` - Production config with real URLs
- `ssh-key`, `*.pem`, `*.key` - SSH private keys
- `data/*.db*` - Database files

### API Security

- **Request timeouts**: 120 seconds for long operations (git, LLM)
- **Input validation**: Required fields checked in handlers
- **SQL injection prevention**: Parameterized queries in all stores
- **No CORS configuration**: Designed for internal use

### External API Endpoint

A separate external endpoint is available for CI/CD systems that should be firewall-protected separately from the main API:

| Method | Endpoint | Auth | Purpose |
|--------|----------|------|---------|
| POST | `/external/builds` | `DEPLOY_API_KEY` | Register builds from CI pipelines |

**Key differences from `/api/v1/builds`:**
- **Authentication**: CI key (`DEPLOY_API_KEY`) only. User tokens (`dp_*`) and the root key are also accepted but `/external/*` is the *only* path the CI key works on.
- **Firewall**: This group sits in its own `r.Group("/external")` so it can be exposed on a separate listener/path from `/api/v1/*`.
- **Functionality**: Identical build registration logic.

**Example usage:**
```bash
curl -X POST https://deploy-api.example.com/external/builds \
  -H "Authorization: Bearer ${DEPLOY_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "app": "my-app",
    "source_repo": "app-my-app",
    "branch": "main",
    "commit_sha": "abc123",
    "image_tag": "v1.0.0",
    "image_uri": "registry.com/my-app:v1.0.0",
    "built_by": "ci-pipeline"
  }'
```

This endpoint is ideal for CI systems that need to register builds but shouldn't have access to the full internal API.

## Development Workflow

### Local Development Setup

1. Create local test repos (paths referenced from `config.local.yaml`):
   - `argocd-staging/` - ArgoCD manifest repo
   - `test-app-source/` - Application source repo

2. Copy and edit environment:
   ```bash
   cp .env.example .env
   # Edit with your API keys (optional, for LLM features)
   ```

3. Start server:
   ```bash
   ./start-local.sh
   ```

4. Run tests:
   ```bash
   ./test-local.sh
   ```

### Adding New Endpoints

1. Define request/response models in `internal/domain/`
2. Create handler in `internal/api/<feature>_handler_gin.go`
3. Register routes in `internal/api/router_gin.go`
4. Add RBAC enforcement if needed (use `RequireRole()` middleware or `CanDeployToEnv()` helper)
5. Add tests in `internal/api/handlers_test.go`

### Adding New LLM Providers

1. Implement `llm.Provider` interface in `internal/llm/<provider>.go`
2. Add to `llm.NewProvider()` factory function
3. Update configuration documentation

## Web UI

The web UI lives in `web/static/` and is a vanilla JS SPA served directly by the Go server.

### Files

- `web/static/index.html` — HTML shell + all CSS styles (dark GitHub-like theme)
- `web/static/app.js` — All application logic (state, API calls, rendering)

### Key Conventions

- **State** is a single global `state` object; all mutations happen there, followed by `renderApp()` / `render()`.
- **API calls** go through the `api(path, options)` helper, which injects the Bearer token and handles 401 redirects to login.
- **Rendering** is full DOM replacement (`innerHTML`) — no virtual DOM or reactive framework. Avoid querying DOM elements outside of event handlers set up immediately after `innerHTML` assignment.
- **HTML escaping**: always wrap user/API data with `escapeHtml()` before interpolating into template literals. `escapeHtml()` does **not** escape `"` — for anything going into an attribute value (`title="..."`, `data-*`), use `escapeAttr()` instead.
- **Date display**: use `dateSpan(dateStr, className?)` (returns a `<span title="ISO">relative</span>`) wherever a date is rendered. Never call `formatDate()` directly in templates. In dense tables use `formatAge(dateStr)` (`"2h"`, `"3d"`, `"5w"`) and put the full timestamp in the surrounding element's `title`.
- **RBAC helpers**: `userCanWrite()`, `userCanDeployTo(env)`, `userIsAdmin()` — use these to conditionally render action buttons.
- **JSON field names**: API responses use `snake_case`. Key mappings to watch:
  - `repo.repo_type` (not `type`), `repo.git_url` (not `url`)
  - `commit.timestamp` (not `date`)
  - `app.repo_id` (not `source_repo`)
  - `build.llm_analysis` (the only analysis field; no `build.analysis` fallback)
  - `user.role` (`"admin"`, `"deployer"`, `"viewer"`), `user.allowed_environments` (array or null)

### Dashboard (deployment matrix)

The Dashboard is a single dense table: one row per app, one column per environment, plus a "Latest build" column. It answers "what is running where, and what is behind" without scrolling. Build history lives in the Builds tab.

- **Row rendering**: `renderDashboard()` → `renderDashRows()` → `renderDashEnvCell()` / `renderDashLatestCell()`.
- **App cell hover**: `dashChangesTip()` shows the changelog of the newest main-branch build, headed "Not in prod yet" or "Live in prod". Staging analysis diffs against prod, so that changelog is what prod lacks. Falls back to the LLM summary, then commit subjects.
- **Per-app rollup**: `getAppDashInfo(app)` computes everything the row and the stat strip need (per-env deployment state, how many builds each env is behind, experiment count, undeployed count, failed analyses). It is cached into `state.dashInfos` on each full render.
- **Env columns**: `dashEnvColumns()` returns the union of per-app envs and envs that actually have a deployment, ordered by `ENV_ORDER` (`staging`, `prod`, `newprod`, then anything else). A cell shows `—` when the app targets the env but nothing is deployed, and `n/a` when the env is out of scope for that app. A deployment in an env the repo config no longer declares still renders — it is not hidden as `n/a`.
- **Markers**: `▲N` (amber) = N loaded builds newer than what is deployed there, `N+` when the loaded page is capped; `⚠` = experiment live; `✓` = every env on the latest build (suppressed when an experiment is live). Once an app has a prod deployment, only prod counts toward "envs behind" and `✓`; the per-cell `▲N` still shows on every env. Risk is shown only for `medium`/`high`/`critical` — `low`/`unknown` is noise at this density.
- **Filtering**: the stat chips set `state.dashMode` (see `DASH_MODES`); the text box sets `state.dashFilter`. Name matches win outright; repo matching is only a fallback so typing an app name doesn't drag in every sibling app from its repo.
- **Partial re-render**: `refreshDashBody()` rewrites only `#dash-body` so the filter input keeps focus. `renderApp()` (used by the 5s analysis poll) saves and restores the filter's focus and caret.
- **Row expansion**: clicking a row toggles `state.expandedApps` and renders the app's builds inline using `renderBuildItem()` — the same renderer as the Builds tab, so deploy/regen buttons and RBAC gating come along for free.
- **Delete / restore** (admin only): each row carries a hover-revealed `×` that soft-deletes the app (`DELETE /api/v1/apps/:name`), and the toolbar's "show deleted" checkbox reloads `/apps?include_deleted=true` so deleted rows come back with a `deleted` badge and a `↻` restore action. Deleting only hides the app: builds and deployments stay, and the next build registration undeletes it (`AppStore.Save` clears `deleted_at`).
- **Event wiring**: row, stat and app-action clicks use one delegated `document` listener keyed on `data-app` / `data-dash-mode` / `data-app-action`, so app names never have to survive a trip through an inline `onclick` attribute. The `data-app-action` branch runs first and calls `stopPropagation()`, otherwise deleting an app would also expand its row.

### Memory tab

Master-detail editor for [repository memory](#repository-memory): the
application repos down the left, the document plus its history on the right.

- **Repo list**: `memoryRepoIds()` is the `application`-typed repos plus any repo id that has memory but no longer appears in the config, so an orphaned document is still reachable. A green dot means memory is set; each row shows version, size and age.
- **Typing must not re-render.** The rest of the SPA replaces the whole DOM on every state change, which would destroy the textarea's focus and caret on each keystroke. `onMemoryInput()` therefore writes `state.memoryDraft` and hand-updates only the three labels that depend on it (`#mem-dirty`, `#mem-count`, `#mem-save`). Do not add a `renderApp()` to that path.
- **Something else will re-render anyway.** The 5s analysis poll calls `renderApp()` regardless of the active tab, so every editable field here must survive that. Two things make it safe: the field's value lives in `state` (`memoryDraft`, `memorySummary`) so `innerHTML` rebuilds it, and its id is in `RESTORABLE_INPUTS`, which `renderApp()` uses to put focus and caret back. A new input on this tab needs **both** or it will silently lose what someone typed.
- **Dirty tracking**: `state.memoryDraft` vs `state.memorySaved`. Switching repos with unsaved edits confirms first; `Revert` restores the last saved text.
- **History**: click a version to expand its content, `Restore` calls the rollback endpoint. A restore appends, so the list only ever grows.
- **Cleared vs never written**: with history but no current row the header reads "cleared · restore a version below", not "never saved" — otherwise clearing looks like data loss.
- **Suggestions panel**: pending LLM proposals for the selected repo render above the editor, each with its action, target section, reason and source build. `Accept` applies it, `Edit` opens the text for correction before accepting, `Reject` discards it. The tab carries a `.tab-badge` with the total pending count and each repo row a `.sugg-count`.
- **After an accept the editor reloads.** Accepting rewrites memory server-side, so `resolveSuggestion` re-reads it into `memoryDraft`/`memorySaved`; skipping that would leave the pre-accept text on screen looking like unsaved work.
- **RBAC**: viewers get a `readonly` textarea, no Save/Clear/Restore, no accept/reject and a read-only note; `userCanWrite()` gates all of it.

### RBAC in the UI

- **Viewers**: deploy buttons, sync buttons, and regen buttons are hidden. All tabs except Admin are visible (read-only); the Memory tab renders as a read-only textarea.
- **Deployers**: deploy buttons shown only for environments in `user.allowed_environments` (or all if empty) **and** apps their scope permits (`userCanDeployApp`). Sync and regen buttons visible.
- **Admins**: full access. Admin tab visible with user management panel (create/edit/delete users, change roles, set allowed environments).
- **Role badge**: shown next to username in header. CSS classes: `.role-admin` (blue), `.role-deployer` (green), `.role-viewer` (gray).

### Adding / Changing UI Features

1. Modify `web/static/app.js` for logic and rendering.
2. Add/update CSS in the `<style>` block inside `web/static/index.html`.
3. There is no build step — changes are served immediately.

### Risk Badge Classes

| Class | Colour | When |
|---|---|---|
| `risk-critical` | red bold | `risk_level === "critical"` |
| `risk-high` | red | `risk_level === "high"` |
| `risk-medium` | yellow | `risk_level === "medium"` |
| `risk-low` | green | `risk_level === "low"` |
| `risk-unknown` | gray | `risk_level === "unknown"` (LLM parse failure) |

### Role Badge Classes

| Class | Colour | When |
|---|---|---|
| `role-admin` | blue | `user.role === "admin"` |
| `role-deployer` | green | `user.role === "deployer"` |
| `role-viewer` | gray | `user.role === "viewer"` |

---

## Deployment

### Docker Image

- Multi-stage build for minimal image size
- Non-root user (`deploy-api`)
- Health check on `/health` endpoint
- Volumes for data persistence: `/app/data`, `/app/repos`, `/etc/deploy-api`

### Required Runtime Dependencies

- `git` - For repository operations
- `openssh-client` - For SSH authentication
- `kustomize` - For manifest editing
- `ca-certificates` - For HTTPS API calls

### Health Endpoints

- `GET /health` - Returns `{"data": {"status": "healthy"}}` (always 200)
- `GET /ready` - Returns `{"data": {"status": "ready"}}` (checks dependencies)

## Troubleshooting

### Common Issues

1. **SQLite locking errors**: WAL mode enabled, should be rare
2. **Git authentication failures**: Check SSH key permissions (600) and format
3. **LLM timeouts**: Large changelogs may exceed timeout; increase in config
4. **Kustomize not found**: Ensure `kustomize` binary is in PATH or set full path
5. **Cursor-based pagination breaks (same items returned on "Load More")**:
   SQLite stores `DATETIME` as TEXT and compares values lexicographically. This has caused recurring bugs:
   - **Timezone mismatch**: `time.Now()` stores timestamps in the server's local timezone (e.g., `"T12:...-03:00"`). If the cursor decodes with `.UTC()`, the parameter becomes `"T15:...Z"`. String comparison makes every stored value appear "before" the cursor, returning the first page forever. **Never call `.UTC()` on decoded cursor times** — use `time.Unix(0, ns)` (local) to match `time.Now()`.
   - **Precision mismatch**: `time.Now()` has nanosecond precision (e.g., `".123456789Z"`). If the cursor truncates to milliseconds (`".123Z"`), the trailing digits cause `'4' < 'Z'` in ASCII, making the cursor's own row appear "before" itself. **Always use `UnixNano()` / `time.Unix(0, ns)` for cursor encoding/decoding**, never `UnixMilli()`.

   Any new cursor-paginated endpoint must follow this pattern. See `builds_handler_gin.go` and `deployments_handler_gin.go` for the correct implementation.

6. **`400 invalid request body` on anything over ~1 KB**: `ErrorBodyLogger` runs
   before every handler and used to buffer the request with
   `io.LimitReader(body, errorBodyMaxBytes+1)` and then hand *that* back as
   `c.Request.Body`. Any POST larger than 1025 bytes reached its handler
   truncated mid-JSON, so it failed to bind. It hit memory saves, pushed
   analyses and suggestion accepts alike, and the 400 blamed the caller. The
   logger now buffers the whole body (capped by `requestBodyMaxBytes`, 8 MB) and
   truncates only what reaches the log, in `formatRequestBody`. A middleware that
   reads `c.Request.Body` must always restore every byte it consumed.

### Debug Mode

Set `DEBUG=true` environment variable for verbose logging of config loading.
