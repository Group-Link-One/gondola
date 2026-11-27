package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig holds the server configuration
type ServerConfig struct {
	Port    int    `yaml:"port"`
	APIKey  string `yaml:"api_key"`
	RootKey string `yaml:"root_key"` // Break-glass key, full access on /api/v1/*. Empty = disabled.
}

// DatabaseConfig holds the database configuration
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// OverlayConfig represents a single kustomize overlay within a deployment
type OverlayConfig struct {
	Name string `yaml:"name"` // Human-readable label (e.g., "api", "crontab")
	Path string `yaml:"path"` // Relative to base_path (e.g., "overlay-api")
}

// DeploymentConfig holds per-app deployment strategy configuration
type DeploymentConfig struct {
	Strategy       string          `yaml:"strategy"`        // "kustomize" (only supported value for now)
	BasePath       string          `yaml:"base_path"`       // Relative to ArgoCD repo root
	Overlays       []OverlayConfig `yaml:"overlays"`        // Explicit overlay list
	OverlayPattern string          `yaml:"overlay_pattern"` // Glob pattern (e.g., "overlay-*")
}

// RepoConfig holds configuration for a single repository
type RepoConfig struct {
	URL               string                        `yaml:"url"`
	Type              string                        `yaml:"type"`
	Environment       string                        `yaml:"environment,omitempty"`
	Branch            string                        `yaml:"branch"`
	Environments      []string                      `yaml:"environments,omitempty"`        // For app repos: which envs can be deployed to
	Deployment        *DeploymentConfig             `yaml:"deployment,omitempty"`          // Legacy single deployment config (fallback)
	Deployments       map[string]*DeploymentConfig  `yaml:"deployments,omitempty"`         // Per-env deployment configs (preferred)
	SourcePaths           []string                      `yaml:"source_paths,omitempty"`            // Path prefixes for changelog filtering (monorepo support)
	ContextFiles          []string                      `yaml:"context_files,omitempty"`           // Files always included in LLM prompt as reference context
	AutoDeployStaging     bool                          `yaml:"auto_deploy_staging,omitempty"`     // If true, main-branch builds auto-deploy to staging unless an experiment is active
	AutoDeployExperiments bool                          `yaml:"auto_deploy_experiments,omitempty"` // If true, first-time experiment promotion (over trunk or no prior deploy) auto-deploys instead of waiting for manual promotion. Cross-experiment swap is still always manual — the system can't reset stateful services between experiments.
	AllowExperiments      bool                          `yaml:"allow_experiments,omitempty"`       // ARGOCD repos only: if true, experiment-branch builds may deploy here. Default false (only main-branch builds allowed). Application repos ignore this field.
}

// ResolveSourcePaths returns the effective source paths for changelog filtering.
// If requestPath is set (from the build request), it takes precedence over config.
// Otherwise, returns the configured SourcePaths (may be nil/empty = no filtering).
func (r *RepoConfig) ResolveSourcePaths(requestPath string) []string {
	if requestPath != "" {
		return []string{requestPath}
	}
	return r.SourcePaths
}

// GitConfig holds git-related configuration
type GitConfig struct {
	SSHKeyPath  string `yaml:"ssh_key_path"`
	AuthorName  string `yaml:"author_name"`
	AuthorEmail string `yaml:"author_email"`
	// ReposDir is the base directory where repos are cloned. Defaults to "./repos".
	ReposDir string `yaml:"repos_dir,omitempty"`
}

// KustomizeConfig holds kustomize configuration
type KustomizeConfig struct {
	BinaryPath string `yaml:"binary_path"`
}

// LLMPrompts holds customizable prompt templates for LLM operations
type LLMPrompts struct {
	AnalysisSystem  string `yaml:"analysis_system"`  // Custom system prompt for risk analysis
	ChangelogSystem string `yaml:"changelog_system"` // Custom system prompt for changelog generation
}

// LLMProviderConfig holds configuration for LLM providers
type LLMProviderConfig struct {
	Type    string            `yaml:"type"` // "cloudflare", "ollama", "openrouter"
	APIKey  string            `yaml:"api_key"`
	BaseURL string            `yaml:"base_url"`
	Model   string            `yaml:"model"`
	Options map[string]string `yaml:"options"`
	Prompts LLMPrompts        `yaml:"prompts"`
}

// WebhookConfig holds webhook notification configuration
type WebhookConfig struct {
	// URLs to send webhooks to (supports multiple endpoints)
	URLs []string `yaml:"urls"`
	// Headers to include in all webhook requests
	Headers map[string]string `yaml:"headers"`
	// Timeout for HTTP requests (seconds)
	Timeout int `yaml:"timeout"`
	// MaxRetries per webhook
	MaxRetries int `yaml:"max_retries"`
	// RetryBackoff base seconds between retries
	RetryBackoff int `yaml:"retry_backoff"`
	// PollInterval for checking pending webhooks (seconds)
	PollInterval int `yaml:"poll_interval"`
	// BaseURL for generating links in webhook payloads (e.g., "https://deploy-api.example.com")
	BaseURL string `yaml:"base_url"`
}

