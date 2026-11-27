package discovery

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"deploy-api/internal/config"
	"deploy-api/internal/git"
)

// DiscoveredApp is the result of parsing one ArgoCD Application manifest.
// Skipped apps carry SkipReason populated and Config nil.
type DiscoveredApp struct {
	Name        string                   `json:"name"`
	ArgoName    string                   `json:"argo_name"`
	AutoSync    bool                     `json:"auto_sync"`
	Environment string                   `json:"environment"`
	ArgoRepoID  string                   `json:"argo_repo_id"`
	SourceURL   string                   `json:"source_url,omitempty"`
	Manifest    string                   `json:"manifest"`
	Config      *config.DeploymentConfig `json:"config,omitempty"`
	SkipReason  string                   `json:"skip_reason,omitempty"`
}

// Registry holds the in-memory discovery state, swapped atomically by RefreshRegistry.
type Registry struct {
	mu            sync.RWMutex
	apps          map[string]*DiscoveredApp
	skipped       []DiscoveredApp
	lastScannedAt time.Time // zero value means "never scanned"
}

func newRegistry() *Registry {
	return &Registry{apps: map[string]*DiscoveredApp{}}
}

// LastScannedAt returns the time of the most recent successful refresh,
// or the zero time if no scan has run yet.
func (r *Registry) LastScannedAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastScannedAt
}

func registryKey(appName, env string) string {
	return appName + "|" + env
}

// cloneDeploymentConfig returns a deep copy so callers can't mutate registry state.
func cloneDeploymentConfig(src *config.DeploymentConfig) *config.DeploymentConfig {
	if src == nil {
		return nil
	}
	dst := &config.DeploymentConfig{
		Strategy:       src.Strategy,
		BasePath:       src.BasePath,
		OverlayPattern: src.OverlayPattern,
	}
	if src.Overlays != nil {
		dst.Overlays = make([]config.OverlayConfig, len(src.Overlays))
		copy(dst.Overlays, src.Overlays)
	}
	return dst
}

// Get satisfies DeploymentConfigResolver in the deployments handler.
// Returns a deep copy so callers cannot mutate registry state.
func (r *Registry) Get(appName, env string) (*config.DeploymentConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	app, ok := r.apps[registryKey(appName, env)]
	if !ok || app.Config == nil {
		return nil, false
	}
	return cloneDeploymentConfig(app.Config), true
}

// Lookup returns a copy of the active discovered app for (appName, env).
func (r *Registry) Lookup(appName, env string) (DiscoveredApp, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	app, ok := r.apps[registryKey(appName, env)]
	if !ok {
		return DiscoveredApp{}, false
	}
	out := *app
	out.Config = cloneDeploymentConfig(app.Config)
	return out, true
}

// All returns the active discovered apps (kustomize-shaped, not skipped).
// Each returned app's Config is deep-copied to prevent mutation across atomic swaps.
func (r *Registry) All() []DiscoveredApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DiscoveredApp, 0, len(r.apps))
	for _, a := range r.apps {
		copy := *a
		copy.Config = cloneDeploymentConfig(a.Config)
		out = append(out, copy)
	}
	return out
}

// Skipped returns apps that were found but didn't satisfy the kustomize contract.
func (r *Registry) Skipped() []DiscoveredApp {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DiscoveredApp, len(r.skipped))
	copy(out, r.skipped)
	return out
}

// EnvsForApp returns the sorted list of environments that have a discovered
// (non-skipped) entry for the given app name. Used in error messages to help
// operators diagnose missing gondola.io/app-name annotations across envs.
func (r *Registry) EnvsForApp(appName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var envs []string
	for _, a := range r.apps {
		if a.Name == appName {
			envs = append(envs, a.Environment)
		}
	}
	// Stable order keeps log output deterministic.
	for i := 1; i < len(envs); i++ {
		for j := i; j > 0 && envs[j] < envs[j-1]; j-- {
			envs[j], envs[j-1] = envs[j-1], envs[j]
		}
	}
	return envs
}

