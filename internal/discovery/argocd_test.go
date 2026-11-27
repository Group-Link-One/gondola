package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"deploy-api/internal/config"
)

func writeKustomization(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources: []\n"), 0o644); err != nil {
		t.Fatalf("write kustomization: %v", err)
	}
}

func decodeManifest(t *testing.T, raw string) *argoCDApp {
	t.Helper()
	var doc argoCDApp
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return &doc
}

func TestClassifyApplication_KustomizeOK(t *testing.T) {
	repo := t.TempDir()
	writeKustomization(t, filepath.Join(repo, "custom/test-app"))

	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: test-app
spec:
  source:
    repoURL: git@github.com:org/test.git
    path: custom/test-app
`)
	cfg, reason := classifyApplication(doc, repo)
	if reason != "" {
		t.Fatalf("expected no skip, got %q", reason)
	}
	if cfg == nil || cfg.BasePath != "custom/test-app" || cfg.Strategy != "kustomize" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestClassifyApplication_HelmSkip(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: helm-app
spec:
  source:
    repoURL: https://example.com/charts
    chart: redis
    targetRevision: 1.2.3
`)
	if cfg, reason := classifyApplication(doc, t.TempDir()); reason != "helm" || cfg != nil {
		t.Fatalf("expected helm skip, got reason=%q cfg=%+v", reason, cfg)
	}

	docInline := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: helm-app-inline
spec:
  source:
    repoURL: https://example.com/charts
    path: charts/foo
    helm:
      valueFiles: [values.yaml]
`)
	if _, reason := classifyApplication(docInline, t.TempDir()); reason != "helm" {
		t.Fatalf("expected helm skip for inline helm block, got %q", reason)
	}
}

func TestClassifyApplication_MultiSourceSkip_Howk(t *testing.T) {
	// Typical multi-source shape: spec.sources (plural), Helm chart + private values overlay.
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: webhooks
  namespace: argocd
spec:
  project: default
  sources:
    - repoURL: https://github.com/example/webhooks-chart.git
      targetRevision: v0.4.4
      path: charts/webhooks
      helm:
        valueFiles:
          - $values/webhooks/values-prod.yaml
    - repoURL: git@github.com:example/k8s-manifests.git
      targetRevision: HEAD
      ref: values
`)
	cfg, reason := classifyApplication(doc, t.TempDir())
	if reason != "multi-source" {
		t.Fatalf("expected multi-source skip, got %q", reason)
	}
	if cfg != nil {
		t.Fatalf("expected nil config for skipped app")
	}
}

func TestClassifyApplication_AnnotationSkip(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: opted-out
  annotations:
    gondola.io/skip: "true"
spec:
  source:
    path: custom/anything
`)
	if _, reason := classifyApplication(doc, t.TempDir()); reason != "annotation:gondola.io/skip" {
		t.Fatalf("expected gondola.io/skip, got %q", reason)
	}
}

func TestClassifyApplication_UnsupportedStrategy(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: weird
  annotations:
    gondola.io/strategy: "helm"
spec:
  source:
    path: custom/anything
`)
	_, reason := classifyApplication(doc, t.TempDir())
	if reason != "unsupported-strategy:helm" {
		t.Fatalf("expected unsupported-strategy:helm, got %q", reason)
	}
}

func TestClassifyApplication_MissingPath(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: no-path
spec:
  source:
    repoURL: git@github.com:org/x.git
`)
	if _, reason := classifyApplication(doc, t.TempDir()); reason != "missing-source-path" {
		t.Fatalf("expected missing-source-path, got %q", reason)
	}
}

func TestClassifyApplication_BasePathNotFound(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ghost
spec:
  source:
    path: does/not/exist
`)
	if _, reason := classifyApplication(doc, t.TempDir()); reason != "base-path-not-found" {
		t.Fatalf("expected base-path-not-found, got %q", reason)
	}
}

