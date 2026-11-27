package notifier

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

func setupTestDB(t *testing.T) *store.DB {
	db, err := store.NewDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}
	return db
}

func TestWebhookNotifier_NotifyAnalysisCompleted(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	webhookStore := store.NewWebhookStore(db)

	// Create test server
	var receivedPayload []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPayload, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := DefaultWebhookNotifierConfig()
	config.WebhookURLs = []string{server.URL}
	config.PollInterval = 100 * time.Millisecond

	notifier := NewWebhookNotifier(webhookStore, config)
	notifier.Start()
	defer notifier.Stop()

	payload := &domain.AnalysisCompletedPayload{
		BuildID:     123,
		AppName:     "test-app",
		ImageTag:    "v1.0.0",
		CommitSHA:   "abc123def456",
		RiskLevel:   "high",
		RiskScore:   75,
		Summary:     "Breaking changes detected",
		Concerns:    []string{"Database migration", "API change"},
		NeedsReview: true,
	}

	// Queue webhook
	id, err := notifier.NotifyAnalysisCompleted(server.URL, payload)
	if err != nil {
		t.Fatalf("NotifyAnalysisCompleted failed: %v", err)
	}
	if id == 0 {
		t.Error("Expected non-zero webhook ID")
	}

	// Wait for delivery
	time.Sleep(300 * time.Millisecond)

	// Verify payload was received
	if receivedPayload == nil {
		t.Fatal("Server did not receive payload")
	}

	var received domain.AnalysisCompletedPayload
	if err := json.Unmarshal(receivedPayload, &received); err != nil {
		t.Fatalf("Failed to unmarshal received payload: %v", err)
	}

	if received.AppName != payload.AppName {
		t.Errorf("AppName: got %s, want %s", received.AppName, payload.AppName)
	}
	if received.RiskLevel != payload.RiskLevel {
		t.Errorf("RiskLevel: got %s, want %s", received.RiskLevel, payload.RiskLevel)
	}
}

func TestWebhookNotifier_RetryOnFailure(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	webhookStore := store.NewWebhookStore(db)

	// Create test server that fails first 2 requests
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := DefaultWebhookNotifierConfig()
	config.WebhookURLs = []string{server.URL}
	config.PollInterval = 100 * time.Millisecond
	config.RetryBackoff = 50 * time.Millisecond

	notifier := NewWebhookNotifier(webhookStore, config)
	notifier.Start()
	defer notifier.Stop()

	payload := &domain.DeploymentFailedPayload{
		DeploymentID: 456,
		BuildID:      123,
		AppName:      "test-app",
		Environment:  "prod",
		ErrorMessage: "Git push failed",
	}

	// Queue webhook
	_, err := notifier.NotifyDeploymentFailed(server.URL, payload)
	if err != nil {
		t.Fatalf("NotifyDeploymentFailed failed: %v", err)
	}

	// Wait for retries and eventual success
	time.Sleep(500 * time.Millisecond)

	// Should have received 3 requests (2 failures + 1 success)
	if requestCount != 3 {
		t.Errorf("Expected 3 requests, got %d", requestCount)
	}
}

func TestWebhookNotifier_MaxRetriesExceeded(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	webhookStore := store.NewWebhookStore(db)

	// Create test server that always fails
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	config := DefaultWebhookNotifierConfig()
	config.WebhookURLs = []string{server.URL}
	config.PollInterval = 100 * time.Millisecond
	config.RetryBackoff = 50 * time.Millisecond
	config.MaxRetries = 2

	notifier := NewWebhookNotifier(webhookStore, config)
	notifier.Start()
	defer notifier.Stop()

	payload := &domain.AnalysisCompletedPayload{
		BuildID:   123,
		AppName:   "test-app",
		RiskLevel: "low",
		RiskScore: 10,
	}

	// Queue webhook
	id, err := notifier.NotifyAnalysisCompleted(server.URL, payload)
	if err != nil {
		t.Fatalf("NotifyAnalysisCompleted failed: %v", err)
	}

	// Wait for all retries to complete
	time.Sleep(600 * time.Millisecond)

	// Check webhook status
	webhook, _ := webhookStore.GetByID(id)
	if webhook == nil {
		t.Fatal("Webhook not found")
	}

	if webhook.Status != domain.WebhookStatusFailed {
		t.Errorf("Expected status %s, got %s", domain.WebhookStatusFailed, webhook.Status)
	}
	if webhook.LastError == "" {
		t.Error("Expected LastError to be set")
	}
}

func TestWebhookNotifier_BuildAnalysisPayload(t *testing.T) {
	build := &domain.Build{
		ID:        123,
		AppName:   "test-app",
		ImageTag:  "v1.0.0",
		CommitSHA: "abc123def456789",
		RiskLevel: "high",
		RiskScore: 75,
	}

	baseURL := "https://deploy.example.com"
	payload := BuildAnalysisPayload(build, baseURL)

	if payload.BuildID != build.ID {
		t.Errorf("BuildID: got %d, want %d", payload.BuildID, build.ID)
	}
	if payload.AppName != build.AppName {
		t.Errorf("AppName: got %s, want %s", payload.AppName, build.AppName)
	}
	if payload.CommitSHA != build.CommitSHA {
		t.Errorf("CommitSHA: got %s, want %s", payload.CommitSHA, build.CommitSHA)
	}
	if payload.RiskLevel != build.RiskLevel {
		t.Errorf("RiskLevel: got %s, want %s", payload.RiskLevel, build.RiskLevel)
	}
	if payload.RiskScore != build.RiskScore {
		t.Errorf("RiskScore: got %d, want %d", payload.RiskScore, build.RiskScore)
	}
	if payload.AnalysisURL == "" {
		t.Error("Expected AnalysisURL to be set")
	}
	if !payload.NeedsReview {
		t.Error("Expected NeedsReview to be true for high risk")
	}
}

