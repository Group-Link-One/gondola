package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

func TestCommitsSince(t *testing.T) {
	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	buildStore := store.NewBuildStore(db)
	h := &DeploymentsHandler{buildStore: buildStore}

	n := 0
	create := func(sha string, prev *domain.Build, commitSHAs ...string) *domain.Build {
		t.Helper()
		n++
		var commits []domain.CommitInfo
		for _, s := range commitSHAs {
			commits = append(commits, domain.CommitInfo{SHA: s, Message: "msg " + s})
		}
		raw, _ := json.Marshal(commits)
		b := &domain.Build{
			AppName: "api", SourceRepo: "api", ImageTag: fmt.Sprintf("t%d", n), ImageURI: "r/api:" + sha,
			CommitSHA: sha, Branch: "main", AnalysisStatus: "completed", CommitsJSON: raw,
		}
		if prev != nil {
			b.PreviousBuildID = &prev.ID
		}
		id, err := buildStore.Create(b)
		if err != nil {
			t.Fatalf("create build: %v", err)
		}
		got, err := buildStore.GetByID(id)
		if err != nil {
			t.Fatalf("get build: %v", err)
		}
		return got
	}
	shas := func(cs []domain.CommitInfo) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.SHA)
		}
		return out
	}

	live := create("aaa", nil, "aaa")
	b1 := create("bbb", live, "bbb")
	rebuild := create("bbb", b1, "bbb")
	b2 := create("ccc", rebuild, "ccc", "c2")

	t.Run("walks the chain back to the live build", func(t *testing.T) {
		got := shas(h.commitsSince(b2, live))
		want := []string{"ccc", "c2", "bbb"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("stops at a rebuild of the live commit", func(t *testing.T) {
		if got := shas(h.commitsSince(b2, b1)); len(got) != 2 || got[0] != "ccc" {
			t.Fatalf("got %v, want [ccc c2]", got)
		}
	})

	t.Run("rollback yields nothing", func(t *testing.T) {
		if got := h.commitsSince(live, b2); got != nil {
			t.Fatalf("got %v, want nil", shas(got))
		}
	})

	t.Run("same commit redeploy yields nothing", func(t *testing.T) {
		if got := h.commitsSince(rebuild, b1); got != nil {
			t.Fatalf("got %v, want nil", shas(got))
		}
	})
}