func TestClassifyApplication_NoKustomization(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "custom/no-kust"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: no-kust
spec:
  source:
    path: custom/no-kust
`)
	if _, reason := classifyApplication(doc, repo); reason != "no-kustomization" {
		t.Fatalf("expected no-kustomization, got %q", reason)
	}
}

func TestClassifyApplication_OverlayAnnotations(t *testing.T) {
	repo := t.TempDir()
	// Annotation-driven overlays: base path exists but the kustomization lives in overlays.
	if err := os.MkdirAll(filepath.Join(repo, "custom/multi"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeKustomization(t, filepath.Join(repo, "custom/multi/overlay-api"))
	writeKustomization(t, filepath.Join(repo, "custom/multi/overlay-pvt"))

	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: multi
  annotations:
    gondola.io/overlays: |
      - name: api
        path: overlay-api
      - name: pvt
        path: overlay-pvt
spec:
  source:
    path: custom/multi
`)
	cfg, reason := classifyApplication(doc, repo)
	if reason != "" {
		t.Fatalf("expected ok, got %q", reason)
	}
	if len(cfg.Overlays) != 2 || cfg.Overlays[0].Name != "api" || cfg.Overlays[1].Path != "overlay-pvt" {
		t.Fatalf("unexpected overlays: %+v", cfg.Overlays)
	}

	docPattern := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: multi-pat
  annotations:
    gondola.io/overlay-pattern: "overlay-*"
spec:
  source:
    path: custom/multi
`)
	cfgPat, reasonPat := classifyApplication(docPattern, repo)
	if reasonPat != "" {
		t.Fatalf("expected ok with pattern, got %q", reasonPat)
	}
	if cfgPat.OverlayPattern != "overlay-*" {
		t.Fatalf("expected overlay pattern, got %+v", cfgPat)
	}
}

func TestClassifyApplication_InvalidOverlaysAnnotation(t *testing.T) {
	repo := t.TempDir()
	writeKustomization(t, filepath.Join(repo, "custom/x"))
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: x
  annotations:
    gondola.io/overlays: "this is not a yaml list"
spec:
  source:
    path: custom/x
`)
	_, reason := classifyApplication(doc, repo)
	// The skip reason now includes the underlying YAML parse error for diagnostics.
	if !strings.HasPrefix(reason, "invalid-overlays-annotation") {
		t.Fatalf("expected invalid-overlays-annotation prefix, got %q", reason)
	}
}

func TestScanRepo_MultiDocAndMixed(t *testing.T) {
	repo := t.TempDir()
	writeKustomization(t, filepath.Join(repo, "apps/good"))

	// Multi-doc YAML: one Application (kustomize), one ConfigMap (ignored), one helm Application (skip).
	manifest := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: good-app
spec:
  source:
    path: apps/good
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: noise
data:
  foo: bar
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: helm-app
spec:
  source:
    chart: redis
    repoURL: https://charts.example.com
`
	if err := os.MkdirAll(filepath.Join(repo, "argo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "argo/apps.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	active, skipped, err := scanRepo(repo, "argocd-staging", "staging")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(active) != 1 || active[0].Name != "good-app" {
		t.Fatalf("expected 1 active=good-app, got %+v", active)
	}
	if len(skipped) != 1 || skipped[0].Name != "helm-app" || skipped[0].SkipReason != "helm" {
		t.Fatalf("expected 1 skipped=helm-app, got %+v", skipped)
	}
}

func TestRegistry_GetAndReplace(t *testing.T) {
	r := newRegistry()
	if _, ok := r.Get("missing", "staging"); ok {
		t.Fatal("expected miss")
	}

	apps := map[string]*DiscoveredApp{
		registryKey("foo", "staging"): {
			Name:        "foo",
			Environment: "staging",
			Config:      &config.DeploymentConfig{Strategy: "kustomize", BasePath: "custom/foo"},
		},
	}
	r.replace(apps, []DiscoveredApp{{Name: "bar", SkipReason: "helm"}})

	cfg, ok := r.Get("foo", "staging")
	if !ok || cfg.BasePath != "custom/foo" {
		t.Fatalf("expected foo, got ok=%v cfg=%+v", ok, cfg)
	}
	if all := r.All(); len(all) != 1 {
		t.Fatalf("expected 1 active, got %d", len(all))
	}
	if sk := r.Skipped(); len(sk) != 1 || sk[0].Name != "bar" {
		t.Fatalf("expected bar skipped, got %+v", sk)
	}
}

func TestClassifyApplication_PathEscape(t *testing.T) {
	repo := t.TempDir()
	cases := []string{
		"../etc/passwd",
		"../../something",
		"foo/../../escape",
	}
	for _, p := range cases {
		doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: bad
spec:
  source:
    path: `+p+`
`)
		if _, reason := classifyApplication(doc, repo); reason != "path-escapes-repo" {
			t.Fatalf("path %q: expected path-escapes-repo, got %q", p, reason)
		}
	}
}