func (r *Registry) replace(apps map[string]*DiscoveredApp, skipped []DiscoveredApp) {
	r.mu.Lock()
	r.apps = apps
	r.skipped = skipped
	r.lastScannedAt = time.Now()
	r.mu.Unlock()
}

// ArgoCDDiscoverer scans configured argocd-typed repos on disk for Application manifests.
type ArgoCDDiscoverer struct {
	cfg          *config.Config
	gitMgr       *git.Manager
	registry     *Registry
	testReposDir string // override for tests; production callers use git.Manager.ReposDir()
}

// NewArgoCDDiscoverer constructs a discoverer. The repos directory is taken
// from the supplied git.Manager, removing the previously hardcoded "./repos".
func NewArgoCDDiscoverer(cfg *config.Config, gitMgr *git.Manager) *ArgoCDDiscoverer {
	return &ArgoCDDiscoverer{
		cfg:      cfg,
		gitMgr:   gitMgr,
		registry: newRegistry(),
	}
}

// Registry returns the underlying registry (shared with the deployments handler).
func (d *ArgoCDDiscoverer) Registry() *Registry { return d.registry }

// reposDir returns the base directory where repos are cloned.
func (d *ArgoCDDiscoverer) reposDir() string {
	if d.testReposDir != "" {
		return d.testReposDir
	}
	if d.gitMgr != nil {
		return d.gitMgr.ReposDir()
	}
	return "./repos"
}

// pullArgoRepos git-fetches every argocd repo (via EnsureRepo, which clones if
// missing and hard-resets to origin/branch otherwise) so a subsequent scan sees
// manifests pushed since the last sync.
//
// Without this, RefreshRegistry only re-reads the on-disk working tree, which is
// refreshed only by (a) a server restart's SyncAll or (b) a deployment to that
// env. That's why a manual/periodic "discover" couldn't pick up a newly pushed
// ArgoCD Application and a full restart was the only reliable path.
//
// EnsureRepo holds the per-repo mutex, so this serializes against CommitAndPush /
// PullRebase / GetWorktree on the same repo.
func (d *ArgoCDDiscoverer) pullArgoRepos() {
	if d.gitMgr == nil {
		return // tests / discovery-only mode operate on a fixed on-disk tree
	}
	for repoID, repoCfg := range d.cfg.Repos {
		if repoCfg.Type != "argocd" {
			continue
		}
		if err := d.gitMgr.EnsureRepo(repoID, repoCfg.URL, repoCfg.Branch); err != nil {
			log.Printf("[discovery] failed to sync argocd repo %q before scan: %v (scanning existing clone)", repoID, err)
		}
	}
}

// RefreshWithSync git-fetches the argocd repos first, then rescans. This is the
// path used by the manual admin endpoint and the periodic loop so newly pushed
// manifests are discovered without a server restart. Startup uses RefreshRegistry
// directly because SyncAll has already pulled every repo by then.
func (d *ArgoCDDiscoverer) RefreshWithSync() error {
	d.pullArgoRepos()
	return d.RefreshRegistry()
}