// EnvironmentConfig holds configuration for a deployment environment
type EnvironmentConfig struct {
	Baseline string          `yaml:"baseline"`        // which env's last deploy to compare against
	Reset    *EnvResetConfig `yaml:"reset,omitempty"` // gate for deploys that displace an experiment
}

// EnvResetConfig gates the next deploy after an experiment has occupied the
// environment: Gondola parks the deploy, fires an env_reset_requested webhook,
// and waits for the reset runner to report back before touching git.
type EnvResetConfig struct {
	Enabled bool `yaml:"enabled"`
	// WebhookURL receives env_reset_requested. Falls back to webhook.urls.
	WebhookURL string `yaml:"webhook_url,omitempty"`
	// Timeout in seconds before an unanswered reset is failed. Default 3600.
	Timeout int `yaml:"timeout,omitempty"`
	// AutoDeploy runs the parked deploy once the reset completes. Default true.
	AutoDeploy *bool `yaml:"auto_deploy,omitempty"`
}

// ResetTimeout is how long a reset may stay open before it is failed.
func (c *EnvResetConfig) ResetTimeout() time.Duration {
	if c == nil || c.Timeout <= 0 {
		return time.Hour
	}
	return time.Duration(c.Timeout) * time.Second
}

// AutoDeployAfterReset reports whether the parked deploy runs on completion.
func (c *EnvResetConfig) AutoDeployAfterReset() bool {
	if c == nil || c.AutoDeploy == nil {
		return true
	}
	return *c.AutoDeploy
}

// ResetConfigFor returns the reset gate config for an environment, or nil when
// the environment has no gate configured or it is disabled.
func (c *Config) ResetConfigFor(env string) *EnvResetConfig {
	if c == nil {
		return nil
	}
	envCfg, ok := c.Environments[env]
	if !ok || envCfg.Reset == nil || !envCfg.Reset.Enabled {
		return nil
	}
	return envCfg.Reset
}

// DiscoveryConfig controls the ArgoCD Application auto-discovery scanner.
type DiscoveryConfig struct {
	// RefreshInterval is the period between automatic background scans (seconds).
	// Defaults to 3600 (1 hour). Set 0 to disable periodic refresh.
	RefreshInterval int `yaml:"refresh_interval,omitempty"`
	// RequestTimeout caps a single admin-triggered scan (seconds). Defaults to 60.
	RequestTimeout int `yaml:"request_timeout,omitempty"`
}

// ArgoCDAPIConfig enables refreshing Argo CD Applications after a push; empty URL or token disables it.
type ArgoCDAPIConfig struct {
	URL   string `yaml:"url,omitempty"`
	Token string `yaml:"token,omitempty"`
}

// Enabled reports whether both URL and token are set.
func (c ArgoCDAPIConfig) Enabled() bool {
	return c.URL != "" && c.Token != ""
}

// Config is the root configuration structure
type Config struct {
	Server       ServerConfig                 `yaml:"server"`
	Database     DatabaseConfig               `yaml:"database"`
	Repos        map[string]RepoConfig        `yaml:"repos"`
	Git          GitConfig                    `yaml:"git"`
	Kustomize    KustomizeConfig              `yaml:"kustomize"`
	LLM          LLMProviderConfig            `yaml:"llm"`
	Webhook      WebhookConfig                `yaml:"webhook"`
	Environments map[string]EnvironmentConfig `yaml:"environments"`
	Discovery    DiscoveryConfig              `yaml:"discovery,omitempty"`
	ArgoCDAPI    ArgoCDAPIConfig              `yaml:"argocd_api,omitempty"`
}