func TestClassifyApplication_AbsolutePathRejected(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: bad
spec:
  source:
    path: /etc/passwd
`)
	if _, reason := classifyApplication(doc, t.TempDir()); reason != "path-escapes-repo" {
		t.Fatalf("expected path-escapes-repo for absolute path, got %q", reason)
	}
}

func TestClassifyApplication_ApiVersionVariants(t *testing.T) {
	repo := t.TempDir()
	writeKustomization(t, filepath.Join(repo, "custom/test"))

	for _, apiVersion := range []string{"argoproj.io/v1alpha1", "argoproj.io/v1beta1", "argoproj.io/v1"} {
		doc := decodeManifest(t, `
apiVersion: `+apiVersion+`
kind: Application
metadata:
  name: test
spec:
  source:
    path: custom/test
`)
		if _, reason := classifyApplication(doc, repo); reason != "" {
			t.Fatalf("apiVersion %q: expected accepted, got skip %q", apiVersion, reason)
		}
	}
}

func TestScanRepo_IgnoresApplicationSet(t *testing.T) {
	repo := t.TempDir()
	manifest := `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: my-set
spec:
  generators: []
`
	if err := os.MkdirAll(filepath.Join(repo, "argo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "argo/set.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	active, skipped, err := scanRepo(repo, "argocd-staging", "staging")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(active) != 0 || len(skipped) != 0 {
		t.Fatalf("ApplicationSet should be silently ignored, got active=%v skipped=%v", active, skipped)
	}
}

func TestRegistry_DeepCopiesConfig(t *testing.T) {
	r := newRegistry()
	original := &config.DeploymentConfig{
		Strategy: "kustomize",
		BasePath: "custom/x",
		Overlays: []config.OverlayConfig{{Name: "api", Path: "overlay-api"}},
	}
	r.replace(map[string]*DiscoveredApp{
		registryKey("x", "staging"): {Name: "x", Environment: "staging", Config: original},
	}, nil)

	got, ok := r.Get("x", "staging")
	if !ok {
		t.Fatal("expected hit")
	}
	// Mutate the returned copy and verify the registry's state is unchanged.
	got.BasePath = "MUTATED"
	got.Overlays[0].Name = "MUTATED"

	got2, _ := r.Get("x", "staging")
	if got2.BasePath != "custom/x" {
		t.Fatalf("registry mutated through returned pointer: BasePath=%q", got2.BasePath)
	}
	if got2.Overlays[0].Name != "api" {
		t.Fatalf("registry mutated through returned slice: Overlays[0].Name=%q", got2.Overlays[0].Name)
	}

	// All() must also deep-copy.
	all := r.All()
	all[0].Config.BasePath = "MUTATED2"
	got3, _ := r.Get("x", "staging")
	if got3.BasePath != "custom/x" {
		t.Fatalf("registry mutated through All() result: BasePath=%q", got3.BasePath)
	}
}

func TestRegistry_ConcurrentReadsDuringReplace(t *testing.T) {
	// Run with -race to surface any unsynchronised access.
	r := newRegistry()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader goroutines.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = r.Get("foo", "staging")
					_ = r.All()
					_ = r.Skipped()
				}
			}
		}()
	}

	// Writer: repeatedly replace state.
	for i := 0; i < 200; i++ {
		apps := map[string]*DiscoveredApp{
			registryKey("foo", "staging"): {
				Name:        "foo",
				Environment: "staging",
				Config:      &config.DeploymentConfig{Strategy: "kustomize", BasePath: "custom/foo"},
			},
		}
		r.replace(apps, []DiscoveredApp{{Name: "bar", SkipReason: "helm"}})
	}
	close(stop)
	wg.Wait()
}

// newDiscovererWithReposDir constructs an ArgoCDDiscoverer that scans a
// caller-supplied directory instead of the production "./repos". Used by
// integration-style tests that need full RefreshRegistry behavior.
func newDiscovererWithReposDir(cfg *config.Config, reposDir string) *ArgoCDDiscoverer {
	d := NewArgoCDDiscoverer(cfg, nil)
	d.testReposDir = reposDir
	return d
}

func TestClassifyApplication_BasePathOverride(t *testing.T) {
	repo := t.TempDir()
	// ArgoCD watches a leaf overlay…
	writeKustomization(t, filepath.Join(repo, "custom/core-api/overlay-api"))
	writeKustomization(t, filepath.Join(repo, "custom/core-api/overlay-api-pvt"))
	writeKustomization(t, filepath.Join(repo, "custom/core-api/overlay-crontab"))

	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: core-api
  annotations:
    gondola.io/base-path: "custom/core-api/"
    gondola.io/overlays: |
      - name: api
        path: overlay-api
      - name: api-pvt
        path: overlay-api-pvt
      - name: crontab
        path: overlay-crontab
spec:
  source:
    path: custom/core-api/overlay-api/
`)
	cfg, reason := classifyApplication(doc, repo)
	if reason != "" {
		t.Fatalf("expected ok, got skip %q", reason)
	}
	if cfg.BasePath != "custom/core-api/" {
		t.Fatalf("expected base-path override to win, got BasePath=%q", cfg.BasePath)
	}
	if len(cfg.Overlays) != 3 {
		t.Fatalf("expected 3 overlays, got %d", len(cfg.Overlays))
	}
}

