package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	
	configContent := `
server:
  port: 9090
  api_key: "test-key"

database:
  path: "./test.db"

git:
  ssh_key_path: "/test/ssh-key"
  author_name: "Test"
  author_email: "test@test.com"

kustomize:
  binary_path: "/test/kustomize"

llm:
  type: "ollama"
  base_url: "http://localhost:11434"
  model: "llama3.2:1b"

repos:
  argocd-staging:
    url: "git@github.com:org/k8s-staging.git"
    type: argocd
    environment: staging
    branch: main
    
  app-test:
    url: "git@github.com:org/test.git"
    type: application
    branch: main
`
	
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}
	
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	
	if cfg.Server.Port != 9090 {
		t.Errorf("Expected port 9090, got %d", cfg.Server.Port)
	}
	
	if cfg.Server.APIKey != "test-key" {
		t.Errorf("Expected API key 'test-key', got '%s'", cfg.Server.APIKey)
	}
	
	if cfg.Database.Path != "./test.db" {
		t.Errorf("Expected database path './test.db', got '%s'", cfg.Database.Path)
	}
	
	if cfg.Git.SSHKeyPath != "/test/ssh-key" {
		t.Errorf("Expected SSH key path '/test/ssh-key', got '%s'", cfg.Git.SSHKeyPath)
	}
	
	if cfg.Kustomize.BinaryPath != "/test/kustomize" {
		t.Errorf("Expected kustomize path '/test/kustomize', got '%s'", cfg.Kustomize.BinaryPath)
	}
	
	if cfg.LLM.Type != "ollama" {
		t.Errorf("Expected LLM type 'ollama', got '%s'", cfg.LLM.Type)
	}
	
	if len(cfg.Repos) != 2 {
		t.Errorf("Expected 2 repos, got %d", len(cfg.Repos))
	}
}

func TestLoad_Defaults(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	
	// Minimal config
	configContent := `
server:
  api_key: "test-key"
`
	
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}
	
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	
	if cfg.Server.Port != 8080 {
		t.Errorf("Expected default port 8080, got %d", cfg.Server.Port)
	}
	
	if cfg.Database.Path != "./data/deploy.db" {
		t.Errorf("Expected default database path, got '%s'", cfg.Database.Path)
	}
	
	if cfg.Git.AuthorName != "deploy-api" {
		t.Errorf("Expected default author name, got '%s'", cfg.Git.AuthorName)
	}
}

func TestLoad_EnvSubstitution(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	
	// Set environment variable
	os.Setenv("TEST_API_KEY", "env-api-key")
	defer os.Unsetenv("TEST_API_KEY")
	
	configContent := `
server:
  port: 8080
  api_key: "${TEST_API_KEY}"
`
	
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}
	
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	
	if cfg.Server.APIKey != "env-api-key" {
		t.Errorf("Expected API key from env, got '%s'", cfg.Server.APIKey)
	}
}

func TestConfig_GetArgoCDRepo(t *testing.T) {
	cfg := &Config{
		Repos: map[string]RepoConfig{
			"argocd-staging": {
				URL:         "git@github.com:org/k8s-staging.git",
				Type:        "argocd",
				Environment: "staging",
				Branch:      "main",
			},
			"argocd-prod": {
				URL:         "git@github.com:org/k8s-prod.git",
				Type:        "argocd",
				Environment: "prod",
				Branch:      "main",
			},
			"app-test": {
				URL:    "git@github.com:org/test.git",
				Type:   "application",
				Branch: "main",
			},
		},
	}
	
	repo, id, ok := cfg.GetArgoCDRepo("staging")
	if !ok {
		t.Fatal("Expected to find staging repo")
	}
	
	if id != "argocd-staging" {
		t.Errorf("Expected repo id 'argocd-staging', got '%s'", id)
	}
	
	if repo.URL != "git@github.com:org/k8s-staging.git" {
		t.Errorf("Expected staging URL, got '%s'", repo.URL)
	}
	
	// Non-existent environment
	_, _, ok = cfg.GetArgoCDRepo("dev")
	if ok {
		t.Error("Expected not to find dev repo")
	}
}

