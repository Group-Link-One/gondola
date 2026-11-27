package git

import (
	"os"
	"path/filepath"
	"testing"

	"deploy-api/internal/config"
)

// createKustomizeDir creates a directory with a kustomization.yaml file for testing
func createKustomizeDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("Failed to create dir %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"), 0644); err != nil {
		t.Fatalf("Failed to write kustomization.yaml: %v", err)
	}
}

func TestResolveOverlayDirs_ExplicitOverlays(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	// Create overlay directories
	createKustomizeDir(t, filepath.Join(tmpDir, "custom/my-app/overlay-api"))
	createKustomizeDir(t, filepath.Join(tmpDir, "custom/my-app/overlay-crontab"))

	cfg := &config.DeploymentConfig{
		Strategy: "kustomize",
		BasePath: "custom/my-app",
		Overlays: []config.OverlayConfig{
			{Name: "api", Path: "overlay-api"},
			{Name: "crontab", Path: "overlay-crontab"},
		},
	}

	dirs, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("Expected 2 dirs, got %d", len(dirs))
	}
	if dirs[0] != filepath.Join(tmpDir, "custom/my-app/overlay-api") {
		t.Errorf("Expected overlay-api path, got %s", dirs[0])
	}
	if dirs[1] != filepath.Join(tmpDir, "custom/my-app/overlay-crontab") {
		t.Errorf("Expected overlay-crontab path, got %s", dirs[1])
	}
}

func TestResolveOverlayDirs_Pattern(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	// Create overlay directories matching pattern
	createKustomizeDir(t, filepath.Join(tmpDir, "apps/my-app/overlay-api"))
	createKustomizeDir(t, filepath.Join(tmpDir, "apps/my-app/overlay-worker"))
	// Create a non-matching dir (no kustomization.yaml)
	os.MkdirAll(filepath.Join(tmpDir, "apps/my-app/overlay-broken"), 0755)

	cfg := &config.DeploymentConfig{
		Strategy:       "kustomize",
		BasePath:       "apps/my-app",
		OverlayPattern: "overlay-*",
	}

	dirs, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("Expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestResolveOverlayDirs_BasePathOnly(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	createKustomizeDir(t, filepath.Join(tmpDir, "apps/simple"))

	cfg := &config.DeploymentConfig{
		Strategy: "kustomize",
		BasePath: "apps/simple",
	}

	dirs, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("Expected 1 dir, got %d", len(dirs))
	}
	if dirs[0] != filepath.Join(tmpDir, "apps/simple") {
		t.Errorf("Expected base path, got %s", dirs[0])
	}
}

func TestResolveOverlayDirs_MissingOverlay(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	// Only create one of the two overlays
	createKustomizeDir(t, filepath.Join(tmpDir, "custom/app/overlay-api"))

	cfg := &config.DeploymentConfig{
		Strategy: "kustomize",
		BasePath: "custom/app",
		Overlays: []config.OverlayConfig{
			{Name: "api", Path: "overlay-api"},
			{Name: "missing", Path: "overlay-missing"},
		},
	}

	_, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err == nil {
		t.Fatal("Expected error for missing overlay, got nil")
	}
}

func TestResolveOverlayDirs_UnsupportedStrategy(t *testing.T) {
	k := NewKustomizeIntegration("kustomize")

	cfg := &config.DeploymentConfig{
		Strategy: "terraform",
		BasePath: "some/path",
	}

	_, err := k.ResolveOverlayDirs("/tmp", cfg)
	if err == nil {
		t.Fatal("Expected error for unsupported strategy, got nil")
	}
}

func TestResolveOverlayDirs_InvalidBasePath(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	cfg := &config.DeploymentConfig{
		Strategy: "kustomize",
		BasePath: "nonexistent/path",
	}

	_, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err == nil {
		t.Fatal("Expected error for invalid base_path, got nil")
	}
}

func TestResolveOverlayDirs_PatternNoMatches(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	// Create base path but no matching overlays
	os.MkdirAll(filepath.Join(tmpDir, "apps/my-app"), 0755)

	cfg := &config.DeploymentConfig{
		Strategy:       "kustomize",
		BasePath:       "apps/my-app",
		OverlayPattern: "overlay-*",
	}

	_, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err == nil {
		t.Fatal("Expected error for no matching pattern, got nil")
	}
}

func TestResolveOverlayDirs_NilConfig(t *testing.T) {
	k := NewKustomizeIntegration("kustomize")

	_, err := k.ResolveOverlayDirs("/tmp", nil)
	if err == nil {
		t.Fatal("Expected error for nil config, got nil")
	}
	if err.Error() != "deployment config is required" {
		t.Errorf("Expected 'deployment config is required' error, got: %s", err.Error())
	}
}

func TestResolveOverlayDirs_StarPattern(t *testing.T) {
	tmpDir := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	// Create base_path with kustomization.yaml (flat staging structure)
	createKustomizeDir(t, filepath.Join(tmpDir, "custom/my-app"))
	// Also create a subdirectory with kustomization.yaml
	createKustomizeDir(t, filepath.Join(tmpDir, "custom/my-app/sub"))

	cfg := &config.DeploymentConfig{
		Strategy:       "kustomize",
		BasePath:       "custom/my-app",
		OverlayPattern: "*",
	}

	dirs, err := k.ResolveOverlayDirs(tmpDir, cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Should include base_path itself + the "sub" subdirectory
	if len(dirs) < 1 {
		t.Fatalf("Expected at least 1 dir, got %d", len(dirs))
	}
	// First entry should be the base path
	if dirs[0] != filepath.Join(tmpDir, "custom/my-app") {
		t.Errorf("Expected base path as first entry, got %s", dirs[0])
	}
}

// TestCurrentImageTag exercises the helper used by performGitOperations to
// distinguish "already at target version" (a true no-op) from "kustomize edit
// silently failed to apply" (which must surface). The two are
// indistinguishable by worktree state alone, so the deploy handler relies on
// this helper to read the manifest's actual current tag.
func TestCurrentImageTag(t *testing.T) {
	tmp := t.TempDir()
	k := NewKustomizeIntegration("kustomize")

	manifest := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
images:
  - name: registry.example.com/my-app
    newTag: v1.2.3
  - name: other-app
    newTag: v9
`
	if err := os.WriteFile(filepath.Join(tmp, "kustomization.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	tag, found, err := k.CurrentImageTag(tmp, "registry.example.com/my-app")
	if err != nil {
		t.Fatalf("CurrentImageTag: %v", err)
	}
	if !found {
		t.Errorf("expected found=true for known image")
	}
	if tag != "v1.2.3" {
		t.Errorf("tag = %q, want %q", tag, "v1.2.3")
	}

	// Bare app name (the bug variant): the image entry uses the registry-
	// qualified name, so a lookup by bare name correctly reports not-found.
	// The deploy handler interprets this as "manifest doesn't have this name",
	// then runs SetImage and lets CommitAndPush surface any silent failure.
	_, found, err = k.CurrentImageTag(tmp, "my-app")
	if err != nil {
		t.Fatalf("CurrentImageTag bare: %v", err)
	}
	if found {
		t.Errorf("expected found=false for bare app name when manifest entry is registry-qualified")
	}

	// Missing kustomization.yaml is an error, not silent miss.
	emptyDir := t.TempDir()
	if _, _, err := k.CurrentImageTag(emptyDir, "anything"); err == nil {
		t.Errorf("expected error when kustomization file is absent")
	}
}
