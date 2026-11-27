package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"deploy-api/internal/config"
	"deploy-api/internal/git"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeAppManifest(t *testing.T, repoRoot, relPath, name, sourcePath string) {
	t.Helper()
	full := filepath.Join(repoRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := "apiVersion: argoproj.io/v1alpha1\n" +
		"kind: Application\n" +
		"metadata:\n  name: " + name + "\n" +
		"spec:\n  source:\n    path: " + sourcePath + "\n"
	if err := os.WriteFile(full, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestRefreshWithSync_PicksUpNewManifests reproduces the operator complaint:
// a manifest pushed to the argocd repo after startup was invisible to a manual
// "discover" (it re-scanned the stale on-disk clone) and only a restart picked
// it up. RefreshWithSync git-fetches first, so the new app appears without a
// restart, while a plain RefreshRegistry still sees only the old clone.
func TestRefreshWithSync_PicksUpNewManifests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()

	// "Remote" repo the operator pushes ArgoCD manifests to.
	remote := filepath.Join(tmp, "remote")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, remote, "-c", "init.defaultBranch=main", "init")
	runGit(t, remote, "config", "user.email", "test@example.com")
	runGit(t, remote, "config", "user.name", "Test")
	writeKustomization(t, filepath.Join(remote, "custom/api"))
	writeAppManifest(t, remote, "argo/apps/api.yaml", "api", "custom/api")
	runGit(t, remote, "add", ".")
	runGit(t, remote, "commit", "-m", "add api")

	clones := filepath.Join(tmp, "clones")
	gitMgr, err := git.NewManager(clones, "", "Test", "test@example.com")
	if err != nil {
		t.Fatalf("git manager: %v", err)
	}
	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"argocd-staging": {Type: "argocd", Environment: "staging", URL: remote, Branch: "main"},
		},
	}
	d := NewArgoCDDiscoverer(cfg, gitMgr)

	// First sync clones the remote and finds "api".
	if err := d.RefreshWithSync(); err != nil {
		t.Fatalf("first RefreshWithSync: %v", err)
	}
	if _, ok := d.Registry().Get("api", "staging"); !ok {
		t.Fatalf("expected api/staging after first sync")
	}

	// Operator pushes a new Application to the remote.
	writeKustomization(t, filepath.Join(remote, "custom/api2"))
	writeAppManifest(t, remote, "argo/apps/api2.yaml", "api2", "custom/api2")
	runGit(t, remote, "add", ".")
	runGit(t, remote, "commit", "-m", "add api2")

	// A scan WITHOUT syncing still sees only the old clone -> api2 absent.
	// This is exactly the stale-clone behavior that forced a restart.
	if err := d.RefreshRegistry(); err != nil {
		t.Fatalf("RefreshRegistry: %v", err)
	}
	if _, ok := d.Registry().Get("api2", "staging"); ok {
		t.Fatalf("api2 should NOT be visible before a sync (stale clone)")
	}

	// RefreshWithSync pulls first -> api2 discovered without a restart.
	if err := d.RefreshWithSync(); err != nil {
		t.Fatalf("second RefreshWithSync: %v", err)
	}
	if _, ok := d.Registry().Get("api2", "staging"); !ok {
		t.Fatalf("expected api2/staging after sync")
	}
	if _, ok := d.Registry().Get("api", "staging"); !ok {
		t.Fatalf("api/staging disappeared after sync")
	}
}
