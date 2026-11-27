package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

const (
	// DefaultTimeout for webhook delivery
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRetries for webhook delivery
	DefaultMaxRetries = 3
	// DefaultRetryBackoff base backoff between retries
	DefaultRetryBackoff = 5 * time.Second
	// MaxBackoff maximum backoff between retries
	MaxBackoff = 5 * time.Minute
	// CleanupInterval for old webhook cleanup
	CleanupInterval = 1 * time.Hour
	// MaxWebhookAge to keep succeeded/failed webhooks
	MaxWebhookAge = 7 * 24 * time.Hour
)

// WebhookNotifier handles webhook delivery
type WebhookNotifier struct {
	store     *store.WebhookStore
	client    *http.Client
	config    WebhookNotifierConfig
	stopCh    chan struct{}
	stoppedCh chan struct{}
}

// WebhookNotifierConfig configures the webhook notifier
type WebhookNotifierConfig struct {
	// URLs to send webhooks to (can be overridden per-event)
	WebhookURLs []string
	// Headers to include in all webhook requests
	Headers map[string]string
	// Timeout for HTTP requests
	Timeout time.Duration
	// MaxRetries per webhook
	MaxRetries int
	// RetryBackoff base duration between retries
	RetryBackoff time.Duration
	// PollInterval for checking pending webhooks
	PollInterval time.Duration
}

// DefaultWebhookNotifierConfig returns a default configuration
func DefaultWebhookNotifierConfig() WebhookNotifierConfig {
	return WebhookNotifierConfig{
		Headers:      make(map[string]string),
		Timeout:      DefaultTimeout,
		MaxRetries:   DefaultMaxRetries,
		RetryBackoff: DefaultRetryBackoff,
		PollInterval: 10 * time.Second,
	}
}

// NewWebhookNotifier creates a new webhook notifier
func NewWebhookNotifier(store *store.WebhookStore, config WebhookNotifierConfig) *WebhookNotifier {
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = DefaultMaxRetries
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = DefaultRetryBackoff
	}
	if config.PollInterval == 0 {
		config.PollInterval = 10 * time.Second
	}
	if config.Headers == nil {
		config.Headers = make(map[string]string)
	}

	// Add default content-type header if not set
	if _, ok := config.Headers["Content-Type"]; !ok {
		config.Headers["Content-Type"] = "application/json"
	}

	return &WebhookNotifier{
		store: store,
		client: &http.Client{
			Timeout: config.Timeout,
		},
		config:    config,
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// Start begins the background webhook delivery processor
func (n *WebhookNotifier) Start() {
	log.Println("[WebhookNotifier] Starting webhook delivery processor")
	go n.processLoop()
}

// Stop gracefully shuts down the webhook processor
func (n *WebhookNotifier) Stop() {
	log.Println("[WebhookNotifier] Stopping webhook delivery processor...")
	close(n.stopCh)
	<-n.stoppedCh
	log.Println("[WebhookNotifier] Webhook delivery processor stopped")
}

// processLoop continuously processes pending webhooks
func (n *WebhookNotifier) processLoop() {
	defer close(n.stoppedCh)

	ticker := time.NewTicker(n.config.PollInterval)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(CleanupInterval)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			n.processPendingWebhooks()
		case <-cleanupTicker.C:
			n.cleanupOldWebhooks()
		}
	}
}

// processPendingWebhooks fetches and delivers pending webhooks
func (n *WebhookNotifier) processPendingWebhooks() {
	webhooks, err := n.store.ListPending(time.Now())
	if err != nil {
		log.Printf("[WebhookNotifier] Failed to list pending webhooks: %v", err)
		return
	}

	if len(webhooks) == 0 {
		return
	}

	log.Printf("[WebhookNotifier] Processing %d pending webhooks", len(webhooks))

	for _, webhook := range webhooks {
		select {
		case <-n.stopCh:
			return
		default:
			n.deliverWebhook(webhook)
		}
	}
}

