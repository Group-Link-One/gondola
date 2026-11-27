package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Manager manages git repositories
type Manager struct {
	reposDir    string
	sshKeyPath  string
	authorName  string
	authorEmail string

	// Mutex per repo for serialized operations
	repoMutexes map[string]*sync.Mutex
	mutexesMu   sync.RWMutex

	// In-flight git operations, for the live activity view. Keyed by a
	// monotonic id so multiple concurrent ops on different repos are tracked
	// independently. Guarded by its own mutex (not the per-repo ones).
	gitOps   map[int64]*gitOp
	gitOpSeq int64
	gitOpsMu sync.Mutex

	// Cached auth method
	auth transport.AuthMethod
}

// gitOp is the internal record of one in-flight git operation.
type gitOp struct {
	kind      string
	repoID    string
	startedAt time.Time
}

// GitOp is the public, JSON-serializable snapshot of one in-flight git
// operation, returned by ActiveOps for the live activity view.
type GitOp struct {
	Kind      string    `json:"kind"`
	RepoID    string    `json:"repo"`
	StartedAt time.Time `json:"started_at"`
}

// startOp registers an in-flight git operation and returns a function that
// clears it. Call the returned func (typically via defer) when the op finishes.
func (m *Manager) startOp(kind, repoID string) func() {
	m.gitOpsMu.Lock()
	m.gitOpSeq++
	id := m.gitOpSeq
	if m.gitOps == nil {
		m.gitOps = make(map[int64]*gitOp)
	}
	m.gitOps[id] = &gitOp{kind: kind, repoID: repoID, startedAt: time.Now()}
	m.gitOpsMu.Unlock()

	return func() {
		m.gitOpsMu.Lock()
		delete(m.gitOps, id)
		m.gitOpsMu.Unlock()
	}
}

// ActiveOps returns a snapshot of all currently in-flight git operations.
func (m *Manager) ActiveOps() []GitOp {
	m.gitOpsMu.Lock()
	defer m.gitOpsMu.Unlock()

	out := make([]GitOp, 0, len(m.gitOps))
	for _, op := range m.gitOps {
		out = append(out, GitOp{Kind: op.kind, RepoID: op.repoID, StartedAt: op.startedAt})
	}
	return out
}

// NewManager creates a new git manager
func NewManager(reposDir, sshKeyPath, authorName, authorEmail string) (*Manager, error) {
	// Ensure repos directory exists
	if err := os.MkdirAll(reposDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create repos directory: %w", err)
	}

	mgr := &Manager{
		reposDir:    reposDir,
		sshKeyPath:  sshKeyPath,
		authorName:  authorName,
		authorEmail: authorEmail,
		repoMutexes: make(map[string]*sync.Mutex),
	}

	// Initialize SSH auth if key provided
	if sshKeyPath != "" {
		auth, err := ssh.NewPublicKeysFromFile("git", sshKeyPath, "")
		if err != nil {
			return nil, fmt.Errorf("failed to load SSH key: %w", err)
		}
		mgr.auth = auth
	}

	return mgr, nil
}

// IsLocalRepo checks if the URL is a local path
func IsLocalRepo(url string) bool {
	return strings.HasPrefix(url, "/") ||
		strings.HasPrefix(url, "file://") ||
		strings.HasPrefix(url, ".")
}

// getRepoMutex returns the mutex for a specific repo
func (m *Manager) getRepoMutex(repoID string) *sync.Mutex {
	m.mutexesMu.Lock()
	defer m.mutexesMu.Unlock()

	if mutex, ok := m.repoMutexes[repoID]; ok {
		return mutex
	}

	mutex := &sync.Mutex{}
	m.repoMutexes[repoID] = mutex
	return mutex
}

// getRepoPath returns the filesystem path for a repo
func (m *Manager) getRepoPath(repoID string) string {
	return filepath.Join(m.reposDir, repoID)
}

// ReposDir returns the base directory where all repos are cloned.
func (m *Manager) ReposDir() string {
	return m.reposDir
}