func TestWebhookNotifier_BuildDeploymentFailedPayload(t *testing.T) {
	deployment := &domain.Deployment{
		ID:          456,
		BuildID:     123,
		AppName:     "test-app",
		Environment: domain.EnvProd,
		CommitSHA:   "abc123",
	}
	build := &domain.Build{
		ID:        123,
		AppName:   "test-app",
		ImageTag:  "v1.0.0",
		CommitSHA: "abc123",
	}

	baseURL := "https://deploy.example.com"
	errorMsg := "Git push failed: non-fast-forward"
	payload := BuildDeploymentFailedPayload(deployment, build, nil, errorMsg, baseURL)

	if payload.DeploymentID != deployment.ID {
		t.Errorf("DeploymentID: got %d, want %d", payload.DeploymentID, deployment.ID)
	}
	if payload.AppName != deployment.AppName {
		t.Errorf("AppName: got %s, want %s", payload.AppName, deployment.AppName)
	}
	if payload.Environment != "prod" {
		t.Errorf("Environment: got %s, want prod", payload.Environment)
	}
	if payload.ErrorMessage != errorMsg {
		t.Errorf("ErrorMessage: got %s, want %s", payload.ErrorMessage, errorMsg)
	}
	if payload.DeploymentURL == "" {
		t.Error("Expected DeploymentURL to be set")
	}
}

func TestWebhookNotifier_BuildDeploymentSucceededPayload(t *testing.T) {
	deployment := &domain.Deployment{
		ID:          456,
		BuildID:     123,
		AppName:     "test-app",
		Environment: domain.EnvStaging,
		CommitSHA:   "abc123",
		DeployedAt:  time.Now(),
	}
	build := &domain.Build{
		ID:        123,
		AppName:   "test-app",
		ImageTag:  "v1.0.0",
		CommitSHA: "abc123",
	}

	baseURL := "https://deploy.example.com"
	payload := BuildDeploymentSucceededPayload(deployment, build, nil, baseURL)

	if payload.DeploymentID != deployment.ID {
		t.Errorf("DeploymentID: got %d, want %d", payload.DeploymentID, deployment.ID)
	}
	if payload.AppName != deployment.AppName {
		t.Errorf("AppName: got %s, want %s", payload.AppName, deployment.AppName)
	}
	if payload.Environment != "staging" {
		t.Errorf("Environment: got %s, want staging", payload.Environment)
	}
	if payload.ChangelogURL == "" {
		t.Error("Expected ChangelogURL to be set")
	}
}

func TestWebhookNotifier_NotifyAll(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	webhookStore := store.NewWebhookStore(db)

	// Create multiple test servers
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server2.Close()

	config := DefaultWebhookNotifierConfig()
	config.WebhookURLs = []string{server1.URL, server2.URL}
	config.PollInterval = 100 * time.Millisecond

	notifier := NewWebhookNotifier(webhookStore, config)
	notifier.Start()
	defer notifier.Stop()

	payload := &domain.DeploymentSucceededPayload{
		DeploymentID: 456,
		BuildID:      123,
		AppName:      "test-app",
		Environment:  "staging",
	}

	// Queue to all URLs
	errs := notifier.NotifyAllDeploymentSucceeded(payload)
	if len(errs) > 0 {
		t.Errorf("Unexpected errors: %v", errs)
	}

	// Wait for delivery
	time.Sleep(300 * time.Millisecond)

	// Should have 2 webhooks in store
	webhooks, _ := webhookStore.List(domain.WebhookFilter{})
	if len(webhooks) != 2 {
		t.Errorf("Expected 2 webhooks, got %d", len(webhooks))
	}
}

func TestCalculateBackoff(t *testing.T) {
	config := DefaultWebhookNotifierConfig()
	config.RetryBackoff = time.Second

	n := &WebhookNotifier{config: config}

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{10, MaxBackoff}, // Should cap at MaxBackoff
	}

	for _, test := range tests {
		backoff := n.calculateBackoff(test.attempt)
		if backoff != test.expected {
			t.Errorf("Attempt %d: expected %v, got %v", test.attempt, test.expected, backoff)
		}
	}
}

func TestRepoWebURL(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme-corp/core-api.git": "https://github.com/acme-corp/core-api",
		"https://github.com/org/repo.git":       "https://github.com/org/repo",
		"ssh://git@github.com/org/repo.git":     "https://github.com/org/repo",
		"/home/me/repo":                         "",
		"file:///tmp/repo":                      "",
	}
	for in, want := range cases {
		if got := RepoWebURL(in); got != want {
			t.Errorf("RepoWebURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttachCommits(t *testing.T) {
	var commits []domain.CommitInfo
	for i := 0; i < maxWebhookCommits+5; i++ {
		commits = append(commits, domain.CommitInfo{SHA: fmt.Sprintf("sha%d", i), Author: "dev", Message: "  feat: x\n\nbody\n"})
	}
	p := &domain.DeploymentSucceededPayload{}
	AttachCommits(p, "git@github.com:org/repo.git", commits)

	if p.RepoURL != "https://github.com/org/repo" {
		t.Errorf("RepoURL = %q", p.RepoURL)
	}
	if p.CommitCount != maxWebhookCommits+5 || len(p.Commits) != maxWebhookCommits {
		t.Errorf("count = %d, len = %d", p.CommitCount, len(p.Commits))
	}
	if p.Commits[0].Subject != "feat: x" {
		t.Errorf("Subject = %q", p.Commits[0].Subject)
	}
}
