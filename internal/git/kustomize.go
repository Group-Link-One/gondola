package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"deploy-api/internal/config"
)

// KustomizeIntegration handles kustomize operations
type KustomizeIntegration struct {
	binaryPath string
}

// NewKustomizeIntegration creates a new kustomize integration
func NewKustomizeIntegration(binaryPath string) *KustomizeIntegration {
	if binaryPath == "" {
		binaryPath = "kustomize"
	}
	return &KustomizeIntegration{binaryPath: binaryPath}
}

// kustomizationDoc is a partial structural model of a kustomization.yaml,
// only enough to read existing images entries.
type kustomizationDoc struct {
	Images []kustomizationImage `yaml:"images"`
}

type kustomizationImage struct {
	Name    string `yaml:"name"`
	NewName string `yaml:"newName,omitempty"`
	NewTag  string `yaml:"newTag,omitempty"`
	Digest  string `yaml:"digest,omitempty"`
}

// findKustomizationFile returns the path to the kustomization manifest in dir,
// honoring the three names kustomize accepts. Returns empty string if none.
func findKustomizationFile(dir string) string {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// CurrentImageTag reads kustomizeDir's kustomization.yaml and returns the
// newTag currently associated with imageName. The second return value is
// true when an entry with matching `name:` is present (regardless of whether
// it has a newTag set). This is used to decide whether a "clean working tree"
// after `kustomize edit set image` is a real no-op (image already at target
// version → success) or a silent failure that should surface.
func (k *KustomizeIntegration) CurrentImageTag(kustomizeDir, imageName string) (tag string, found bool, err error) {
	path := findKustomizationFile(kustomizeDir)
	if path == "" {
		return "", false, fmt.Errorf("no kustomization file found in %s", kustomizeDir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	var doc kustomizationDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, img := range doc.Images {
		if img.Name == imageName {
			return img.NewTag, true, nil
		}
	}
	return "", false, nil
}

// SetImage runs kustomize edit set image in the specified directory
func (k *KustomizeIntegration) SetImage(kustomizeDir, imageName, newImageURI string) error {
	// Check if kustomization.yaml exists
	kustomizationPath := filepath.Join(kustomizeDir, "kustomization.yaml")
	if _, err := os.Stat(kustomizationPath); os.IsNotExist(err) {
		// Try kustomization.yml
		kustomizationPath = filepath.Join(kustomizeDir, "kustomization.yml")
		if _, err := os.Stat(kustomizationPath); os.IsNotExist(err) {
			// Try Kustomization
			kustomizationPath = filepath.Join(kustomizeDir, "Kustomization")
			if _, err := os.Stat(kustomizationPath); os.IsNotExist(err) {
				return fmt.Errorf("no kustomization file found in %s", kustomizeDir)
			}
		}
	}

	// Build the command
	// Format: kustomize edit set image name=new_image
	imageArg := fmt.Sprintf("%s=%s", imageName, newImageURI)

	cmd := exec.Command(k.binaryPath, "edit", "set", "image", imageArg)
	cmd.Dir = kustomizeDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kustomize edit set image failed: %w, output: %s", err, string(output))
	}

	return nil
}

// Build runs kustomize build to validate the configuration
func (k *KustomizeIntegration) Build(kustomizeDir string) (string, error) {
	cmd := exec.Command(k.binaryPath, "build", kustomizeDir)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kustomize build failed: %w, output: %s", err, string(output))
	}

	return string(output), nil
}

// isValidKustomizeDir checks if a directory contains a valid kustomization file
func (k *KustomizeIntegration) isValidKustomizeDir(dir string) bool {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false
	}

	// Check for kustomization files
	kustomizationFiles := []string{
		"kustomization.yaml",
		"kustomization.yml",
		"Kustomization",
	}

	for _, file := range kustomizationFiles {
		path := filepath.Join(dir, file)
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}

	return false
}

// ResolveOverlayDirs resolves kustomize overlay directories from a DeploymentConfig.
// Resolution order:
//   - If Overlays list is set: join base_path + overlay.Path for each, validate all exist
//   - Else if OverlayPattern is set: glob base_path/pattern, filter valid kustomize dirs
//     Special case: pattern "*" will also include base_path itself if it has kustomization.yaml
//   - Else: treat base_path itself as the single overlay dir
func (k *KustomizeIntegration) ResolveOverlayDirs(argoRepoPath string, deployConfig *config.DeploymentConfig) ([]string, error) {
	if deployConfig == nil {
		return nil, fmt.Errorf("deployment config is required")
	}

	if deployConfig.Strategy != "kustomize" {
		return nil, fmt.Errorf("unsupported deployment strategy: %s", deployConfig.Strategy)
	}

	basePath := filepath.Join(argoRepoPath, deployConfig.BasePath)

	if len(deployConfig.Overlays) > 0 {
		var dirs []string
		for _, overlay := range deployConfig.Overlays {
			dir := filepath.Join(basePath, overlay.Path)
			if !k.isValidKustomizeDir(dir) {
				return nil, fmt.Errorf("overlay %q not found or missing kustomization.yaml at %s", overlay.Name, dir)
			}
			dirs = append(dirs, dir)
		}
		return dirs, nil
	}

	if deployConfig.OverlayPattern != "" {
		var dirs []string

		// Special case: pattern "*" should also check base_path itself
		if deployConfig.OverlayPattern == "*" {
			if k.isValidKustomizeDir(basePath) {
				dirs = append(dirs, basePath)
			}
		}

		matches, err := filepath.Glob(filepath.Join(basePath, deployConfig.OverlayPattern))
		if err != nil {
			return nil, fmt.Errorf("invalid overlay pattern %q: %w", deployConfig.OverlayPattern, err)
		}
		for _, m := range matches {
			if k.isValidKustomizeDir(m) {
				dirs = append(dirs, m)
			}
		}
		if len(dirs) == 0 {
			return nil, fmt.Errorf("no valid kustomize directories found matching pattern %q in %s", deployConfig.OverlayPattern, basePath)
		}
		return dirs, nil
	}

	// No overlays or pattern: treat base_path as the single overlay dir
	if !k.isValidKustomizeDir(basePath) {
		return nil, fmt.Errorf("base_path %s is not a valid kustomize directory", basePath)
	}
	return []string{basePath}, nil
}