// RefreshRegistry rescans every argocd repo and atomically replaces the registry contents.
// Duplicate (app_name, environment) pairs across repos are reported in the skipped slice
// with reason "duplicate-app-name" so operators can see the conflict in /repos/discovered.
func (d *ArgoCDDiscoverer) RefreshRegistry() error {
	apps := map[string]*DiscoveredApp{}
	var skipped []DiscoveredApp
	reposDir := d.reposDir()

	for repoID, repoCfg := range d.cfg.Repos {
		if repoCfg.Type != "argocd" {
			continue
		}
		if repoCfg.Environment == "" {
			log.Printf("[discovery] argocd repo %q has no environment set, skipping", repoID)
			continue
		}
		repoPath := filepath.Join(reposDir, repoID)
		if _, err := os.Stat(repoPath); err != nil {
			log.Printf("[discovery] argocd repo %q not cloned at %s, skipping", repoID, repoPath)
			continue
		}

		discovered, repoSkipped, err := scanRepo(repoPath, repoID, repoCfg.Environment)
		if err != nil {
			log.Printf("[discovery] scan of %q failed: %v", repoID, err)
			continue
		}
		for _, app := range discovered {
			a := app
			key := registryKey(a.Name, a.Environment)
			if existing, dup := apps[key]; dup {
				log.Printf("[discovery] duplicate app name %q in env %q: already defined in %s/%s, ignoring %s/%s",
					a.Name, a.Environment, existing.ArgoRepoID, existing.Manifest, a.ArgoRepoID, a.Manifest)
				a.Config = nil
				a.SkipReason = fmt.Sprintf("duplicate-app-name: also in %s/%s", existing.ArgoRepoID, existing.Manifest)
				skipped = append(skipped, a)
				continue
			}
			apps[key] = &a
		}
		skipped = append(skipped, repoSkipped...)
	}

	d.registry.replace(apps, skipped)
	log.Printf("[discovery] refresh complete: %d active, %d skipped", len(apps), len(skipped))
	return nil
}

