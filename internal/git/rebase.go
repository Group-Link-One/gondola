package git

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// RebaseResult represents the outcome of a rebase operation
type RebaseResult int

const (
	RebaseSuccess RebaseResult = iota
	RebaseConflict
	RebaseError
	RebaseUpToDate
)

// String returns a string representation of the rebase result
func (r RebaseResult) String() string {
	switch r {
	case RebaseSuccess:
		return "success"
	case RebaseConflict:
		return "conflict"
	case RebaseError:
		return "error"
	case RebaseUpToDate:
		return "up_to_date"
	default:
		return "unknown"
	}
}

// PullRebase performs a git pull --rebase equivalent:
// 1. Fetches from remote
// 2. Rebases current branch onto origin/<branch>
// Returns the result and any error
func (m *Manager) PullRebase(repoID, branch string) (RebaseResult, error) {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()
	return m.pullRebaseLocked(repoID, branch)
}

// pullRebaseLocked is the body of PullRebase without acquiring the repo mutex.
// Callers must already hold it (directly or via WithRepoLock/LockedRepo).
func (m *Manager) pullRebaseLocked(repoID, branch string) (RebaseResult, error) {
	repoPath := m.getRepoPath(repoID)

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return RebaseError, fmt.Errorf("failed to open repo: %w", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		return RebaseError, fmt.Errorf("failed to get worktree: %w", err)
	}

	// Get current HEAD for later comparison
	headRef, err := repo.Head()
	if err != nil {
		return RebaseError, fmt.Errorf("failed to get HEAD: %w", err)
	}
	currentHash := headRef.Hash()

	// Get remote URL to check if it's local
	remote, err := repo.Remote("origin")
	if err != nil {
		return RebaseError, fmt.Errorf("failed to get remote: %w", err)
	}

	remoteURL := ""
	if len(remote.Config().URLs) > 0 {
		remoteURL = remote.Config().URLs[0]
	}

	// Fetch from remote
	fetchOpts := &git.FetchOptions{
		RemoteName: "origin",
		Auth:       m.auth,
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch))},
	}

	// For local repos, don't use auth
	if IsLocalRepo(remoteURL) {
		fetchOpts.Auth = nil
	}

	if err := repo.Fetch(fetchOpts); err != nil && err != git.NoErrAlreadyUpToDate {
		return RebaseError, fmt.Errorf("failed to fetch: %w", err)
	}

	// Get the remote reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return RebaseError, fmt.Errorf("failed to get remote reference: %w", err)
	}

	// Check if we're already up to date
	if currentHash == remoteRef.Hash() {
		return RebaseUpToDate, nil
	}

	// Check if remote is ancestor of current (we're ahead)
	// In this case, no rebase needed
	isAncestor, err := isAncestorOf(repo, remoteRef.Hash(), currentHash)
	if err != nil {
		return RebaseError, fmt.Errorf("failed to check ancestry: %w", err)
	}
	if isAncestor {
		// We're ahead of remote, no rebase needed
		return RebaseUpToDate, nil
	}

	// Check if current is ancestor of remote (simple fast-forward)
	isFF, err := isAncestorOf(repo, currentHash, remoteRef.Hash())
	if err != nil {
		return RebaseError, fmt.Errorf("failed to check fast-forward: %w", err)
	}

	if isFF {
		// Fast-forward is safe - just reset to remote
		if err := w.Reset(&git.ResetOptions{
			Mode:   git.HardReset,
			Commit: remoteRef.Hash(),
		}); err != nil {
			return RebaseError, fmt.Errorf("failed to fast-forward: %w", err)
		}
		return RebaseSuccess, nil
	}

	// Need to rebase: reset to remote and re-apply our changes
	// Since we use git for deployment manifests with simple changes (kustomize set image),
	// we can use a simple strategy: stash our changes, reset to remote, then re-apply

	// First, check if there are uncommitted changes
	status, err := w.Status()
	if err != nil {
		return RebaseError, fmt.Errorf("failed to get status: %w", err)
	}

	hasChanges := !status.IsClean()

	// Stash our changes if any
	if hasChanges {
		// Add all changes to index
		if err := w.AddGlob("."); err != nil {
			return RebaseError, fmt.Errorf("failed to stage changes: %w", err)
		}

		// Create a temporary stash commit
		stashHash, err := w.Commit("DEPLOY_API_STASH", &git.CommitOptions{
			All: true,
		})
		if err != nil {
			return RebaseError, fmt.Errorf("failed to create stash: %w", err)
		}

		// Reset to remote
		if err := w.Reset(&git.ResetOptions{
			Mode:   git.HardReset,
			Commit: remoteRef.Hash(),
		}); err != nil {
			return RebaseError, fmt.Errorf("failed to reset for rebase: %w", err)
		}

		// Cherry-pick the stash commit
		// Note: go-git doesn't have cherry-pick, so we'll try to apply changes manually
		// For our use case (kustomize edits), we'll just try to re-apply the kustomize change
		// If there are conflicts, the caller will need to handle retry

		// Try to cherry-pick by merging the stash commit
		// Get the parent of stash (our original position)
		stashCommit, err := repo.CommitObject(stashHash)
		if err != nil {
			return RebaseError, fmt.Errorf("failed to get stash commit: %w", err)
		}

		// Get the tree from stash commit
		stashTree, err := stashCommit.Tree()
		if err != nil {
			return RebaseError, fmt.Errorf("failed to get stash tree: %w", err)
		}

		// Get current tree
		headCommit, err := repo.CommitObject(remoteRef.Hash())
		if err != nil {
			return RebaseError, fmt.Errorf("failed to get HEAD commit: %w", err)
		}
		headTree, err := headCommit.Tree()
		if err != nil {
			return RebaseError, fmt.Errorf("failed to get HEAD tree: %w", err)
		}

		// Check for conflicts by comparing trees
		// If the same file was modified in both, we have a conflict
		conflicts := findTreeConflicts(headTree, stashTree)
		if len(conflicts) > 0 {
			// Conflict detected - abort and return conflict status
			// Reset back to original to clean up
			w.Reset(&git.ResetOptions{
				Mode:   git.HardReset,
				Commit: currentHash,
			})
			return RebaseConflict, fmt.Errorf("merge conflicts in: %s", strings.Join(conflicts, ", "))
		}

		// No conflicts - apply the stash changes by checking out files from stash
		if err := w.Checkout(&git.CheckoutOptions{
			Hash:  stashHash,
			Force: false,
			Keep:  true,
		}); err != nil {
			return RebaseError, fmt.Errorf("failed to checkout stash: %w", err)
		}
	} else {
		// No local changes, just reset to remote
		if err := w.Reset(&git.ResetOptions{
			Mode:   git.HardReset,
			Commit: remoteRef.Hash(),
		}); err != nil {
			return RebaseError, fmt.Errorf("failed to reset to remote: %w", err)
		}
	}

	return RebaseSuccess, nil
}