func TestConfig_GetAppRepo(t *testing.T) {
	cfg := &Config{
		Repos: map[string]RepoConfig{
			"app-core-api": {
				URL:    "git@github.com:org/core-api.git",
				Type:   "application",
				Branch: "main",
			},
		},
	}
	
	repo, ok := cfg.GetAppRepo("app-core-api")
	if !ok {
		t.Fatal("Expected to find app repo")
	}
	
	if repo.URL != "git@github.com:org/core-api.git" {
		t.Errorf("Expected app URL, got '%s'", repo.URL)
	}
	
	// Non-existent repo
	_, ok = cfg.GetAppRepo("app-nonexistent")
	if ok {
		t.Error("Expected not to find nonexistent repo")
	}
}

func TestConfig_FindAppRepoByAppName(t *testing.T) {
	cfg := &Config{
		Repos: map[string]RepoConfig{
			"app-core-api": {
				URL:    "git@github.com:org/core-api.git",
				Type:   "application",
				Branch: "main",
			},
		},
	}
	
	repo, id, ok := cfg.FindAppRepoByAppName("core-api")
	if !ok {
		t.Fatal("Expected to find app repo by name")
	}
	
	if id != "app-core-api" {
		t.Errorf("Expected id 'app-core-api', got '%s'", id)
	}
	
	if !strings.Contains(repo.URL, "core-api") {
		t.Errorf("Expected URL to contain app name, got '%s'", repo.URL)
	}
}

func TestLoad_EnvResetGate(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  port: 8080
database:
  path: "./test.db"
environments:
  staging:
    baseline: prod
    reset:
      enabled: true
      webhook_url: "https://reset.example.com/hook"
      timeout: 900
      auto_deploy: false
  prod:
    baseline: prod
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	staging := cfg.ResetConfigFor("staging")
	if staging == nil {
		t.Fatal("ResetConfigFor(staging) = nil, want the configured gate")
	}
	if staging.WebhookURL != "https://reset.example.com/hook" {
		t.Errorf("WebhookURL = %q", staging.WebhookURL)
	}
	if got := staging.ResetTimeout(); got.Seconds() != 900 {
		t.Errorf("ResetTimeout() = %v, want 900s", got)
	}
	if staging.AutoDeployAfterReset() {
		t.Error("AutoDeployAfterReset() = true, want false (explicitly disabled)")
	}

	if cfg.ResetConfigFor("prod") != nil {
		t.Error("ResetConfigFor(prod) must be nil: no reset block configured")
	}
	if cfg.ResetConfigFor("nosuchenv") != nil {
		t.Error("ResetConfigFor(unknown env) must be nil")
	}

	// Defaults when the block exists but is minimal.
	var zero *EnvResetConfig
	if got := zero.ResetTimeout(); got.Hours() != 1 {
		t.Errorf("nil ResetTimeout() = %v, want 1h", got)
	}
	if !zero.AutoDeployAfterReset() {
		t.Error("nil AutoDeployAfterReset() = false, want true (default)")
	}
}

func TestLoad_ArgoCDAPIFromEnv(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  api_key: \"k\"\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("ARGOCD_API_URL", "")
	t.Setenv("ARGOCD_API_TOKEN", "")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.ArgoCDAPI.Enabled() {
		t.Fatalf("expected argocd api disabled, got %+v", cfg.ArgoCDAPI)
	}

	t.Setenv("ARGOCD_API_URL", "http://argocd-server.argocd.svc/")
	t.Setenv("ARGOCD_API_TOKEN", " tok ")
	cfg, err = Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.ArgoCDAPI.Enabled() || cfg.ArgoCDAPI.URL != "http://argocd-server.argocd.svc" || cfg.ArgoCDAPI.Token != "tok" {
		t.Fatalf("unexpected argocd api config: %+v", cfg.ArgoCDAPI)
	}
}