// EnsureRepo ensures a repository is cloned and up to date
func (m *Manager) EnsureRepo(repoID, gitURL, branch string) error {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()

	// Track this pull/clone in the live activity view. Covers every git sync
	// path — startup SyncAll, discovery refresh, and build-time EnsureRepo all
	// funnel through here.
	done := m.startOp("sync", repoID)
	defer done()

	repoPath := m.getRepoPath(repoID)

	// Check if repo already exists
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		// Repo exists, pull latest changes
		return m.pull(repoPath, branch, gitURL)
	}

	// Clone the repository
	return m.clone(repoID, gitURL, branch)
}

// clone clones a repository
func (m *Manager) clone(repoID, gitURL, branch string) error {
	repoPath := m.getRepoPath(repoID)

	// Create parent directory
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		return fmt.Errorf("failed to create repo directory: %w", err)
	}

	opts := &git.CloneOptions{
		URL:           gitURL,
		Progress:      nil,
		Auth:          m.auth,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  false, // Clone all branches so commits from any branch can be found
	}

	// For local repos, don't use auth
	if IsLocalRepo(gitURL) {
		opts.Auth = nil
	}

	_, err := git.PlainClone(repoPath, false, opts)
	if err != nil {
		return fmt.Errorf("failed to clone repo: %w", err)
	}

	return nil
}

// pull pulls latest changes for a repository using fetch + reset for reliability
func (m *Manager) pull(repoPath, branch, gitURL string) error {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("failed to open repo: %w", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Fetch all branches from remote (not just the configured one)
	// This ensures commits from other branches can be found
	fetchOpts := &git.FetchOptions{
		RemoteName: "origin",
		Auth:       m.auth,
		RefSpecs:   []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}

	// For local repos, don't use auth
	if IsLocalRepo(gitURL) {
		fetchOpts.Auth = nil
	}

	if err := repo.Fetch(fetchOpts); err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("failed to fetch: %w", err)
	}

	// Get the remote reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return fmt.Errorf("failed to get remote reference: %w", err)
	}

	// Reset worktree to the remote commit (equivalent to git reset --hard origin/branch)
	if err := w.Reset(&git.ResetOptions{
		Mode:   git.HardReset,
		Commit: remoteRef.Hash(),
	}); err != nil {
		return fmt.Errorf("failed to reset to remote: %w", err)
	}

	return nil
}

// OpenRepo opens a repository for operations
func (m *Manager) OpenRepo(repoID string) (*git.Repository, error) {
	repoPath := m.getRepoPath(repoID)

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open repo %s: %w", repoID, err)
	}

	return repo, nil
}

// GetWorktree returns the worktree for a repo
func (m *Manager) GetWorktree(repoID string) (*git.Worktree, error) {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()

	repo, err := m.OpenRepo(repoID)
	if err != nil {
		return nil, err
	}

	return repo.Worktree()
}

// LockedRepo is a handle to a repo whose per-repo mutex is already held by the
// caller (via WithRepoLock). Its methods run the lock-free cores of the
// corresponding Manager methods, so a caller can perform a multi-step sequence
// (e.g. kustomize edit → commit → rebase → retry) as one atomic critical
// section without another goroutine's git op (like a discovery pull) interleaving.
//
// Do not retain a LockedRepo beyond the WithRepoLock callback — the lock is
// released when the callback returns.
type LockedRepo struct {
	m      *Manager
	repoID string
}

// WithRepoLock runs fn while holding repoID's per-repo mutex. Use it when a
// sequence of git operations on one repo must be serialized as a whole against
// other callers (deploy commit/rebase vs. discovery pull). fn must call only the
// LockedRepo methods for git operations on this repo — calling the Manager's
// public (self-locking) methods for the same repoID inside fn would deadlock.
func (m *Manager) WithRepoLock(repoID string, fn func(*LockedRepo) error) error {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()
	return fn(&LockedRepo{m: m, repoID: repoID})
}

// CommitAndPush runs the lock-free commit+push core; the caller already holds the lock.
func (l *LockedRepo) CommitAndPush(message string) error {
	return l.m.commitAndPushLocked(l.repoID, message)
}

