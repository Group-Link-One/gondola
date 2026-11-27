package git

import "testing"

// TestActiveOps_StartAndClear verifies the in-flight git-op registry that backs
// the live activity view: startOp registers an op, and the returned func clears it.
func TestActiveOps_StartAndClear(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", "Test", "test@example.com")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if ops := m.ActiveOps(); len(ops) != 0 {
		t.Fatalf("expected no ops initially, got %d", len(ops))
	}

	done1 := m.startOp("sync", "repo-a")
	done2 := m.startOp("sync", "repo-b")

	ops := m.ActiveOps()
	if len(ops) != 2 {
		t.Fatalf("expected 2 active ops, got %d", len(ops))
	}
	repos := map[string]bool{}
	for _, o := range ops {
		repos[o.RepoID] = true
		if o.Kind != "sync" {
			t.Errorf("unexpected op kind %q", o.Kind)
		}
		if o.StartedAt.IsZero() {
			t.Errorf("op for %q has zero StartedAt", o.RepoID)
		}
	}
	if !repos["repo-a"] || !repos["repo-b"] {
		t.Errorf("missing expected repos in active ops: %+v", ops)
	}

	done1()
	if ops := m.ActiveOps(); len(ops) != 1 || ops[0].RepoID != "repo-b" {
		t.Fatalf("after clearing repo-a, expected only repo-b, got %+v", ops)
	}

	done2()
	if ops := m.ActiveOps(); len(ops) != 0 {
		t.Fatalf("after clearing both, expected no ops, got %d", len(ops))
	}
}
