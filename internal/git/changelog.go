package git

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"deploy-api/internal/domain"
)

// ChangelogExtractor extracts changelogs from git repositories
type ChangelogExtractor struct {
	manager *Manager
}

// NewChangelogExtractor creates a new changelog extractor
func NewChangelogExtractor(manager *Manager) *ChangelogExtractor {
	return &ChangelogExtractor{manager: manager}
}

// ResolveCommit checks if a commit SHA exists in the repository
// Returns the resolved hash or an error if not found
func (e *ChangelogExtractor) ResolveCommit(repoID, sha string) (string, error) {
	repo, err := e.manager.OpenRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("failed to open repo %s: %w", repoID, err)
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(sha))
	if err != nil {
		return "", err
	}

	return hash.String(), nil
}

// ExtractChangelog extracts commits between two SHAs from a repository.
// sourcePaths filters commits to only those touching files matching any of the given prefixes.
// If sourcePaths is nil/empty, all commits are included.
func (e *ChangelogExtractor) ExtractChangelog(repoID, fromSHA, toSHA string, sourcePaths []string) (*domain.ChangelogResponse, error) {
	// Get the repository
	repo, err := e.manager.OpenRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to open repo %s: %w", repoID, err)
	}

	// Resolve the commit hashes
	fromHash, err := repo.ResolveRevision(plumbing.Revision(fromSHA))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve from SHA %s: %w", fromSHA, err)
	}

	toHash, err := repo.ResolveRevision(plumbing.Revision(toSHA))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve to SHA %s: %w", toSHA, err)
	}

	// Get the commits
	fromCommit, err := repo.CommitObject(*fromHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get from commit: %w", err)
	}

	toCommit, err := repo.CommitObject(*toHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get to commit: %w", err)
	}

	// Build commit list using commit iteration
	commits, err := e.getCommitsBetween(repo, fromCommit, toCommit, sourcePaths)
	if err != nil {
		return nil, fmt.Errorf("failed to get commits: %w", err)
	}

	diff, err := e.calculateDiff(repo, fromCommit, toCommit, sourcePaths)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate diff: %w", err)
	}

	riskIndicators := computeRiskIndicators(commits, diff.files, diff.envVarsAdded)

	return &domain.ChangelogResponse{
		App:            repoID,
		FromCommit:     fromHash.String(),
		ToCommit:       toHash.String(),
		Commits:        commits,
		CommitCount:    len(commits),
		DiffSummary:    diff.summary,
		RawDiff:        diff.raw,
		RiskIndicators: riskIndicators,
	}, nil
}

// getCommitsBetween gets all commits between two commits
func (e *ChangelogExtractor) getCommitsBetween(repo *git.Repository, fromCommit, toCommit *object.Commit, sourcePaths []string) ([]domain.CommitInfo, error) {
	var commits []domain.CommitInfo

	// Use log iteration to get commits
	logOpts := &git.LogOptions{
		From:  toCommit.Hash,
		Order: git.LogOrderCommitterTime,
	}

	logIter, err := repo.Log(logOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to get log: %w", err)
	}
	defer logIter.Close()

	err = logIter.ForEach(func(c *object.Commit) error {
		// Stop when we reach the from commit (don't include it)
		if c.Hash == fromCommit.Hash {
			return object.ErrCanceled // Stop iteration
		}

		// Check if this commit affects any of the source paths
		if len(sourcePaths) > 0 {
			affectsPath, err := commitAffectsAnyPath(c, sourcePaths, e)
			if err != nil {
				return err
			}
			if !affectsPath {
				return nil // Skip this commit
			}
		}

		// Get files changed in this commit
		files, err := e.getFilesChanged(c)
		if err != nil {
			return err
		}

		commitInfo := domain.CommitInfo{
			SHA:          c.Hash.String(),
			Author:       c.Author.Name,
			AuthorEmail:  c.Author.Email,
			Message:      c.Message,
			Timestamp:    c.Author.When,
			FilesChanged: files,
		}

		commits = append(commits, commitInfo)
		return nil
	})

	if err != nil && err != object.ErrCanceled {
		return nil, err
	}

	return commits, nil
}

// getFilesChanged gets the list of files changed in a commit
func (e *ChangelogExtractor) getFilesChanged(commit *object.Commit) ([]string, error) {
	var files []string

	// Get parent commit
	if commit.NumParents() == 0 {
		// Initial commit - get all files
		tree, err := commit.Tree()
		if err != nil {
			return nil, err
		}

		err = tree.Files().ForEach(func(f *object.File) error {
			files = append(files, f.Name)
			return nil
		})
		return files, err
	}

	parent, err := commit.Parent(0)
	if err != nil {
		return nil, err
	}

	// Get diff between parent and this commit
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}

	currentTree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	diff, err := parentTree.Diff(currentTree)
	if err != nil {
		return nil, err
	}

	for _, d := range diff {
		if d.To.Name != "" {
			files = append(files, d.To.Name)
		} else if d.From.Name != "" {
			files = append(files, d.From.Name)
		}
	}

	return files, nil
}

// maxRawDiffBytes bounds the patch text handed to the LLM. The prompt builder
// truncates again to fit the context window; this only keeps a huge merge from
// being materialised in memory first.
const maxRawDiffBytes = 100000