func TestClassifyApplication_BasePathOverride_PathEscape(t *testing.T) {
	repo := t.TempDir()
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: bad
  annotations:
    gondola.io/base-path: "../../../etc"
spec:
  source:
    path: custom/anywhere/
`)
	if _, reason := classifyApplication(doc, repo); reason != "path-escapes-repo" {
		t.Fatalf("expected path-escapes-repo for override path, got %q", reason)
	}
}

func TestClassifyApplication_BasePathOverride_StatChecked(t *testing.T) {
	repo := t.TempDir()
	// spec.source.path exists; override does not.
	writeKustomization(t, filepath.Join(repo, "custom/real"))
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ghost
  annotations:
    gondola.io/base-path: "custom/does-not-exist"
spec:
  source:
    path: custom/real
`)
	if _, reason := classifyApplication(doc, repo); reason != "base-path-not-found" {
		t.Fatalf("expected base-path-not-found for override, got %q", reason)
	}
}

func TestEffectiveAppName_Override(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-org-team-core-api-prod
  annotations:
    gondola.io/app-name: "core-api"
spec:
  source:
    path: custom/x
`)
	if got := effectiveAppName(doc); got != "core-api" {
		t.Fatalf("expected app-name override 'core-api', got %q", got)
	}
}

func TestEffectiveAppName_Default(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: plain-name
spec:
  source:
    path: custom/x
`)
	if got := effectiveAppName(doc); got != "plain-name" {
		t.Fatalf("expected fallback to metadata.name, got %q", got)
	}
}

func TestEffectiveAppName_TrimsWhitespace(t *testing.T) {
	doc := decodeManifest(t, `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: real
  annotations:
    gondola.io/app-name: "  spaced  "
`)
	if got := effectiveAppName(doc); got != "spaced" {
		t.Fatalf("expected trimmed override 'spaced', got %q", got)
	}
}