// deliverWebhook attempts to deliver a single webhook
func (n *WebhookNotifier) deliverWebhook(webhook *domain.Webhook) {
	// Mark as delivering to prevent concurrent delivery
	if err := n.store.MarkDelivering(webhook.ID); err != nil {
		// Already being delivered or state changed
		return
	}

	log.Printf("[WebhookNotifier] Delivering webhook %d (attempt %d/%d) to %s",
		webhook.ID, webhook.Attempts, webhook.MaxAttempts, webhook.URL)

	// Create delivery context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), n.config.Timeout)
	defer cancel()

	// Build request
	req, err := http.NewRequestWithContext(ctx, "POST", webhook.URL, bytes.NewReader(webhook.Payload))
	if err != nil {
		n.handleDeliveryError(webhook, fmt.Errorf("failed to create request: %w", err))
		return
	}

	// Add headers
	for key, value := range n.config.Headers {
		req.Header.Set(key, value)
	}

	// Attempt delivery
	resp, err := n.client.Do(req)
	if err != nil {
		n.handleDeliveryError(webhook, fmt.Errorf("delivery failed: %w", err))
		return
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Success!
		if err := n.store.MarkSucceeded(webhook.ID); err != nil {
			log.Printf("[WebhookNotifier] Failed to mark webhook %d as succeeded: %v", webhook.ID, err)
		} else {
			log.Printf("[WebhookNotifier] Successfully delivered webhook %d (HTTP %d)",
				webhook.ID, resp.StatusCode)
		}
	} else {
		// HTTP error - might be retryable
		n.handleDeliveryError(webhook, fmt.Errorf("HTTP error %d", resp.StatusCode))
	}
}

// handleDeliveryError handles delivery failure, potentially scheduling a retry
func (n *WebhookNotifier) handleDeliveryError(webhook *domain.Webhook, err error) {
	errorMsg := err.Error()

	if webhook.Attempts >= webhook.MaxAttempts {
		// Max retries exceeded - mark as failed
		if markErr := n.store.MarkFailed(webhook.ID, errorMsg); markErr != nil {
			log.Printf("[WebhookNotifier] Failed to mark webhook %d as failed: %v", webhook.ID, markErr)
		} else {
			log.Printf("[WebhookNotifier] Webhook %d failed permanently after %d attempts: %s",
				webhook.ID, webhook.Attempts, errorMsg)
		}
		return
	}

	// Calculate exponential backoff
	backoff := n.calculateBackoff(webhook.Attempts)
	nextRetry := time.Now().Add(backoff)

	if err := n.store.ScheduleRetry(webhook.ID, nextRetry, errorMsg); err != nil {
		log.Printf("[WebhookNotifier] Failed to schedule retry for webhook %d: %v", webhook.ID, err)
	} else {
		log.Printf("[WebhookNotifier] Scheduled retry for webhook %d at %s (attempt %d/%d, backoff: %s)",
			webhook.ID, nextRetry.Format(time.RFC3339), webhook.Attempts+1, webhook.MaxAttempts, backoff)
	}
}

// calculateBackoff calculates exponential backoff with jitter cap
func (n *WebhookNotifier) calculateBackoff(attempt int) time.Duration {
	// Exponential: backoff * 2^(attempt-1)
	backoff := n.config.RetryBackoff
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff > MaxBackoff {
			backoff = MaxBackoff
			break
		}
	}
	return backoff
}

// cleanupOldWebhooks removes old webhook records to prevent table bloat
func (n *WebhookNotifier) cleanupOldWebhooks() {
	deleted, err := n.store.DeleteOld(MaxWebhookAge)
	if err != nil {
		log.Printf("[WebhookNotifier] Failed to cleanup old webhooks: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[WebhookNotifier] Cleaned up %d old webhook records", deleted)
	}
}

// NotifyAnalysisCompleted queues a webhook for analysis completion
func (n *WebhookNotifier) NotifyAnalysisCompleted(url string, payload *domain.AnalysisCompletedPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateAnalysisCompletedWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create analysis webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued analysis completed webhook %d for %s (build %d)",
		id, payload.AppName, payload.BuildID)
	return id, nil
}

// NotifyAnalysisFailed queues a webhook for analysis failure
func (n *WebhookNotifier) NotifyAnalysisFailed(url string, payload *domain.AnalysisFailedPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateAnalysisFailedWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create analysis failed webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued analysis failed webhook %d for %s (build %d)",
		id, payload.AppName, payload.BuildID)
	return id, nil
}

// NotifyDeploymentFailed queues a webhook for deployment failure
func (n *WebhookNotifier) NotifyDeploymentFailed(url string, payload *domain.DeploymentFailedPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateDeploymentFailedWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create deployment failed webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued deployment failed webhook %d for %s deployment to %s",
		id, payload.AppName, payload.Environment)
	return id, nil
}