// isAncestorOf checks if 'ancestor' is an ancestor of 'descendant' using commit history
func isAncestorOf(repo *git.Repository, ancestor, descendant plumbing.Hash) (bool, error) {
	// Simple check: walk from descendant to see if we hit ancestor
	// This is inefficient for deep histories but sufficient for deployment repos
	visited := make(map[plumbing.Hash]bool)
	toVisit := []plumbing.Hash{descendant}

	for len(toVisit) > 0 {
		// Pop
		hash := toVisit[len(toVisit)-1]
		toVisit = toVisit[:len(toVisit)-1]

		if hash == ancestor {
			return true, nil
		}

		if visited[hash] {
			continue
		}
		visited[hash] = true

		commit, err := repo.CommitObject(hash)
		if err != nil {
			// Could be a shallow clone, just skip
			continue
		}

		// Add parents
		for _, parent := range commit.ParentHashes {
			toVisit = append(toVisit, parent)
		}

		// Limit search depth to prevent excessive processing
		if len(visited) > 1000 {
			break
		}
	}

	return false, nil
}

// findTreeConflicts finds files that were modified in both trees
func findTreeConflicts(localTree, remoteTree *object.Tree) []string {
	// Get all files from both trees
	localFiles := make(map[string]bool)
	remoteFiles := make(map[string]bool)

	localTree.Files().ForEach(func(f *object.File) error {
		localFiles[f.Name] = true
		return nil
	})

	remoteTree.Files().ForEach(func(f *object.File) error {
		remoteFiles[f.Name] = true
		return nil
	})

	// Find common files (potential conflicts)
	var conflicts []string
	for file := range localFiles {
		if remoteFiles[file] {
			// Both trees have this file - potential conflict
			// In a real implementation, we'd compare file hashes
			// For now, assume any common file is a potential conflict
			conflicts = append(conflicts, file)
		}
	}

	return conflicts
}

// ResetHard performs a hard reset to the specified ref
func (m *Manager) ResetHard(repoID string, ref plumbing.Hash) error {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()

	repoPath := m.getRepoPath(repoID)

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("failed to open repo: %w", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	return w.Reset(&git.ResetOptions{
		Mode:   git.HardReset,
		Commit: ref,
	})
}

// GetRemoteHEAD gets the current HEAD hash of the remote branch
func (m *Manager) GetRemoteHEAD(repoID, branch string) (plumbing.Hash, error) {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()

	repoPath := m.getRepoPath(repoID)

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to open repo: %w", err)
	}

	// Get remote reference
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get remote reference: %w", err)
	}

	return remoteRef.Hash(), nil
}

// IsNonFastForward checks if push would be rejected as non-fast-forward
func (m *Manager) IsNonFastForward(repoID, branch string) (bool, error) {
	mutex := m.getRepoMutex(repoID)
	mutex.Lock()
	defer mutex.Unlock()

	repoPath := m.getRepoPath(repoID)

	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return false, fmt.Errorf("failed to open repo: %w", err)
	}

	// Get current local HEAD
	headRef, err := repo.Head()
	if err != nil {
		return false, fmt.Errorf("failed to get HEAD: %w", err)
	}
	localHash := headRef.Hash()

	// Get remote HEAD
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		// Remote branch might not exist yet
		return false, nil
	}
	remoteHash := remoteRef.Hash()

	// Check if remote is ancestor of local (fast-forward)
	isFF, err := isAncestorOf(repo, remoteHash, localHash)
	if err != nil {
		return false, err
	}

	// If not fast-forward, push would be rejected
	return !isFF, nil
}
