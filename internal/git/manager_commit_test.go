package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

// TestCommitAndPush_AuthorWhenIsCurrent guards against a regression where
// CommitAndPush created commits dated 1970-01-01 because go-git's
// object.Signature has a zero-value When field unless set explicitly.
func TestCommitAndPush_AuthorWhenIsCurrent(t *testing.T) {
	tmp := t.TempDir()
	reposDir := filepath.Join(tmp, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatalf("mkdir reposDir: %v", err)
	}

	// Initialise a real local git repo to act as the remote (required so
	// CommitAndPush doesn't bail before reading the worktree).
	repoDir := filepath.Join(reposDir, "test-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repoDir: %v", err)
	}
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// Seed an initial commit so the worktree has a HEAD.
	if err := os.WriteFile(filepath.Join(repoDir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("seed.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := wt.Commit("seed", &git.CommitOptions{}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	mgr, err := NewManager(reposDir, "", "deploy-api", "deploy-api@test.com")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Drop a new file then commit.
	if err := os.WriteFile(filepath.Join(repoDir, "kustomization.yaml"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write change: %v", err)
	}
	before := time.Now().Add(-2 * time.Second)
	if err := mgr.CommitAndPush("test-repo", "Deploy x:v1"); err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	after := time.Now().Add(2 * time.Second)

	// Inspect HEAD's author and committer dates via the git CLI (cheap, robust).
	out, err := exec.Command("git", "-C", repoDir, "log", "-1", "--format=%aI%n%cI").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	lines := splitLines(string(out))
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines (author, committer), got %q", out)
	}
	authored, err := time.Parse(time.RFC3339, lines[0])
	if err != nil {
		t.Fatalf("parse author date %q: %v", lines[0], err)
	}
	committed, err := time.Parse(time.RFC3339, lines[1])
	if err != nil {
		t.Fatalf("parse committer date %q: %v", lines[1], err)
	}
	if authored.Before(before) || authored.After(after) {
		t.Errorf("author date %v not within [%v, %v] (Unix epoch regression?)", authored, before, after)
	}
	if committed.Before(before) || committed.After(after) {
		t.Errorf("committer date %v not within [%v, %v]", committed, before, after)
	}
	if authored.Year() < 2024 {
		t.Errorf("author year %d looks like the Unix-epoch bug returned", authored.Year())
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