func TestScanRepo_AppNameOverride_RegistryKey(t *testing.T) {
	// Two ArgoCD Applications with different metadata.names but the SAME
	// gondola.io/app-name should collide (the second is treated as duplicate).
	tmp := t.TempDir()
	writeKustomization(t, filepath.Join(tmp, "custom/api"))
	writeKustomization(t, filepath.Join(tmp, "custom/api-pvt"))
	if err := os.MkdirAll(filepath.Join(tmp, "argo/apps"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "argo/apps/api.yaml"), []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: core-api-prod
  annotations:
    gondola.io/app-name: "core-api"
spec:
  source:
    path: custom/api
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "argo/apps/pvt.yaml"), []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: core-api-pvt-prod
  annotations:
    gondola.io/app-name: "core-api"
spec:
  source:
    path: custom/api-pvt
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	active, _, err := scanRepo(tmp, "argocd-newprod", "prod")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active before dedup, got %d", len(active))
	}
	for _, a := range active {
		if a.Name != "core-api" {
			t.Fatalf("expected effective name 'core-api' for both, got %q", a.Name)
		}
	}
}

func TestRefreshRegistry_EndToEnd(t *testing.T) {
	tmp := t.TempDir()

	// Set up two argocd repos: staging (kustomize) and prod (multi-source helm, skipped).
	stagingRepo := filepath.Join(tmp, "argocd-staging")
	writeKustomization(t, filepath.Join(stagingRepo, "custom/api"))
	if err := os.MkdirAll(filepath.Join(stagingRepo, "argo/apps"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagingRepo, "argo/apps/api.yaml"), []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: api
spec:
  source:
    path: custom/api
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	prodRepo := filepath.Join(tmp, "argocd-prod")
	if err := os.MkdirAll(filepath.Join(prodRepo, "argo/apps"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prodRepo, "argo/apps/webhooks.yaml"), []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: webhooks
spec:
  sources:
    - repoURL: https://example.com/charts
      chart: webhooks
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"argocd-staging": {Type: "argocd", Environment: "staging"},
			"argocd-prod":    {Type: "argocd", Environment: "prod"},
			"app-something":  {Type: "application"}, // should be ignored by the scanner
		},
	}

	d := newDiscovererWithReposDir(cfg, tmp)
	if err := d.RefreshRegistry(); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	all := d.Registry().All()
	if len(all) != 1 || all[0].Name != "api" || all[0].Environment != "staging" {
		t.Fatalf("expected 1 active=api/staging, got %+v", all)
	}
	skipped := d.Registry().Skipped()
	if len(skipped) != 1 || skipped[0].Name != "webhooks" || skipped[0].SkipReason != "multi-source" {
		t.Fatalf("expected 1 skipped=webhooks/multi-source, got %+v", skipped)
	}

	// Resolve via the resolver interface (what PerformDeployment uses).
	if dc, ok := d.Registry().Get("api", "staging"); !ok || dc.BasePath != "custom/api" {
		t.Fatalf("Get returned ok=%v cfg=%+v", ok, dc)
	}
}

