package testutil

import (
	"fmt"
	"path/filepath"
	"sync"

	"deploy-api/internal/domain"
	"deploy-api/internal/git"
)

// MockGitManager is a mock implementation of git operations for testing
type MockGitManager struct {
	mu sync.Mutex

	// Repositories map
	Repos map[string]*MockRepo

	// Operations tracking
	Operations []GitOperation

	// Clone errors to simulate
	CloneErrors map[string]error

	// Pull errors to simulate
	PullErrors map[string]error
}

// MockRepo represents a mock repository
type MockRepo struct {
	ID       string
	GitURL   string
	Path     string
	Branch   string
	Commits  map[string]*MockCommit
	Cloned   bool
	PulledAt int64
}

// MockCommit represents a mock commit
type MockCommit struct {
	SHA     string
	Message string
	Author  string
	Email   string
	Parent  string
	Files   []string
}

// GitOperation tracks a git operation
type GitOperation struct {
	Type   string
	RepoID string
	Error  error
}

// NewMockGitManager creates a new mock git manager
func NewMockGitManager() *MockGitManager {
	return &MockGitManager{
		Repos:       make(map[string]*MockRepo),
		Operations:  make([]GitOperation, 0),
		CloneErrors: make(map[string]error),
		PullErrors:  make(map[string]error),
	}
}

// CloneRepo mocks cloning a repository
func (m *MockGitManager) CloneRepo(repoID, gitURL, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Operations = append(m.Operations, GitOperation{
		Type:   "clone",
		RepoID: repoID,
	})

	// Check for simulated error
	if err, ok := m.CloneErrors[repoID]; ok {
		return err
	}

	// Create mock repo
	repo := &MockRepo{
		ID:     repoID,
		GitURL: gitURL,
		Path:   filepath.Join("/tmp/mock-repos", repoID),
		Branch: branch,
		Commits: map[string]*MockCommit{
			"abc123": {
				SHA:     "abc123",
				Message: "Initial commit",
				Author:  "Test User",
				Email:   "test@example.com",
			},
			"def456": {
				SHA:     "def456",
				Message: "Add feature",
				Author:  "Developer",
				Email:   "dev@example.com",
				Parent:  "abc123",
			},
		},
		Cloned: true,
	}

	m.Repos[repoID] = repo
	return nil
}

// PullRepo mocks pulling a repository
func (m *MockGitManager) PullRepo(repoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Operations = append(m.Operations, GitOperation{
		Type:   "pull",
		RepoID: repoID,
	})

	if err, ok := m.PullErrors[repoID]; ok {
		return err
	}

	repo, ok := m.Repos[repoID]
	if !ok {
		return fmt.Errorf("repository %s not found", repoID)
	}

	repo.PulledAt = 1234567890
	return nil
}

// GetRepoPath returns the path to a repository
func (m *MockGitManager) GetRepoPath(repoID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if repo, ok := m.Repos[repoID]; ok {
		return repo.Path
	}
	return ""
}

// EnsureRepo ensures a repo exists (clone if needed, pull if exists)
func (m *MockGitManager) EnsureRepo(repoID, gitURL, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if repo, ok := m.Repos[repoID]; ok && repo.Cloned {
		return m.PullRepo(repoID)
	}
	return m.CloneRepo(repoID, gitURL, branch)
}

// AddMockCommit adds a commit to a mock repo
func (m *MockGitManager) AddMockCommit(repoID string, commit *MockCommit) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if repo, ok := m.Repos[repoID]; ok {
		repo.Commits[commit.SHA] = commit
	}
}

// SetCloneError sets an error to return when cloning
func (m *MockGitManager) SetCloneError(repoID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CloneErrors[repoID] = err
}

// GetOperations returns recorded operations
func (m *MockGitManager) GetOperations() []GitOperation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]GitOperation{}, m.Operations...)
}

// Reset clears all state
func (m *MockGitManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Repos = make(map[string]*MockRepo)
	m.Operations = make([]GitOperation, 0)
	m.CloneErrors = make(map[string]error)
	m.PullErrors = make(map[string]error)
}

// MockChangelogExtractor is a mock changelog extractor
type MockChangelogExtractor struct {
	*git.ChangelogExtractor
	mu sync.Mutex

	// Predefined changelogs
	Changelogs map[string]*domain.ChangelogResponse

	// Default changelog
	DefaultChangelog *domain.ChangelogResponse

	// Call tracking
	Extractions []ExtractionCall
}

// ExtractionCall tracks an extraction call
type ExtractionCall struct {
	RepoID      string
	FromSHA     string
	ToSHA       string
	SourcePaths []string
}

// NewMockChangelogExtractor creates a mock extractor
func NewMockChangelogExtractor() *MockChangelogExtractor {
	return &MockChangelogExtractor{
		Changelogs: make(map[string]*domain.ChangelogResponse),
		DefaultChangelog: &domain.ChangelogResponse{
			App:         "test-app",
			FromCommit:  "abc123",
			ToCommit:    "def456",
			CommitCount: 2,
			Commits: []domain.CommitInfo{
				{
					SHA:          "def456",
					Message:      "Latest commit",
					Author:       "Developer",
					AuthorEmail:  "dev@example.com",
				},
				{
					SHA:          "abc123",
					Message:      "Previous commit",
					Author:       "Developer",
					AuthorEmail:  "dev@example.com",
				},
			},
		},
		Extractions: make([]ExtractionCall, 0),
	}
}

// ExtractChangelog mocks extracting a changelog
func (m *MockChangelogExtractor) ExtractChangelog(repoID, fromSHA, toSHA string, sourcePaths []string) (*domain.ChangelogResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Extractions = append(m.Extractions, ExtractionCall{
		RepoID:      repoID,
		FromSHA:     fromSHA,
		ToSHA:       toSHA,
		SourcePaths: sourcePaths,
	})

	// Look up by key
	key := fmt.Sprintf("%s:%s:%s", repoID, fromSHA, toSHA)
	if changelog, ok := m.Changelogs[key]; ok {
		return changelog, nil
	}

	return m.DefaultChangelog, nil
}

// ReadContextFiles mocks reading context files from a repo
func (m *MockChangelogExtractor) ReadContextFiles(repoID string, paths []string) map[string]string {
	return nil
}

// SetChangelog sets a changelog for a specific range
func (m *MockChangelogExtractor) SetChangelog(repoID, fromSHA, toSHA string, changelog *domain.ChangelogResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%s", repoID, fromSHA, toSHA)
	m.Changelogs[key] = changelog
}

// GetExtractions returns recorded extraction calls
func (m *MockChangelogExtractor) GetExtractions() []ExtractionCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ExtractionCall{}, m.Extractions...)
}

// Reset clears all state
func (m *MockChangelogExtractor) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Changelogs = make(map[string]*domain.ChangelogResponse)
	m.Extractions = make([]ExtractionCall, 0)
}