// diffResult is the net from..to change, scoped to the app's source paths.
// files and envVarsAdded cover every file, even past the raw diff cap.
type diffResult struct {
	summary      string
	raw          string
	files        []string
	envVarsAdded []string
}

func (e *ChangelogExtractor) calculateDiff(repo *git.Repository, fromCommit, toCommit *object.Commit, sourcePaths []string) (*diffResult, error) {
	fromTree, err := fromCommit.Tree()
	if err != nil {
		return nil, err
	}

	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, err
	}

	diff, err := fromTree.Diff(toTree)
	if err != nil {
		return nil, err
	}

	var totalAdditions, totalDeletions int
	var diffBuilder strings.Builder
	var files []string
	envScanner := newEnvReadScanner()

	for _, d := range diff {
		// Get the file name (handle additions, deletions, modifications)
		fileName := d.To.Name
		if fileName == "" {
			fileName = d.From.Name
		}

		// Skip if not matching any source path
		if len(sourcePaths) > 0 && !fileMatchesAnyPath(fileName, sourcePaths) {
			continue
		}

		files = append(files, fileName)

		patch, err := d.Patch()
		if err != nil {
			// A file whose patch cannot be produced still counts as changed.
			log.Printf("[Changelog] Skipping patch for %s: %v", fileName, err)
			continue
		}

		for _, st := range patch.Stats() {
			totalAdditions += st.Addition
			totalDeletions += st.Deletion
		}

		text := patch.String()
		if !testPathPattern.MatchString(fileName) {
			envScanner.scan(text)
		}
		if diffBuilder.Len() < maxRawDiffBytes {
			diffBuilder.WriteString(text)
			if !strings.HasSuffix(text, "\n") {
				diffBuilder.WriteString("\n")
			}
		}
	}

	rawDiff := diffBuilder.String()
	if len(rawDiff) > maxRawDiffBytes {
		rawDiff = rawDiff[:maxRawDiffBytes] + "\n... [diff truncated]\n"
	}

	sort.Strings(files)
	return &diffResult{
		summary: fmt.Sprintf("%d files changed, %d insertions(+), %d deletions(-)",
			len(files), totalAdditions, totalDeletions),
		raw:          rawDiff,
		files:        files,
		envVarsAdded: envScanner.vars(),
	}, nil
}

// GetCommitInfo retrieves details for a single commit by SHA
func (e *ChangelogExtractor) GetCommitInfo(repoID, sha string) (*domain.CommitInfo, error) {
	repo, err := e.manager.OpenRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to open repo %s: %w", repoID, err)
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(sha))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve SHA %s: %w", sha, err)
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	files, err := e.getFilesChanged(commit)
	if err != nil {
		files = nil // non-fatal
	}

	return &domain.CommitInfo{
		SHA:          commit.Hash.String(),
		Author:       commit.Author.Name,
		AuthorEmail:  commit.Author.Email,
		Message:      commit.Message,
		Timestamp:    commit.Author.When,
		FilesChanged: files,
	}, nil
}

// fileMatchesAnyPath returns true if fileName starts with any of the given path prefixes
func fileMatchesAnyPath(fileName string, paths []string) bool {
	for _, p := range paths {
		if strings.HasPrefix(fileName, p) {
			return true
		}
	}
	return false
}

// commitAffectsAnyPath returns true if the commit touches a file matching any of the given path prefixes
func commitAffectsAnyPath(commit *object.Commit, paths []string, e *ChangelogExtractor) (bool, error) {
	files, err := e.getFilesChanged(commit)
	if err != nil {
		return false, err
	}

	for _, file := range files {
		if fileMatchesAnyPath(file, paths) {
			return true, nil
		}
	}
	return false, nil
}

// ReadContextFiles reads the specified files from the HEAD of a repository.
// Returns a map of path → content. Files that don't exist are silently skipped.
// Each file is capped at 10KB to avoid bloating LLM prompts.
func (e *ChangelogExtractor) ReadContextFiles(repoID string, paths []string) map[string]string {
	const maxFileSize = 10 * 1024 // 10KB

	if len(paths) == 0 {
		return nil
	}

	repo, err := e.manager.OpenRepo(repoID)
	if err != nil {
		log.Printf("[ChangelogExtractor] Failed to open repo %s for context files: %v", repoID, err)
		return nil
	}

	head, err := repo.Head()
	if err != nil {
		log.Printf("[ChangelogExtractor] Failed to get HEAD for repo %s: %v", repoID, err)
		return nil
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		log.Printf("[ChangelogExtractor] Failed to get HEAD commit for repo %s: %v", repoID, err)
		return nil
	}

	tree, err := commit.Tree()
	if err != nil {
		log.Printf("[ChangelogExtractor] Failed to get tree for repo %s: %v", repoID, err)
		return nil
	}

	result := make(map[string]string)
	for _, path := range paths {
		f, err := tree.File(path)
		if err != nil {
			log.Printf("[ChangelogExtractor] Context file %s not found in repo %s: %v", path, repoID, err)
			continue
		}

		content, err := f.Contents()
		if err != nil {
			log.Printf("[ChangelogExtractor] Failed to read context file %s in repo %s: %v", path, repoID, err)
			continue
		}

		if len(content) > maxFileSize {
			content = content[:maxFileSize] + "\n... [truncated]"
		}
		result[path] = content
	}

	if len(result) == 0 {
		return nil
	}
	return result
}