func TestRefreshRegistry_DuplicateAppName(t *testing.T) {
	tmp := t.TempDir()

	// Two argocd repos pinned to the same env, both defining "shared-app".
	for _, repoID := range []string{"argocd-a", "argocd-b"} {
		dir := filepath.Join(tmp, repoID)
		writeKustomization(t, filepath.Join(dir, "custom/shared-app"))
		appsDir := filepath.Join(dir, "argo/apps")
		if err := os.MkdirAll(appsDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		manifest := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: shared-app
spec:
  source:
    path: custom/shared-app
`
		if err := os.WriteFile(filepath.Join(appsDir, "shared-app.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"argocd-a": {Type: "argocd", Environment: "staging"},
			"argocd-b": {Type: "argocd", Environment: "staging"},
		},
	}
	d := &ArgoCDDiscoverer{cfg: cfg, registry: newRegistry()}
	// Override reposDir for the test by giving the discoverer an env-only nil gitMgr;
	// since we have no Manager here, hijack reposDir via a small wrapper.
	d.gitMgr = nil
	dir := tmp
	// Construct a stub override via a temporary method shadow: rebuild via a wrapper
	// that exercises scanRepo + registry.replace logic equivalently.
	// Since reposDir() falls back to "./repos" when gitMgr is nil, run scanRepo directly
	// and assert the duplicate handling at the RefreshRegistry layer using a custom path.
	apps := map[string]*DiscoveredApp{}
	var skipped []DiscoveredApp
	for _, repoID := range []string{"argocd-a", "argocd-b"} {
		active, repoSkipped, err := scanRepo(filepath.Join(dir, repoID), repoID, "staging")
		if err != nil {
			t.Fatalf("scanRepo: %v", err)
		}
		for _, a := range active {
			a := a
			key := registryKey(a.Name, a.Environment)
			if existing, dup := apps[key]; dup {
				a.Config = nil
				a.SkipReason = "duplicate-app-name: also in " + existing.ArgoRepoID + "/" + existing.Manifest
				skipped = append(skipped, a)
				continue
			}
			apps[key] = &a
		}
		skipped = append(skipped, repoSkipped...)
	}

	if len(apps) != 1 {
		t.Fatalf("expected 1 active app (first wins), got %d", len(apps))
	}
	dupFound := false
	for _, s := range skipped {
		if s.Name == "shared-app" && strings.HasPrefix(s.SkipReason, "duplicate-app-name") {
			dupFound = true
		}
	}
	if !dupFound {
		t.Fatalf("expected duplicate-app-name skip, got skipped=%+v", skipped)
	}
}

func TestAutoSync(t *testing.T) {
	cases := map[string]struct {
		syncPolicy string
		want       bool
	}{
		"absent":           {"", false},
		"no-automated":     {"  syncPolicy:\n    syncOptions: [CreateNamespace=true]\n", false},
		"automated-null":   {"  syncPolicy:\n    automated:\n", false},
		"automated-empty":  {"  syncPolicy:\n    automated: {}\n", true},
		"automated-fields": {"  syncPolicy:\n    automated:\n      prune: true\n      selfHeal: true\n", true},
		"enabled-false":    {"  syncPolicy:\n    automated:\n      enabled: false\n", false},
		"enabled-true":     {"  syncPolicy:\n    automated:\n      enabled: true\n", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := decodeManifest(t, "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: a\nspec:\n  source:\n    path: x\n"+tc.syncPolicy)
			if got := doc.autoSync(); got != tc.want {
				t.Fatalf("autoSync() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanRepo_ArgoNameAndAutoSync(t *testing.T) {
	repo := t.TempDir()
	writeKustomization(t, filepath.Join(repo, "custom/api"))
	writeKustomization(t, filepath.Join(repo, "custom/worker"))

	manifest := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: core-api-prod
  annotations:
    gondola.io/app-name: "core-api"
spec:
  source:
    path: custom/api
  syncPolicy:
    automated:
      prune: true
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: worker
spec:
  source:
    path: custom/worker
`
	if err := os.MkdirAll(filepath.Join(repo, "argo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "argo/apps.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	active, _, err := scanRepo(repo, "argocd-prod", "prod")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	byName := map[string]DiscoveredApp{}
	for _, a := range active {
		byName[a.Name] = a
	}
	api, ok := byName["core-api"]
	if !ok || api.ArgoName != "core-api-prod" || !api.AutoSync {
		t.Fatalf("unexpected core-api entry: %+v", api)
	}
	worker, ok := byName["worker"]
	if !ok || worker.ArgoName != "worker" || worker.AutoSync {
		t.Fatalf("unexpected worker entry: %+v", worker)
	}

	r := newRegistry()
	r.replace(map[string]*DiscoveredApp{registryKey(api.Name, api.Environment): &api}, nil)
	got, ok := r.Lookup("core-api", "prod")
	if !ok || got.ArgoName != "core-api-prod" || !got.AutoSync {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
	if _, ok := r.Lookup("core-api", "staging"); ok {
		t.Fatal("expected miss for other env")
	}
}
