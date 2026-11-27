package git

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestWithRepoLock_Serializes verifies that two concurrent WithRepoLock calls on
// the same repoID never overlap — this is what protects a deploy's edit→commit
// sequence from a concurrent discovery pull on the same repo.
func TestWithRepoLock_Serializes(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", "Test", "test@example.com")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	var inside int32  // # goroutines currently inside the critical section
	var maxSeen int32 // high-water mark; must stay 1 if serialized
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.WithRepoLock("repo-a", func(_ *LockedRepo) error {
				n := atomic.AddInt32(&inside, 1)
				for {
					old := atomic.LoadInt32(&maxSeen)
					if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
						break
					}
				}
				// Spin briefly so overlaps would actually be observed.
				for j := 0; j < 100000; j++ {
					_ = j
				}
				atomic.AddInt32(&inside, -1)
				return nil
			})
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Fatalf("WithRepoLock allowed %d concurrent holders on the same repo; expected strict serialization", maxSeen)
	}
}

// TestWithRepoLock_DifferentReposConcurrent verifies locks are per-repo: work on
// distinct repoIDs is not needlessly serialized.
func TestWithRepoLock_DifferentReposConcurrent(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", "Test", "test@example.com")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"repo-a", "repo-b"} {
		wg.Add(1)
		go func(repoID string) {
			defer wg.Done()
			_ = m.WithRepoLock(repoID, func(_ *LockedRepo) error {
				entered <- struct{}{}
				<-release // hold the lock until both have entered
				return nil
			})
		}(id)
	}

	// Both distinct-repo locks must be holdable at once; if they were serialized
	// this would deadlock (only one entered signal, never the second).
	<-entered
	<-entered
	close(release)
	wg.Wait()
}