// NotifyDeploymentSucceeded queues a webhook for deployment success
func (n *WebhookNotifier) NotifyDeploymentSucceeded(url string, payload *domain.DeploymentSucceededPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateDeploymentSucceededWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create deployment succeeded webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued deployment succeeded webhook %d for %s deployment to %s",
		id, payload.AppName, payload.Environment)
	return id, nil
}

// NotifyAllAnalysisCompleted queues webhooks to all configured URLs
func (n *WebhookNotifier) NotifyAllAnalysisCompleted(payload *domain.AnalysisCompletedPayload) []error {
	var errs []error
	for _, url := range n.config.WebhookURLs {
		if _, err := n.NotifyAnalysisCompleted(url, payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// NotifyAllAnalysisFailed queues webhooks to all configured URLs
func (n *WebhookNotifier) NotifyAllAnalysisFailed(payload *domain.AnalysisFailedPayload) []error {
	var errs []error
	for _, url := range n.config.WebhookURLs {
		if _, err := n.NotifyAnalysisFailed(url, payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// NotifyAllDeploymentFailed queues webhooks to all configured URLs
func (n *WebhookNotifier) NotifyAllDeploymentFailed(payload *domain.DeploymentFailedPayload) []error {
	var errs []error
	for _, url := range n.config.WebhookURLs {
		if _, err := n.NotifyDeploymentFailed(url, payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// NotifyAllDeploymentSucceeded queues webhooks to all configured URLs
func (n *WebhookNotifier) NotifyAllDeploymentSucceeded(payload *domain.DeploymentSucceededPayload) []error {
	var errs []error
	for _, url := range n.config.WebhookURLs {
		if _, err := n.NotifyDeploymentSucceeded(url, payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// NotifyExperimentDeployed queues a webhook for an experiment deployment
func (n *WebhookNotifier) NotifyExperimentDeployed(url string, payload *domain.ExperimentDeployedPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateExperimentDeployedWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create experiment deployed webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued experiment_deployed webhook %d for %s (branch: %s) to %s",
		id, payload.AppName, payload.Branch, payload.Environment)
	return id, nil
}

// NotifyAllExperimentDeployed queues webhooks to all configured URLs
func (n *WebhookNotifier) NotifyAllExperimentDeployed(payload *domain.ExperimentDeployedPayload) []error {
	var errs []error
	for _, url := range n.config.WebhookURLs {
		if _, err := n.NotifyExperimentDeployed(url, payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// NotifyExperimentReplaced queues a webhook for when an experiment is replaced by a production build
func (n *WebhookNotifier) NotifyExperimentReplaced(url string, payload *domain.ExperimentReplacedPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateExperimentReplacedWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create experiment replaced webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued experiment_replaced webhook %d for %s (replaced: %s) in %s",
		id, payload.AppName, payload.ReplacedExperimentBranch, payload.Environment)
	return id, nil
}

// NotifyAllExperimentReplaced queues webhooks to all configured URLs
func (n *WebhookNotifier) NotifyAllExperimentReplaced(payload *domain.ExperimentReplacedPayload) []error {
	var errs []error
	for _, url := range n.config.WebhookURLs {
		if _, err := n.NotifyExperimentReplaced(url, payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// extractLLMFields extracts summary and concerns from a build's LLMAnalysis JSON.
func extractLLMFields(build *domain.Build) (summary string, concerns []string) {
	if len(build.LLMAnalysis) == 0 {
		return
	}
	var analysis struct {
		Summary  string   `json:"summary"`
		Concerns []string `json:"concerns"`
	}
	json.Unmarshal(build.LLMAnalysis, &analysis)
	return analysis.Summary, analysis.Concerns
}

// BuildAnalysisPayload creates an analysis payload from a build and analysis data
func BuildAnalysisPayload(build *domain.Build, baseURL string) *domain.AnalysisCompletedPayload {
	var analysisURL string
	if baseURL != "" {
		analysisURL = fmt.Sprintf("%s/api/v1/builds/%d", baseURL, build.ID)
	}

	summary, concerns := extractLLMFields(build)

	return &domain.AnalysisCompletedPayload{
		BuildID:     build.ID,
		AppName:     build.AppName,
		ImageTag:    build.ImageTag,
		CommitSHA:   build.CommitSHA,
		RiskLevel:   build.RiskLevel,
		RiskScore:   build.RiskScore,
		Summary:     summary,
		Concerns:    concerns,
		NeedsReview: build.RiskScore > 50 || build.RiskLevel == "high" || build.RiskLevel == "critical",
		AnalysisURL: analysisURL,
		EventType:   string(domain.WebhookEventAnalysisCompleted),
	}
}

// BuildAnalysisFailedPayload creates an analysis failed payload from a build.
func BuildAnalysisFailedPayload(build *domain.Build, baseURL string) *domain.AnalysisFailedPayload {
	var analysisURL string
	if baseURL != "" {
		analysisURL = fmt.Sprintf("%s/api/v1/builds/%d", baseURL, build.ID)
	}

	return &domain.AnalysisFailedPayload{
		BuildID:     build.ID,
		AppName:     build.AppName,
		ImageTag:    build.ImageTag,
		CommitSHA:   build.CommitSHA,
		Branch:      build.Branch,
		FailedAt:    time.Now(),
		AnalysisURL: analysisURL,
		EventType:   string(domain.WebhookEventAnalysisFailed),
	}
}

// BuildDeploymentFailedPayload creates a deployment failed payload.
// prevBuild is the build that was previously deployed (may be nil for first deployments).
func BuildDeploymentFailedPayload(deployment *domain.Deployment, build *domain.Build, prevBuild *domain.Build, errorMsg, baseURL string) *domain.DeploymentFailedPayload {
	var deploymentURL string
	if baseURL != "" {
		deploymentURL = fmt.Sprintf("%s/api/v1/deployments/%d", baseURL, deployment.ID)
	}

	isRollback := prevBuild != nil && build.ID < prevBuild.ID
	prevImageTag := ""
	if prevBuild != nil {
		prevImageTag = prevBuild.ImageTag
	}

	summary, concerns := extractLLMFields(build)

	return &domain.DeploymentFailedPayload{
		DeploymentID:     deployment.ID,
		BuildID:          deployment.BuildID,
		AppName:          deployment.AppName,
		Environment:      string(deployment.Environment),
		ImageTag:         build.ImageTag,
		CommitSHA:        deployment.CommitSHA,
		ErrorMessage:     errorMsg,
		FailedAt:         time.Now(),
		DeploymentURL:    deploymentURL,
		EventType:        string(domain.WebhookEventDeploymentFailed),
		DeployedBy:       deployment.DeployedBy,
		IsRollback:       isRollback,
		PreviousImageTag: prevImageTag,
		RiskLevel:        build.RiskLevel,
		RiskScore:        build.RiskScore,
		Summary:          summary,
		Concerns:         concerns,
		AnalysisStatus:   build.AnalysisStatus,
	}
}

// BuildExperimentDeployedPayload creates a payload for when an experiment build is deployed.
func BuildExperimentDeployedPayload(deployment *domain.Deployment, build *domain.Build) *domain.ExperimentDeployedPayload {
	return &domain.ExperimentDeployedPayload{
		DeploymentID: deployment.ID,
		BuildID:      build.ID,
		AppName:      deployment.AppName,
		Environment:  string(deployment.Environment),
		Branch:       build.Branch,
		ImageTag:     build.ImageTag,
		CommitSHA:    build.CommitSHA,
		DeployedBy:   deployment.DeployedBy,
		DeployedAt:   deployment.DeployedAt,
		EventType:    string(domain.WebhookEventExperimentDeployed),
	}
}

// BuildExperimentReplacedPayload creates a payload for when a production build replaces an experiment.
// prevBuild is the experiment build that was replaced.
func BuildExperimentReplacedPayload(deployment *domain.Deployment, build *domain.Build, prevBuild *domain.Build) *domain.ExperimentReplacedPayload {
	payload := &domain.ExperimentReplacedPayload{
		DeploymentID: deployment.ID,
		BuildID:      build.ID,
		AppName:      deployment.AppName,
		Environment:  string(deployment.Environment),
		ImageTag:     build.ImageTag,
		CommitSHA:    build.CommitSHA,
		DeployedBy:   deployment.DeployedBy,
		DeployedAt:   deployment.DeployedAt,
		EventType:    string(domain.WebhookEventExperimentReplaced),
	}
	if prevBuild != nil {
		payload.ReplacedExperimentBranch = prevBuild.Branch
		payload.ReplacedExperimentImageTag = prevBuild.ImageTag
		payload.ReplacedExperimentBuildID = prevBuild.ID
	}
	return payload
}

// BuildDeploymentSucceededPayload creates a deployment succeeded payload.
// prevBuild is the build that was previously deployed (may be nil for first deployments).
func BuildDeploymentSucceededPayload(deployment *domain.Deployment, build *domain.Build, prevBuild *domain.Build, baseURL string) *domain.DeploymentSucceededPayload {
	var changelogURL string
	if baseURL != "" {
		changelogURL = fmt.Sprintf("%s/api/v1/deployments/%d", baseURL, deployment.ID)
	}

	isRollback := prevBuild != nil && build.ID < prevBuild.ID
	prevImageTag := ""
	prevCommitSHA := ""
	if prevBuild != nil {
		prevImageTag = prevBuild.ImageTag
		prevCommitSHA = prevBuild.CommitSHA
	}

	summary, concerns := extractLLMFields(build)
	changelogText := ""
	if build.ChangelogText != nil {
		changelogText = *build.ChangelogText
	}

	return &domain.DeploymentSucceededPayload{
		DeploymentID:      deployment.ID,
		BuildID:           deployment.BuildID,
		AppName:           deployment.AppName,
		Environment:       string(deployment.Environment),
		ImageTag:          build.ImageTag,
		CommitSHA:         deployment.CommitSHA,
		DeployedAt:        deployment.DeployedAt,
		ChangelogURL:      changelogURL,
		EventType:         string(domain.WebhookEventDeploymentSucceeded),
		DeployedBy:        deployment.DeployedBy,
		IsRollback:        isRollback,
		PreviousImageTag:  prevImageTag,
		PreviousCommitSHA: prevCommitSHA,
		RiskLevel:         build.RiskLevel,
		RiskScore:         build.RiskScore,
		Summary:           summary,
		Concerns:          concerns,
		ChangelogText:     changelogText,
		AnalysisStatus:    build.AnalysisStatus,
	}
}

// maxWebhookCommits keeps a deploy payload well under a chat message's size limit.
const maxWebhookCommits = 30

// AttachCommits fills the repo link and the commit list, newest first.
func AttachCommits(payload *domain.DeploymentSucceededPayload, gitURL string, commits []domain.CommitInfo) {
	payload.RepoURL = RepoWebURL(gitURL)
	payload.CommitCount = len(commits)
	for i, c := range commits {
		if i == maxWebhookCommits {
			break
		}
		subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
		payload.Commits = append(payload.Commits, domain.WebhookCommit{
			SHA:     c.SHA,
			Author:  c.Author,
			Subject: strings.TrimSpace(subject),
		})
	}
}

// RepoWebURL turns a clone URL into its browsable https form, or "" for local repos.
func RepoWebURL(gitURL string) string {
	u := strings.TrimSuffix(strings.TrimSpace(gitURL), ".git")
	switch {
	case strings.HasPrefix(u, "https://"):
		return u
	case strings.HasPrefix(u, "http://"):
		return "https://" + strings.TrimPrefix(u, "http://")
	case strings.HasPrefix(u, "ssh://"):
		u = strings.TrimPrefix(u, "ssh://")
		if _, rest, ok := strings.Cut(u, "@"); ok {
			u = rest
		}
		return "https://" + u
	case strings.HasPrefix(u, "git@"):
		host, path, ok := strings.Cut(strings.TrimPrefix(u, "git@"), ":")
		if !ok {
			return ""
		}
		return "https://" + host + "/" + path
	}
	return ""
}

// NotifyEnvResetRequested queues a webhook asking for an environment reset
func (n *WebhookNotifier) NotifyEnvResetRequested(url string, payload *domain.EnvResetRequestedPayload) (int64, error) {
	if url == "" {
		return 0, fmt.Errorf("webhook URL is required")
	}

	id, err := n.store.CreateEnvResetRequestedWebhook(url, payload, n.config.MaxRetries)
	if err != nil {
		return 0, fmt.Errorf("failed to create env reset webhook: %w", err)
	}

	log.Printf("[WebhookNotifier] Queued env_reset_requested webhook %d for %s in %s (reset %d)",
		id, payload.AppName, payload.Environment, payload.ResetID)
	return id, nil
}

// NotifyAllEnvResetRequested queues the reset request to targetURL when set,
// otherwise to every configured webhook URL. Returns how many were queued.
func (n *WebhookNotifier) NotifyAllEnvResetRequested(targetURL string, payload *domain.EnvResetRequestedPayload) (int, []error) {
	urls := n.config.WebhookURLs
	if targetURL != "" {
		urls = []string{targetURL}
	}

	queued := 0
	var errs []error
	for _, url := range urls {
		if _, err := n.NotifyEnvResetRequested(url, payload); err != nil {
			errs = append(errs, err)
			continue
		}
		queued++
	}
	return queued, errs
}