// PullRebase runs the lock-free pull-rebase core; the caller already holds the lock.
func (l *LockedRepo) PullRebase(branch string) (RebaseResult, error) {
	return l.m.pullRebaseLocked(l.repoID, branch)
}

// CommitAndPush commits changes and pushes to remote
func (m *Manager) CommitAndPush(repoID, message string) error {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()
	return m.commitAndPushLocked(repoID, message)
}

// commitAndPushLocked is the body of CommitAndPush without acquiring the repo
// mutex. Callers must already hold it (directly or via WithRepoLock/LockedRepo).
func (m *Manager) commitAndPushLocked(repoID, message string) error {
	repo, err := m.OpenRepo(repoID)
	if err != nil {
		return err
	}

	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// Add all changes
	if err := w.AddGlob("."); err != nil {
		return fmt.Errorf("failed to add changes: %w", err)
	}

	// Commit
	commitOpts := &git.CommitOptions{
		All: true,
	}

	// Set author if configured. The When field MUST be set explicitly — go-git
	// uses the zero value (Unix epoch) when the Signature is created without
	// it, which produces commits dated 1970-01-01.
	if m.authorName != "" && m.authorEmail != "" {
		now := time.Now()
		sig := &object.Signature{
			Name:  m.authorName,
			Email: m.authorEmail,
			When:  now,
		}
		commitOpts.Author = sig
		commitOpts.Committer = sig
	}

	_, err = w.Commit(message, commitOpts)
	if err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	// Check if this repo has a remote before trying to push
	remote, err := repo.Remote("origin")
	if err != nil {
		// No remote configured - this is a local-only repo (e.g., for testing)
		// Skip the push
		return nil
	}

	// Check if this is a local repo by checking the remote URL
	if len(remote.Config().URLs) > 0 && IsLocalRepo(remote.Config().URLs[0]) {
		// For local repos, don't push (or push will fail)
		// Local repos are just for testing
		return nil
	}

	// Push
	pushOpts := &git.PushOptions{
		Auth: m.auth,
	}

	if err := repo.Push(pushOpts); err != nil {
		return fmt.Errorf("failed to push: %w", err)
	}

	return nil
}

// CreateOrUpdateTag creates or updates a lightweight tag at the given commit SHA.
// For remote repos, the tag is force-pushed. For local repos, push is skipped.
func (m *Manager) CreateOrUpdateTag(repoID, tagName, commitSHA string) error {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()

	repo, err := m.OpenRepo(repoID)
	if err != nil {
		return err
	}

	// Resolve the commit hash
	hash := plumbing.NewHash(commitSHA)

	// Delete existing tag if it exists (ignore errors)
	_ = repo.DeleteTag(tagName)

	// Create the lightweight tag
	ref := plumbing.NewReferenceFromStrings("refs/tags/"+tagName, hash.String())
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to create tag %s: %w", tagName, err)
	}

	// Check if we should push
	remote, err := repo.Remote("origin")
	if err != nil {
		// No remote - local only repo
		return nil
	}

	if len(remote.Config().URLs) > 0 && IsLocalRepo(remote.Config().URLs[0]) {
		// Local repo - skip push
		return nil
	}

	// Force push the tag
	refSpec := config.RefSpec(fmt.Sprintf("+refs/tags/%s:refs/tags/%s", tagName, tagName))
	pushOpts := &git.PushOptions{
		Auth:     m.auth,
		RefSpecs: []config.RefSpec{refSpec},
	}

	if err := repo.Push(pushOpts); err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("failed to push tag %s: %w", tagName, err)
	}

	return nil
}

// SyncAll pulls all repositories
func (m *Manager) SyncAll(repos map[string]struct{ URL, Branch string }) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(repos))

	for repoID, repoInfo := range repos {
		wg.Add(1)
		go func(id, url, branch string) {
			defer wg.Done()
			if err := m.EnsureRepo(id, url, branch); err != nil {
				errChan <- fmt.Errorf("failed to sync repo %s: %w", id, err)
			}
		}(repoID, repoInfo.URL, repoInfo.Branch)
	}

	wg.Wait()
	close(errChan)

	// Collect errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("sync errors: %v", errs)
	}

	return nil
}