// Load loads configuration from a YAML file and applies environment variable substitution
func Load(path string) (*Config, error) {
	// Load .env file if it exists (in the same directory as the config or current directory)
	loadEnvFile(filepath.Join(filepath.Dir(path), ".env"))
	loadEnvFile(".env")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Debug: print SSH key path from env
	if os.Getenv("DEBUG") == "true" {
		fmt.Fprintf(os.Stderr, "[DEBUG] Config path: %s\n", path)
		fmt.Fprintf(os.Stderr, "[DEBUG] CF_API_KEY from env: %s...\n", os.Getenv("CF_API_KEY")[:min(10, len(os.Getenv("CF_API_KEY")))])
		fmt.Fprintf(os.Stderr, "[DEBUG] SSH_KEY_PATH from env: %s\n", os.Getenv("SSH_KEY_PATH"))
	}

	// Substitute environment variables
	content := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./data/deploy.db"
	}
	if cfg.Git.AuthorName == "" {
		cfg.Git.AuthorName = "deploy-api"
	}
	if cfg.Git.AuthorEmail == "" {
		cfg.Git.AuthorEmail = "deploy-api@localhost"
	}
	if cfg.Kustomize.BinaryPath == "" {
		cfg.Kustomize.BinaryPath = "kustomize"  // Assume kustomize is in PATH
	}

	// Debug: print loaded config
	if os.Getenv("DEBUG") == "true" {
		fmt.Fprintf(os.Stderr, "[DEBUG] Loaded config:\n")
		fmt.Fprintf(os.Stderr, "[DEBUG]   Git.SSHKeyPath: '%s'\n", cfg.Git.SSHKeyPath)
		fmt.Fprintf(os.Stderr, "[DEBUG]   LLM.Type: '%s'\n", cfg.LLM.Type)
		fmt.Fprintf(os.Stderr, "[DEBUG]   LLM.APIKey (first 10): '%s...'\n", cfg.LLM.APIKey[:min(10, len(cfg.LLM.APIKey))])
		fmt.Fprintf(os.Stderr, "[DEBUG]   LLM.BaseURL: '%s'\n", cfg.LLM.BaseURL)
	}

	// Validate required fields
	if cfg.Server.APIKey == "" {
		// Check for API key in environment
		cfg.Server.APIKey = os.Getenv("DEPLOY_API_KEY")
	}
	
	// Default API key for local testing
	if cfg.Server.APIKey == "" {
		cfg.Server.APIKey = "test-key-local"
	}
	
	// Trim whitespace from API key
	cfg.Server.APIKey = strings.TrimSpace(cfg.Server.APIKey)

	// Root key: env fallback, optional (empty = disabled)
	if cfg.Server.RootKey == "" {
		cfg.Server.RootKey = os.Getenv("DEPLOY_ROOT_KEY")
	}
	cfg.Server.RootKey = strings.TrimSpace(cfg.Server.RootKey)

	// Reject the obvious foot-gun: ROOT_KEY identical to the CI API key would
	// silently grant /api/v1/* admin access via the /external/* ingress key.
	if cfg.Server.RootKey != "" && cfg.Server.RootKey == cfg.Server.APIKey {
		return nil, fmt.Errorf("server.root_key must not equal server.api_key")
	}

	if cfg.ArgoCDAPI.URL == "" {
		cfg.ArgoCDAPI.URL = os.Getenv("ARGOCD_API_URL")
	}
	if cfg.ArgoCDAPI.Token == "" {
		cfg.ArgoCDAPI.Token = os.Getenv("ARGOCD_API_TOKEN")
	}
	cfg.ArgoCDAPI.URL = strings.TrimRight(strings.TrimSpace(cfg.ArgoCDAPI.URL), "/")
	cfg.ArgoCDAPI.Token = strings.TrimSpace(cfg.ArgoCDAPI.Token)

	return &cfg, nil
}

// GetArgoCDRepo returns the ArgoCD repo config for a given environment
func (c *Config) GetArgoCDRepo(env string) (*RepoConfig, string, bool) {
	for id, repo := range c.Repos {
		if repo.Type == "argocd" && repo.Environment == env {
			return &repo, id, true
		}
	}
	return nil, "", false
}

// GetAppRepo returns the application repo config by its ID
func (c *Config) GetAppRepo(repoID string) (*RepoConfig, bool) {
	if repo, ok := c.Repos[repoID]; ok && repo.Type == "application" {
		return &repo, true
	}
	return nil, false
}

// FindAppRepoByAppName tries to find an application repo config for an app
func (c *Config) FindAppRepoByAppName(appName string) (*RepoConfig, string, bool) {
	// Try to find a repo that matches the app name
	repoID := fmt.Sprintf("app-%s", appName)
	if repo, ok := c.Repos[repoID]; ok && repo.Type == "application" {
		return &repo, repoID, true
	}
	
	// Look for any application repo
	for id, repo := range c.Repos {
		if repo.Type == "application" {
			return &repo, id, true
		}
	}
	
	return nil, "", false
}

// ResolveSourcePaths returns the effective source paths for changelog filtering.
// It checks the config for the app's repo first; falls back to sourcePath if non-empty.
func (c *Config) ResolveSourcePaths(repoID, sourcePath string) []string {
	if repoCfg, ok := c.GetAppRepo(repoID); ok {
		return repoCfg.ResolveSourcePaths(sourcePath)
	}
	if sourcePath != "" {
		return []string{sourcePath}
	}
	return nil
}

// GetBaseline returns the baseline environment for a given environment
// Defaults to the environment itself if not configured
func (c *Config) GetBaseline(env string) string {
	if cfg, ok := c.Environments[env]; ok {
		return cfg.Baseline
	}
	return env
}

// loadEnvFile loads environment variables from a .env file
// Simple parser: ignores comments (#), empty lines, and exports KEY=VALUE
func loadEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		// Silently ignore if file doesn't exist
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		
		// Remove optional 'export ' prefix
		line = strings.TrimPrefix(line, "export ")
		
		// Parse KEY=VALUE
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		
		// Remove quotes if present
		if (strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) ||
		   (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) {
			value = value[1 : len(value)-1]
		}
		
		// Only set if not already set in environment
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
