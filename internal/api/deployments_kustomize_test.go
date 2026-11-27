package api

import (
	"errors"
	"strings"
	"testing"

	"deploy-api/internal/config"
	"deploy-api/internal/git"
)

// testGitManager returns a real *git.Manager rooted at a temp dir. performGitOperations
// now takes the per-repo lock up front (via WithRepoLock), so a nil gitManager would
// panic before any kustomize work; these tests supply a real manager and rely on the
// recordingKustomize "stop here" error (or the already-at-target short-circuit) to
// return before any git operation actually runs.
func testGitManager(t *testing.T) *git.Manager {
	t.Helper()
	m, err := git.NewManager(t.TempDir(), "", "Test", "test@example.com")
	if err != nil {
		t.Fatalf("git manager: %v", err)
	}
	return m
}

// recordingKustomize is a KustomizeInterface that records every SetImage call
// and returns an error to short-circuit before performGitOperations reaches
// the (concrete) gitManager.
type recordingKustomize struct {
	overlayDirs []string
	calls       []recordedSetImage

	// currentTag is the tag the manifest currently has for imageName. If
	// currentTagFound is false, the manifest has no entry for imageName.
	currentTag      string
	currentTagFound bool
}

type recordedSetImage struct {
	dir    string
	name   string
	newRef string
}

func (r *recordingKustomize) SetImage(kustomizeDir, imageName, newImageURI string) error {
	r.calls = append(r.calls, recordedSetImage{kustomizeDir, imageName, newImageURI})
	return errors.New("stop here")
}

func (r *recordingKustomize) ResolveOverlayDirs(_ string, _ *config.DeploymentConfig) ([]string, error) {
	return r.overlayDirs, nil
}

func (r *recordingKustomize) CurrentImageTag(_, _ string) (string, bool, error) {
	// If the test didn't configure a tag, default to a different tag so the
	// pre-check doesn't short-circuit and SetImage gets recorded by the
	// existing tests.
	if r.currentTag == "" && !r.currentTagFound {
		return "different-old-tag", true, nil
	}
	return r.currentTag, r.currentTagFound, nil
}

// TestPerformGitOperations_ImageNameMatchesManifest is a regression test for a
// case where gondola passed the bare app name (e.g. "register-http") as the
// kustomize image-name key, while the deployment manifest references the image
// by its full registry path ("registry.example.com/register-http"). Kustomize
// matches by exact name, so the override silently no-op'd and ArgoCD kept
// pulling the old tag.
func TestPerformGitOperations_ImageNameMatchesManifest(t *testing.T) {
	tests := []struct {
		name       string
		appName    string
		imageURI   string
		imageTag   string
		wantName   string
		wantNewRef string
	}{
		{
			name:       "registry-prefixed image",
			appName:    "register-http",
			imageURI:   "registry.example.com/register-http:0be478137561",
			imageTag:   "abc123",
			wantName:   "registry.example.com/register-http",
			wantNewRef: "registry.example.com/register-http:abc123",
		},
		{
			name:       "registry with port",
			appName:    "my-app",
			imageURI:   "localhost:5000/my-app:v1",
			imageTag:   "v2",
			wantName:   "localhost:5000/my-app",
			wantNewRef: "localhost:5000/my-app:v2",
		},
		{
			name:       "image without tag",
			appName:    "foo",
			imageURI:   "registry.example.com/foo",
			imageTag:   "deadbeef",
			wantName:   "registry.example.com/foo",
			wantNewRef: "registry.example.com/foo:deadbeef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rk := &recordingKustomize{overlayDirs: []string{"/tmp/overlay-a", "/tmp/overlay-b"}}
			h := &DeploymentsHandler{kustomize: rk, gitManager: testGitManager(t)}

			err := h.performGitOperations(
				"argocd-repo",
				"/tmp/argocd-repo",
				tt.appName,
				tt.imageURI,
				tt.imageTag,
				"staging",
				&config.DeploymentConfig{Strategy: "kustomize", BasePath: "."},
			)
			if err == nil || !strings.Contains(err.Error(), "stop here") {
				t.Fatalf("expected the recording error to surface, got %v", err)
			}

			if len(rk.calls) != 1 {
				t.Fatalf("expected SetImage to be called once before short-circuit, got %d calls", len(rk.calls))
			}
			got := rk.calls[0]
			if got.name != tt.wantName {
				t.Errorf("SetImage name = %q, want %q (must match the image as written in the deployment manifest, not the app name)", got.name, tt.wantName)
			}
			if got.name == tt.appName && tt.appName != tt.wantName {
				t.Errorf("SetImage name was the bare app name %q — kustomize would silently no-op against a fully-qualified image", tt.appName)
			}
			if got.newRef != tt.wantNewRef {
				t.Errorf("SetImage newRef = %q, want %q", got.newRef, tt.wantNewRef)
			}
		})
	}
}

// TestPerformGitOperations_SkipsAlreadyAtTargetTag covers the production case
// where the manifest already has the desired image+tag (e.g. a redeploy of the
// same image, or a CI script pushed manifests directly before Gondola did).
// The deploy must succeed without calling SetImage and without producing a
// commit — what was previously surfacing as "cannot create empty commit:
// clean working tree".
func TestPerformGitOperations_SkipsAlreadyAtTargetTag(t *testing.T) {
	rk := &recordingKustomize{
		overlayDirs:     []string{"/tmp/overlay-a", "/tmp/overlay-b"},
		currentTag:      "abc123", // matches the build's imageTag below
		currentTagFound: true,
	}
	// A real gitManager is supplied (needed for the up-front WithRepoLock), but
	// the already-at-target short-circuit means CommitAndPush is never reached —
	// so this test still asserts that no commit happens.
	h := &DeploymentsHandler{kustomize: rk, gitManager: testGitManager(t)}

	err := h.performGitOperations(
		"argocd-repo",
		"/tmp/argocd-repo",
		"register-http",
		"registry.example.com/register-http:0be478137561",
		"abc123",
		"prod",
		&config.DeploymentConfig{Strategy: "kustomize", BasePath: "."},
	)
	if err != nil {
		t.Fatalf("expected success when all overlays already at target tag, got %v", err)
	}
	if len(rk.calls) != 0 {
		t.Errorf("expected SetImage to NOT be called when manifest is already at target, got %d calls: %+v", len(rk.calls), rk.calls)
	}
}

// TestPerformGitOperations_NameMismatchStillFails guards against the user's
// concern: a clean working tree must NOT be silently treated as success unless
// the manifest's current tag actually equals the desired tag. If the kustomize
// image-name key isn't present in the manifest at all (or has a different tag),
// we still call SetImage and let CommitAndPush surface any real silent-failure.
func TestPerformGitOperations_NameMismatchStillFails(t *testing.T) {
	rk := &recordingKustomize{
		overlayDirs:     []string{"/tmp/overlay-a"},
		currentTagFound: false, // imageName not present in manifest
	}
	h := &DeploymentsHandler{kustomize: rk, gitManager: testGitManager(t)}

	err := h.performGitOperations(
		"argocd-repo",
		"/tmp/argocd-repo",
		"register-http",
		"registry.example.com/register-http:old",
		"new-tag",
		"prod",
		&config.DeploymentConfig{Strategy: "kustomize", BasePath: "."},
	)
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("expected SetImage to be invoked (and the recording error to surface), got %v", err)
	}
	if len(rk.calls) != 1 {
		t.Fatalf("expected SetImage to be called once when name is missing from manifest, got %d", len(rk.calls))
	}
}