// scanRepo walks the cloned argocd repo and classifies every Application manifest it finds.
func scanRepo(repoPath, repoID, env string) ([]DiscoveredApp, []DiscoveredApp, error) {
	var active, skipped []DiscoveredApp

	walkErr := filepath.WalkDir(repoPath, func(path string, dirent fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("[discovery] walk error at %s: %v (continuing)", path, err)
			return nil
		}
		if dirent.IsDir() {
			if dirent.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(dirent.Name()))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		manifestRel, _ := filepath.Rel(repoPath, path)
		f, err := os.Open(path)
		if err != nil {
			log.Printf("[discovery] cannot open %s: %v (skipping)", manifestRel, err)
			return nil
		}
		defer f.Close()

		dec := yaml.NewDecoder(f)
		for {
			var doc argoCDApp
			if err := dec.Decode(&doc); err != nil {
				if err == io.EOF {
					break
				}
				log.Printf("[discovery] malformed YAML in %s: %v (rest of file skipped)", manifestRel, err)
				return nil
			}
			if !strings.HasPrefix(doc.APIVersion, "argoproj.io/") || doc.Kind != "Application" {
				continue
			}
			if doc.Metadata.Name == "" {
				continue
			}

			app := DiscoveredApp{
				Name:        effectiveAppName(&doc),
				ArgoName:    doc.Metadata.Name,
				AutoSync:    doc.autoSync(),
				Environment: env,
				ArgoRepoID:  repoID,
				SourceURL:   doc.Spec.Source.RepoURL,
				Manifest:    manifestRel,
			}
			cfg, reason := classifyApplication(&doc, repoPath)
			if reason != "" {
				app.SkipReason = reason
				skipped = append(skipped, app)
				continue
			}
			app.Config = cfg
			active = append(active, app)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	return active, skipped, nil
}

// argoCDApp is the narrow YAML shape we decode. Unknown fields are ignored.
type argoCDApp struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Source struct {
			RepoURL string         `yaml:"repoURL"`
			Path    string         `yaml:"path"`
			Chart   string         `yaml:"chart"`
			Helm    map[string]any `yaml:"helm"`
		} `yaml:"source"`
		Sources    []map[string]any `yaml:"sources"`
		SyncPolicy struct {
			Automated *struct {
				Enabled *bool `yaml:"enabled"`
			} `yaml:"automated"`
		} `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

// autoSync reports whether spec.syncPolicy.automated is set and not explicitly disabled.
func (doc *argoCDApp) autoSync() bool {
	a := doc.Spec.SyncPolicy.Automated
	return a != nil && (a.Enabled == nil || *a.Enabled)
}

// effectiveAppName returns the registry name for a discovered Application,
// honoring the gondola.io/app-name annotation when present. This lets
// operators decouple the ArgoCD Application name from the build/image name
// (e.g., when ArgoCD names include env or org suffixes).
func effectiveAppName(doc *argoCDApp) string {
	if override := strings.TrimSpace(doc.Metadata.Annotations["gondola.io/app-name"]); override != "" {
		return override
	}
	return doc.Metadata.Name
}

// classifyApplication decides whether an Application yields a usable kustomize DeploymentConfig.
// Returns (nil, reason) when the app should be skipped.
//
// Recognized annotations (must be exact strings):
//   - gondola.io/skip: "true"          -> skip this Application
//   - gondola.io/strategy: "kustomize" -> only "kustomize" is supported
//   - gondola.io/app-name              -> override metadata.name as the registry app name
//   - gondola.io/base-path             -> override spec.source.path as Gondola's BasePath
//     (decouples ArgoCD's watch path from overlay-resolution root)
//   - gondola.io/overlay-pattern       -> glob (e.g. "overlay-*", "*")
//   - gondola.io/overlays              -> YAML list of {name, path}
func classifyApplication(doc *argoCDApp, repoPath string) (*config.DeploymentConfig, string) {
	annotations := doc.Metadata.Annotations
	if annotations["gondola.io/skip"] == "true" {
		return nil, "annotation:gondola.io/skip"
	}

	if strategy := annotations["gondola.io/strategy"]; strategy != "" && strategy != "kustomize" {
		return nil, "unsupported-strategy:" + strategy
	}

	// Multi-source apps (spec.sources, plural) — common Helm + values-overlay pattern (e.g. howk).
	if len(doc.Spec.Sources) > 0 {
		return nil, "multi-source"
	}

	// Single-source Helm.
	if doc.Spec.Source.Chart != "" || len(doc.Spec.Source.Helm) > 0 {
		return nil, "helm"
	}

	if doc.Spec.Source.Path == "" {
		return nil, "missing-source-path"
	}

	// Gondola's base_path defaults to spec.source.path. The annotation override
	// lets the operator decouple ArgoCD's watch path (a leaf overlay) from the
	// parent directory that Gondola needs for overlay resolution.
	basePath := strings.TrimSpace(annotations["gondola.io/base-path"])
	if basePath == "" {
		basePath = doc.Spec.Source.Path
	}

	// Reject absolute paths and any path that escapes repoPath after Join+Clean.
	if filepath.IsAbs(basePath) {
		return nil, "path-escapes-repo"
	}
	absBase := filepath.Join(repoPath, basePath)
	rel, err := filepath.Rel(repoPath, absBase)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "path-escapes-repo"
	}

	if _, err := os.Stat(absBase); err != nil {
		return nil, "base-path-not-found"
	}

	overlayPattern := annotations["gondola.io/overlay-pattern"]
	overlays, parseErr := parseOverlaysAnnotation(annotations["gondola.io/overlays"])
	if parseErr != nil {
		return nil, fmt.Sprintf("invalid-overlays-annotation: %v", parseErr)
	}

	// If neither annotation is set, we expect a kustomization file directly in base_path.
	if overlayPattern == "" && len(overlays) == 0 {
		if !hasKustomization(absBase) {
			return nil, "no-kustomization"
		}
	}

	return &config.DeploymentConfig{
		Strategy:       "kustomize",
		BasePath:       basePath,
		Overlays:       overlays,
		OverlayPattern: overlayPattern,
	}, ""
}

func parseOverlaysAnnotation(raw string) ([]config.OverlayConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var overlays []config.OverlayConfig
	if err := yaml.Unmarshal([]byte(raw), &overlays); err != nil {
		return nil, fmt.Errorf("parse gondola.io/overlays: %w", err)
	}
	return overlays, nil
}

func hasKustomization(dir string) bool {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
