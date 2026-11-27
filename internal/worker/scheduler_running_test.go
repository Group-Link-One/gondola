package worker

import (
	"testing"
	"time"

	"deploy-api/internal/domain"
)

// TestRunningTasks_Snapshot verifies the live snapshot reports queued vs.
// processing state. It manipulates the running map directly (white-box) so the
// test is deterministic and doesn't race the worker goroutines draining a real
// queue.
func TestRunningTasks_Snapshot(t *testing.T) {
	s := NewScheduler(SchedulerConfig{Workers: 1})
	defer s.Stop()

	queuedAt := time.Now().Add(-2 * time.Second)
	startedAt := time.Now().Add(-1 * time.Second)

	s.mu.Lock()
	s.running["build_analysis:1"] = &runningEntry{
		task:     Task{ID: "t1", Type: TaskTypeBuildAnalysis, BuildID: 1, AppName: "svc-a"},
		queuedAt: queuedAt,
	}
	s.running["auto_deploy:2"] = &runningEntry{
		task:      Task{ID: "t2", Type: TaskTypeAutoDeploy, BuildID: 2, AppName: "svc-b", Environment: domain.EnvStaging},
		queuedAt:  queuedAt,
		startedAt: &startedAt,
	}
	s.mu.Unlock()

	snap := s.RunningTasks()
	if len(snap) != 2 {
		t.Fatalf("expected 2 running tasks, got %d", len(snap))
	}

	byID := map[string]RunningTask{}
	for _, rt := range snap {
		byID[rt.ID] = rt
	}

	t1, ok := byID["t1"]
	if !ok {
		t.Fatal("missing task t1")
	}
	if t1.State != "queued" {
		t.Errorf("t1 state = %q, want queued", t1.State)
	}
	if t1.App != "svc-a" || t1.BuildID != 1 || t1.Type != "build_analysis" {
		t.Errorf("t1 fields wrong: %+v", t1)
	}
	if t1.StartedAt != nil {
		t.Errorf("t1 StartedAt should be nil for queued task")
	}

	t2, ok := byID["t2"]
	if !ok {
		t.Fatal("missing task t2")
	}
	if t2.State != "processing" {
		t.Errorf("t2 state = %q, want processing", t2.State)
	}
	if t2.Environment != "staging" || t2.StartedAt == nil {
		t.Errorf("t2 fields wrong: %+v", t2)
	}
}
