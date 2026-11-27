package git

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitFile writes a file and commits it, returning the new commit.
func commitFile(t *testing.T, repo *git.Repository, dir, name, body string) *object.Commit {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	h, err := wt.Commit("change "+name, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	c, err := repo.CommitObject(h)
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	return c
}

func TestCalculateDiff_ProducesRealHunksAndStats(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	from := commitFile(t, repo, dir, "app.go", "package app\n\nfunc A() {}\n")
	to := commitFile(t, repo, dir, "app.go", "package app\n\nfunc A() { doSomethingNew() }\nfunc B() {}\n")

	e := &ChangelogExtractor{}
	d, err := e.calculateDiff(repo, from, to, nil)
	if err != nil {
		t.Fatalf("calculateDiff: %v", err)
	}
	summary, raw := d.summary, d.raw

	if !strings.Contains(raw, "@@") {
		t.Fatalf("expected unified diff hunks, got:\n%s", raw)
	}
	if !strings.Contains(raw, "doSomethingNew") {
		t.Fatalf("expected the changed line in the diff, got:\n%s", raw)
	}
	if summary != "1 files changed, 2 insertions(+), 1 deletions(-)" {
		t.Fatalf("expected stats counted from the patch, got %q", summary)
	}
}

// A monorepo build must not be told about its siblings' code.
func TestCalculateDiff_HonoursSourcePaths(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, p := range []string{"svc-a", "svc-b"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	from := commitFile(t, repo, dir, "svc-a/main.go", "package a\n")
	commitFile(t, repo, dir, "svc-b/main.go", "package b\n")
	to := commitFile(t, repo, dir, "svc-a/main.go", "package a\n\nvar Mine = 1\n")

	e := &ChangelogExtractor{}
	d, err := e.calculateDiff(repo, from, to, []string{"svc-a"})
	if err != nil {
		t.Fatalf("calculateDiff: %v", err)
	}
	raw := d.raw
	if !reflect.DeepEqual(d.files, []string{"svc-a/main.go"}) {
		t.Fatalf("expected only svc-a files, got %v", d.files)
	}
	if !strings.Contains(raw, "Mine") {
		t.Fatalf("expected svc-a's change, got:\n%s", raw)
	}
	if strings.Contains(raw, "svc-b") {
		t.Fatalf("expected svc-b filtered out, got:\n%s", raw)
	}
}

func TestCalculateDiff_FilesAreTheNetChange(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	from := commitFile(t, repo, dir, "app.go", "package app\n")
	commitFile(t, repo, dir, "migrations/001.sql", "ALTER TABLE x ADD y int;\n")
	wt, _ := repo.Worktree()
	if _, err := wt.Remove("migrations/001.sql"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	to := commitFile(t, repo, dir, "app.go", "package app\n\nvar A = 1\n")

	d, err := (&ChangelogExtractor{}).calculateDiff(repo, from, to, nil)
	if err != nil {
		t.Fatalf("calculateDiff: %v", err)
	}
	if !reflect.DeepEqual(d.files, []string{"app.go"}) {
		t.Fatalf("a migration added then reverted must not appear, got %v", d.files)
	}
}

func TestCalculateDiff_EnvVarsPastTheRawDiffCap(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	from := commitFile(t, repo, dir, "a_big.ts", "export const a = 1;\n")
	commitFile(t, repo, dir, "a_big.ts", strings.Repeat("export const filler = 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx';\n", 4000))
	to := commitFile(t, repo, dir, "z_late.ts", "const s = process.env.NEW_SECRET;\n")

	d, err := (&ChangelogExtractor{}).calculateDiff(repo, from, to, nil)
	if err != nil {
		t.Fatalf("calculateDiff: %v", err)
	}
	if strings.Contains(d.raw, "NEW_SECRET") {
		t.Fatalf("test setup: expected z_late.ts past the raw diff cap")
	}
	if !reflect.DeepEqual(d.envVarsAdded, []string{"NEW_SECRET"}) {
		t.Fatalf("got %v, want [NEW_SECRET]", d.envVarsAdded)
	}
}
