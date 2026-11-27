package store

import (
	"encoding/json"
	"testing"
	"time"

	"deploy-api/internal/domain"
)

func TestWebhookStore_CreateAndGet(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := map[string]string{"test": "data"}
	payloadJSON, _ := json.Marshal(payload)

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payloadJSON,
		URL:         "https://example.com/webhook",
		Status:      domain.WebhookStatusPending,
		MaxAttempts: 3,
	}

	// Create
	id, err := store.Create(webhook)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if id == 0 {
		t.Error("Expected non-zero ID")
	}

	// Get
	retrieved, err := store.GetByID(id)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("Expected webhook, got nil")
	}

	if retrieved.EventType != webhook.EventType {
		t.Errorf("EventType: got %s, want %s", retrieved.EventType, webhook.EventType)
	}
	if retrieved.URL != webhook.URL {
		t.Errorf("URL: got %s, want %s", retrieved.URL, webhook.URL)
	}
	if retrieved.Status != webhook.Status {
		t.Errorf("Status: got %s, want %s", retrieved.Status, webhook.Status)
	}
}

func TestWebhookStore_ListPending(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := json.RawMessage(`{"test": "data"}`)

	// Create pending webhook with no retry scheduled
	webhook1 := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook1",
		Status:      domain.WebhookStatusPending,
		MaxAttempts: 3,
		Attempts:    0,
	}
	store.Create(webhook1)

	// Create pending webhook with past retry time
	pastTime := time.Now().Add(-time.Hour)
	webhook2 := &domain.Webhook{
		EventType:   domain.WebhookEventDeploymentFailed,
		Payload:     payload,
		URL:         "https://example.com/webhook2",
		Status:      domain.WebhookStatusFailed,
		MaxAttempts: 3,
		Attempts:    1,
		NextRetryAt: &pastTime,
	}
	store.Create(webhook2)

	// Create webhook with future retry time (should not be listed)
	futureTime := time.Now().Add(time.Hour)
	webhook3 := &domain.Webhook{
		EventType:   domain.WebhookEventDeploymentSucceeded,
		Payload:     payload,
		URL:         "https://example.com/webhook3",
		Status:      domain.WebhookStatusFailed,
		MaxAttempts: 3,
		Attempts:    1,
		NextRetryAt: &futureTime,
	}
	store.Create(webhook3)

	// Create succeeded webhook (should not be listed)
	webhook4 := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook4",
		Status:      domain.WebhookStatusSucceeded,
		MaxAttempts: 3,
	}
	store.Create(webhook4)

	// List pending
	pending, err := store.ListPending(time.Now())
	if err != nil {
		t.Fatalf("ListPending failed: %v", err)
	}

	if len(pending) != 2 {
		t.Errorf("Expected 2 pending webhooks, got %d", len(pending))
	}
}

func TestWebhookStore_MarkDelivering(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := json.RawMessage(`{"test": "data"}`)
	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook",
		Status:      domain.WebhookStatusPending,
		MaxAttempts: 3,
		Attempts:    0,
	}
	id, _ := store.Create(webhook)

	// Mark delivering
	err := store.MarkDelivering(id)
	if err != nil {
		t.Fatalf("MarkDelivering failed: %v", err)
	}

	// Verify
	retrieved, _ := store.GetByID(id)
	if retrieved.Status != domain.WebhookStatusDelivering {
		t.Errorf("Status: got %s, want %s", retrieved.Status, domain.WebhookStatusDelivering)
	}
	if retrieved.Attempts != 1 {
		t.Errorf("Attempts: got %d, want 1", retrieved.Attempts)
	}
}

func TestWebhookStore_MarkSucceeded(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := json.RawMessage(`{"test": "data"}`)
	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook",
		Status:      domain.WebhookStatusDelivering,
		MaxAttempts: 3,
		Attempts:    1,
	}
	id, _ := store.Create(webhook)

	// Mark succeeded
	err := store.MarkSucceeded(id)
	if err != nil {
		t.Fatalf("MarkSucceeded failed: %v", err)
	}

	// Verify
	retrieved, _ := store.GetByID(id)
	if retrieved.Status != domain.WebhookStatusSucceeded {
		t.Errorf("Status: got %s, want %s", retrieved.Status, domain.WebhookStatusSucceeded)
	}
	if retrieved.DeliveredAt == nil {
		t.Error("Expected DeliveredAt to be set")
	}
	if retrieved.NextRetryAt != nil {
		t.Error("Expected NextRetryAt to be nil")
	}
}

