package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"deploy-api/internal/store"
)

const (
	// StuckAnalysisThreshold is the time after which an analysis is considered stuck
	StuckAnalysisThreshold = 5 * time.Minute
	// HealthCheckInterval is how often to check for stuck analyses
	HealthCheckInterval = 1 * time.Minute
)

// HealthChecker monitors the health of the analysis system and recovers stuck tasks
type HealthChecker struct {
	buildStore *store.BuildStore
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	running    bool
	mu         sync.Mutex
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(buildStore *store.BuildStore) *HealthChecker {
	ctx, cancel := context.WithCancel(context.Background())
	return &HealthChecker{
		buildStore: buildStore,
		ctx:        ctx,
		cancel:     cancel,
		running:    false,
	}
}

// Start begins the health check loop
func (h *HealthChecker) Start() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.running {
		return
	}

	h.running = true
	h.wg.Add(1)
	go h.loop()

	log.Println("[HealthChecker] Health checker started")
}

// Stop gracefully stops the health checker
func (h *HealthChecker) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.running {
		return
	}

	log.Println("[HealthChecker] Stopping health checker...")
	h.cancel()
	h.wg.Wait()
	h.running = false
	log.Println("[HealthChecker] Health checker stopped")
}

func (h *HealthChecker) loop() {
	defer h.wg.Done()

	// Run immediately on start
	h.check()

	ticker := time.NewTicker(HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.check()
		}
	}
}

// check performs a single health check
func (h *HealthChecker) check() {
	// Find builds stuck in "in_progress" status
	stuckBuilds, err := h.buildStore.GetStuckAnalyses(StuckAnalysisThreshold)
	if err != nil {
		log.Printf("[HealthChecker] Failed to get stuck analyses: %v", err)
		return
	}

	if len(stuckBuilds) == 0 {
		return
	}

	log.Printf("[HealthChecker] Found %d stuck analyses, recovering...", len(stuckBuilds))

	for _, build := range stuckBuilds {
		log.Printf("[HealthChecker] Recovering stuck build %d (app=%s, stuck for %v)",
			build.ID, build.AppName, time.Since(build.UpdatedAt))

		// Update status to failed
		if err := h.buildStore.UpdateAnalysisWithStatus(build.ID, "", 0, nil, "", "", "", "failed"); err != nil {
			log.Printf("[HealthChecker] Failed to recover build %d: %v", build.ID, err)
		} else {
			log.Printf("[HealthChecker] Successfully recovered build %d", build.ID)
		}
	}
}
