package api

import (
	"errors"
	"fmt"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// TestIsNonFastForwardError pins the strict matching that the retry/rebase
// loop in performGitOperations relies on. The earlier overly-broad list of
// substrings caused real push failures (notably "deploy key has no write
// access" on GitLab) to be classified as retryable races: the loop would
// rebase (a no-op, since no race had happened), observe the in-tree edit
// from the prior attempt on the next iteration, and report success without
// ever pushing.
func TestIsNonFastForwardError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sentinel directly", gogit.ErrNonFastForwardUpdate, true},
		{"sentinel wrapped via %w", fmt.Errorf("failed to push: %w", gogit.ErrNonFastForwardUpdate), true},
		{"formatted non-fast-forward update string", errors.New("non-fast-forward update: refs/heads/master"), true},
		{"wrapped formatted non-fast-forward", fmt.Errorf("failed to push: %w", errors.New("non-fast-forward update: refs/heads/master")), true},

		// Real-world misclassifications that prompted this tightening.
		{"deploy key no write access", errors.New("failed to push: remote: ERROR: This deploy key does not have write access to this project."), false},
		{"pre-receive hook declined", errors.New("failed to push: command error on refs/heads/master: pre-receive hook declined"), false},
		{"network blip", errors.New("failed to push: dial tcp: i/o timeout"), false},
		{"ssh handshake", errors.New("failed to push: ssh: handshake failed: ssh: unable to authenticate"), false},
		// Generic wording that the old heuristic matched on.
		{"plain rejected", errors.New("rejected"), false},
		{"plain failed to push wrapper", errors.New("failed to push: something else"), false},
		{"atomic in unrelated message", errors.New("atomic operation interrupted"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNonFastForwardError(tc.err); got != tc.want {
				t.Fatalf("isNonFastForwardError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