func TestWebhookStore_ScheduleRetry(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := json.RawMessage(`{"test": "data"}`)
	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook",
		Status:      domain.WebhookStatusDelivering,
		MaxAttempts: 3,
		Attempts:    1,
	}
	id, _ := store.Create(webhook)

	// Schedule retry
	nextRetry := time.Now().Add(5 * time.Minute)
	errMsg := "Connection timeout"
	err := store.ScheduleRetry(id, nextRetry, errMsg)
	if err != nil {
		t.Fatalf("ScheduleRetry failed: %v", err)
	}

	// Verify
	retrieved, _ := store.GetByID(id)
	if retrieved.Status != domain.WebhookStatusFailed {
		t.Errorf("Status: got %s, want %s", retrieved.Status, domain.WebhookStatusFailed)
	}
	if retrieved.LastError != errMsg {
		t.Errorf("LastError: got %s, want %s", retrieved.LastError, errMsg)
	}
	if retrieved.NextRetryAt == nil {
		t.Fatal("Expected NextRetryAt to be set")
	}
	if !retrieved.NextRetryAt.Equal(nextRetry) {
		t.Errorf("NextRetryAt: got %v, want %v", retrieved.NextRetryAt, nextRetry)
	}
}

func TestWebhookStore_MarkFailed(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := json.RawMessage(`{"test": "data"}`)
	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook",
		Status:      domain.WebhookStatusDelivering,
		MaxAttempts: 3,
		Attempts:    3,
	}
	id, _ := store.Create(webhook)

	// Mark failed
	errMsg := "Max retries exceeded"
	err := store.MarkFailed(id, errMsg)
	if err != nil {
		t.Fatalf("MarkFailed failed: %v", err)
	}

	// Verify
	retrieved, _ := store.GetByID(id)
	if retrieved.Status != domain.WebhookStatusFailed {
		t.Errorf("Status: got %s, want %s", retrieved.Status, domain.WebhookStatusFailed)
	}
	if retrieved.LastError != errMsg {
		t.Errorf("LastError: got %s, want %s", retrieved.LastError, errMsg)
	}
	if retrieved.NextRetryAt != nil {
		t.Error("Expected NextRetryAt to be nil after permanent failure")
	}
}

func TestWebhookStore_DeleteOld(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := json.RawMessage(`{"test": "data"}`)

	// Create old succeeded webhook
	// Note: We can't easily manipulate created_at, so we'll just verify the method works
	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payload,
		URL:         "https://example.com/webhook",
		Status:      domain.WebhookStatusSucceeded,
		MaxAttempts: 3,
	}
	store.Create(webhook)

	// Delete old webhooks (0 duration should delete all)
	deleted, err := store.DeleteOld(0)
	if err != nil {
		t.Fatalf("DeleteOld failed: %v", err)
	}

	// Should have deleted at least our webhook
	if deleted < 1 {
		t.Errorf("Expected at least 1 deletion, got %d", deleted)
	}
}

func TestWebhookStore_CreateAnalysisCompletedWebhook(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := &domain.AnalysisCompletedPayload{
		BuildID:   123,
		AppName:   "test-app",
		ImageTag:  "v1.0.0",
		CommitSHA: "abc123",
		RiskLevel: "medium",
		RiskScore: 45,
	}

	id, err := store.CreateAnalysisCompletedWebhook("https://example.com/webhook", payload, 3)
	if err != nil {
		t.Fatalf("CreateAnalysisCompletedWebhook failed: %v", err)
	}

	retrieved, _ := store.GetByID(id)
	if retrieved.EventType != domain.WebhookEventAnalysisCompleted {
		t.Errorf("EventType: got %s, want %s", retrieved.EventType, domain.WebhookEventAnalysisCompleted)
	}

	// Verify payload was stored correctly
	var storedPayload domain.AnalysisCompletedPayload
	if err := json.Unmarshal(retrieved.Payload, &storedPayload); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}
	if storedPayload.AppName != payload.AppName {
		t.Errorf("Payload AppName: got %s, want %s", storedPayload.AppName, payload.AppName)
	}
}

func TestWebhookStore_CreateDeploymentFailedWebhook(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := &domain.DeploymentFailedPayload{
		DeploymentID: 456,
		BuildID:      123,
		AppName:      "test-app",
		Environment:  "prod",
		ImageTag:     "v1.0.0",
		ErrorMessage: "Git push failed",
	}

	id, err := store.CreateDeploymentFailedWebhook("https://example.com/webhook", payload, 3)
	if err != nil {
		t.Fatalf("CreateDeploymentFailedWebhook failed: %v", err)
	}

	retrieved, _ := store.GetByID(id)
	if retrieved.EventType != domain.WebhookEventDeploymentFailed {
		t.Errorf("EventType: got %s, want %s", retrieved.EventType, domain.WebhookEventDeploymentFailed)
	}
}

func TestWebhookStore_CreateDeploymentSucceededWebhook(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewWebhookStore(db)

	payload := &domain.DeploymentSucceededPayload{
		DeploymentID: 456,
		BuildID:      123,
		AppName:      "test-app",
		Environment:  "staging",
		ImageTag:     "v1.0.0",
	}

	id, err := store.CreateDeploymentSucceededWebhook("https://example.com/webhook", payload, 3)
	if err != nil {
		t.Fatalf("CreateDeploymentSucceededWebhook failed: %v", err)
	}

	retrieved, _ := store.GetByID(id)
	if retrieved.EventType != domain.WebhookEventDeploymentSucceeded {
		t.Errorf("EventType: got %s, want %s", retrieved.EventType, domain.WebhookEventDeploymentSucceeded)
	}
}
