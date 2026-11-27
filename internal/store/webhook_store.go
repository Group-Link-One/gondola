package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"deploy-api/internal/domain"
)

// WebhookStore handles webhook persistence
type WebhookStore struct {
	db *DB
}

// NewWebhookStore creates a new webhook store
func NewWebhookStore(db *DB) *WebhookStore {
	return &WebhookStore{db: db}
}

// Create stores a new webhook
func (s *WebhookStore) Create(webhook *domain.Webhook) (int64, error) {
	query := `
		INSERT INTO webhooks (event_type, payload, url, status, attempts, max_attempts, next_retry_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	now := time.Now()
	result, err := s.db.Exec(
		query,
		webhook.EventType,
		webhook.Payload,
		webhook.URL,
		webhook.Status,
		webhook.Attempts,
		webhook.MaxAttempts,
		webhook.NextRetryAt,
		now,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to create webhook: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get webhook id: %w", err)
	}

	return id, nil
}

// GetByID retrieves a webhook by ID
func (s *WebhookStore) GetByID(id int64) (*domain.Webhook, error) {
	query := `
		SELECT id, event_type, payload, url, status, attempts, max_attempts, 
		       next_retry_at, last_error, delivered_at, created_at, updated_at
		FROM webhooks
		WHERE id = ?
	`

	webhook := &domain.Webhook{}
	var nextRetryAt, deliveredAt sql.NullTime
	var lastError sql.NullString

	err := s.db.QueryRow(query, id).Scan(
		&webhook.ID,
		&webhook.EventType,
		&webhook.Payload,
		&webhook.URL,
		&webhook.Status,
		&webhook.Attempts,
		&webhook.MaxAttempts,
		&nextRetryAt,
		&lastError,
		&deliveredAt,
		&webhook.CreatedAt,
		&webhook.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get webhook: %w", err)
	}

	if nextRetryAt.Valid {
		webhook.NextRetryAt = &nextRetryAt.Time
	}
	if deliveredAt.Valid {
		webhook.DeliveredAt = &deliveredAt.Time
	}
	if lastError.Valid {
		webhook.LastError = lastError.String
	}

	return webhook, nil
}

// List retrieves webhooks with optional filtering
func (s *WebhookStore) List(filter domain.WebhookFilter) ([]*domain.Webhook, error) {
	query := `
		SELECT id, event_type, payload, url, status, attempts, max_attempts, 
		       next_retry_at, last_error, delivered_at, created_at, updated_at
		FROM webhooks
		WHERE 1=1
	`
	args := []interface{}{}

	if filter.Status != "" {
		query += " AND status = ?"
		args = append(args, filter.Status)
	}
	if filter.EventType != "" {
		query += " AND event_type = ?"
		args = append(args, filter.EventType)
	}

	query += " ORDER BY created_at DESC"

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
		if filter.Offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", filter.Offset)
		}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list webhooks: %w", err)
	}
	defer rows.Close()

	return s.scanWebhooks(rows)
}

// ListPending retrieves webhooks ready for delivery (pending or failed with retry scheduled)
func (s *WebhookStore) ListPending(now time.Time) ([]*domain.Webhook, error) {
	query := `
		SELECT id, event_type, payload, url, status, attempts, max_attempts, 
		       next_retry_at, last_error, delivered_at, created_at, updated_at
		FROM webhooks
		WHERE status IN ('pending', 'failed')
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		  AND attempts < max_attempts
		ORDER BY created_at ASC, id ASC
		LIMIT 100
	`

	rows, err := s.db.Query(query, now)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending webhooks: %w", err)
	}
	defer rows.Close()

	return s.scanWebhooks(rows)
}

// MarkDelivering marks a webhook as currently being delivered
func (s *WebhookStore) MarkDelivering(id int64) error {
	query := `
		UPDATE webhooks 
		SET status = 'delivering', attempts = attempts + 1, updated_at = ?
		WHERE id = ? AND status IN ('pending', 'failed')
	`

	result, err := s.db.Exec(query, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to mark webhook delivering: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("webhook %d not found or not in deliverable state", id)
	}

	return nil
}

// MarkSucceeded marks a webhook as successfully delivered
func (s *WebhookStore) MarkSucceeded(id int64) error {
	query := `
		UPDATE webhooks 
		SET status = 'succeeded', delivered_at = ?, next_retry_at = NULL, updated_at = ?
		WHERE id = ?
	`

	now := time.Now()
	_, err := s.db.Exec(query, now, now, id)
	if err != nil {
		return fmt.Errorf("failed to mark webhook succeeded: %w", err)
	}

	return nil
}

// MarkFailed marks a webhook as failed (max retries exceeded)
func (s *WebhookStore) MarkFailed(id int64, errorMsg string) error {
	query := `
		UPDATE webhooks 
		SET status = 'failed', last_error = ?, next_retry_at = NULL, updated_at = ?
		WHERE id = ?
	`

	now := time.Now()
	_, err := s.db.Exec(query, errorMsg, now, id)
	if err != nil {
		return fmt.Errorf("failed to mark webhook failed: %w", err)
	}

	return nil
}

// Retry re-queues a webhook for delivery. Used by the admin "Retry" action
// for webhooks in failed/succeeded state. Clears next_retry_at and extends
// max_attempts so the worker's next poll picks it up.
func (s *WebhookStore) Retry(id int64, additionalAttempts int) error {
	if additionalAttempts <= 0 {
		additionalAttempts = 3
	}

	query := `
		UPDATE webhooks
		SET status = 'pending',
		    next_retry_at = NULL,
		    last_error = NULL,
		    delivered_at = NULL,
		    max_attempts = MAX(max_attempts, attempts + ?),
		    updated_at = ?
		WHERE id = ?
	`

	result, err := s.db.Exec(query, additionalAttempts, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to retry webhook: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("webhook %d not found", id)
	}

	return nil
}

// ScheduleRetry schedules a webhook for retry with exponential backoff
func (s *WebhookStore) ScheduleRetry(id int64, nextRetry time.Time, errorMsg string) error {
	query := `
		UPDATE webhooks 
		SET status = 'failed', 
		    next_retry_at = ?, 
		    last_error = ?, 
		    updated_at = ?
		WHERE id = ?
	`

	_, err := s.db.Exec(query, nextRetry, errorMsg, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to schedule webhook retry: %w", err)
	}

	return nil
}

// DeleteOld removes old delivered/failed webhooks to prevent table bloat
func (s *WebhookStore) DeleteOld(maxAge time.Duration) (int64, error) {
	query := `
		DELETE FROM webhooks 
		WHERE status IN ('succeeded', 'failed')
		  AND updated_at < ?
	`

	cutoff := time.Now().Add(-maxAge)
	result, err := s.db.Exec(query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old webhooks: %w", err)
	}

	return result.RowsAffected()
}

// scanWebhooks scans rows into webhook structs
func (s *WebhookStore) scanWebhooks(rows *sql.Rows) ([]*domain.Webhook, error) {
	var webhooks []*domain.Webhook

	for rows.Next() {
		webhook := &domain.Webhook{}
		var nextRetryAt, deliveredAt sql.NullTime
		var lastError sql.NullString

		err := rows.Scan(
			&webhook.ID,
			&webhook.EventType,
			&webhook.Payload,
			&webhook.URL,
			&webhook.Status,
			&webhook.Attempts,
			&webhook.MaxAttempts,
			&nextRetryAt,
			&lastError,
			&deliveredAt,
			&webhook.CreatedAt,
			&webhook.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan webhook: %w", err)
		}

		if nextRetryAt.Valid {
			webhook.NextRetryAt = &nextRetryAt.Time
		}
		if deliveredAt.Valid {
			webhook.DeliveredAt = &deliveredAt.Time
		}
		if lastError.Valid {
			webhook.LastError = lastError.String
		}

		webhooks = append(webhooks, webhook)
	}

	return webhooks, rows.Err()
}

// CreateAnalysisCompletedWebhook creates a webhook for analysis completion
func (s *WebhookStore) CreateAnalysisCompletedWebhook(url string, payload *domain.AnalysisCompletedPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisCompleted,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}

// CreateAnalysisFailedWebhook creates a webhook for analysis failure
func (s *WebhookStore) CreateAnalysisFailedWebhook(url string, payload *domain.AnalysisFailedPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventAnalysisFailed,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}

// CreateDeploymentFailedWebhook creates a webhook for deployment failure
func (s *WebhookStore) CreateDeploymentFailedWebhook(url string, payload *domain.DeploymentFailedPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventDeploymentFailed,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}

// CreateDeploymentSucceededWebhook creates a webhook for deployment success
func (s *WebhookStore) CreateDeploymentSucceededWebhook(url string, payload *domain.DeploymentSucceededPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventDeploymentSucceeded,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}

// CreateExperimentDeployedWebhook creates a webhook for experiment deployment
func (s *WebhookStore) CreateExperimentDeployedWebhook(url string, payload *domain.ExperimentDeployedPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventExperimentDeployed,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}

// CreateExperimentReplacedWebhook creates a webhook for experiment replacement
func (s *WebhookStore) CreateExperimentReplacedWebhook(url string, payload *domain.ExperimentReplacedPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventExperimentReplaced,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}

// CreateEnvResetRequestedWebhook creates a webhook asking for an environment reset
func (s *WebhookStore) CreateEnvResetRequestedWebhook(url string, payload *domain.EnvResetRequestedPayload, maxAttempts int) (int64, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	webhook := &domain.Webhook{
		EventType:   domain.WebhookEventEnvResetRequested,
		Payload:     payloadJSON,
		URL:         url,
		Status:      domain.WebhookStatusPending,
		Attempts:    0,
		MaxAttempts: maxAttempts,
	}

	return s.Create(webhook)
}
