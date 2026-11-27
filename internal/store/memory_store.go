package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// MemoryStore handles persistence for repository memory and versions
type MemoryStore struct {
	db *DB
}

// NewMemoryStore creates a new MemoryStore
func NewMemoryStore(db *DB) *MemoryStore {
	return &MemoryStore{db: db}
}

// GetByRepoID retrieves memory for a specific repo
func (s *MemoryStore) GetByRepoID(repoID string) (*domain.RepoMemory, error) {
	memory := &domain.RepoMemory{}

	// Get current memory with version
	var version sql.NullInt64
	err := s.db.QueryRow(`
		SELECT repo_id, COALESCE(version, 0), content, updated_at, updated_by
		FROM repo_memory WHERE repo_id = ?
	`, repoID).Scan(
		&memory.RepoID, &version, &memory.Content, &memory.UpdatedAt, &memory.UpdatedBy,
	)

	if err == sql.ErrNoRows {
		// Return empty memory if not found
		return &domain.RepoMemory{
			RepoID:  repoID,
			Version: 0,
			Content: "",
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get memory: %w", err)
	}

	if version.Valid {
		memory.Version = int(version.Int64)
	}

	return memory, nil
}

// Save creates or updates memory for a repo, creating a version entry
func (s *MemoryStore) Save(memory *domain.RepoMemory) error {
	// Start transaction
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get the highest version number from versions table (not just current memory)
	// This ensures we never reuse a version number even if current memory was deleted
	var maxVersion int
	err = tx.QueryRow(`
		SELECT COALESCE(MAX(version), 0) FROM repo_memory_versions WHERE repo_id = ?
	`, memory.RepoID).Scan(&maxVersion)

	if err != nil {
		return fmt.Errorf("failed to get max version: %w", err)
	}

	// New version is max + 1
	newVersion := maxVersion + 1
	memory.Version = newVersion

	// Ensure we have a timestamp
	if memory.UpdatedAt.IsZero() {
		memory.UpdatedAt = time.Now()
	}

	summary := strings.TrimSpace(memory.ChangeSummary)
	if summary == "" {
		summary = fmt.Sprintf("Updated to version %d", newVersion)
	}

	// Insert version record first
	_, err = tx.Exec(`
		INSERT INTO repo_memory_versions (repo_id, version, content, updated_at, updated_by, change_summary)
		VALUES (?, ?, ?, ?, ?, ?)
	`, memory.RepoID, newVersion, memory.Content, memory.UpdatedAt, memory.UpdatedBy, summary)

	if err != nil {
		return fmt.Errorf("failed to insert version: %w", err)
	}

	// Upsert current memory
	_, err = tx.Exec(`
		INSERT INTO repo_memory (repo_id, version, content, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET
			version = excluded.version,
			content = excluded.content,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by
	`, memory.RepoID, newVersion, memory.Content, memory.UpdatedAt, memory.UpdatedBy)

	if err != nil {
		return fmt.Errorf("failed to save memory: %w", err)
	}

	return tx.Commit()
}

// Delete removes memory for a repo (but keeps versions for history)
func (s *MemoryStore) Delete(repoID string) error {
	_, err := s.db.Exec(`DELETE FROM repo_memory WHERE repo_id = ?`, repoID)
	if err != nil {
		return fmt.Errorf("failed to delete memory: %w", err)
	}
	return nil
}

// GetVersions retrieves version history for a repo
func (s *MemoryStore) GetVersions(repoID string, limit int) ([]*domain.RepoMemoryVersion, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.Query(`
		SELECT id, repo_id, version, content, updated_at, updated_by, change_summary
		FROM repo_memory_versions
		WHERE repo_id = ?
		ORDER BY version DESC
		LIMIT ?
	`, repoID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list versions: %w", err)
	}
	defer rows.Close()

	return s.scanVersions(rows)
}

// GetVersion retrieves a specific version
func (s *MemoryStore) GetVersion(repoID string, version int) (*domain.RepoMemoryVersion, error) {
	v := &domain.RepoMemoryVersion{}
	var changeSummary sql.NullString

	err := s.db.QueryRow(`
		SELECT id, repo_id, version, content, updated_at, updated_by, change_summary
		FROM repo_memory_versions
		WHERE repo_id = ? AND version = ?
	`, repoID, version).Scan(
		&v.ID, &v.RepoID, &v.Version, &v.Content, &v.UpdatedAt, &v.UpdatedBy, &changeSummary,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}

	if changeSummary.Valid {
		v.ChangeSummary = changeSummary.String
	}

	return v, nil
}

// Rollback restores a specific version as a new current version. History is
// append-only, so a rollback is itself a version rather than a rewind.
func (s *MemoryStore) Rollback(repoID string, version int, updatedBy string) (*domain.RepoMemory, error) {
	old, err := s.GetVersion(repoID, version)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, fmt.Errorf("version %d not found", version)
	}

	if updatedBy == "" {
		updatedBy = "rollback"
	}

	newMemory := &domain.RepoMemory{
		RepoID:        repoID,
		Content:       old.Content,
		UpdatedAt:     time.Now(),
		UpdatedBy:     updatedBy,
		ChangeSummary: fmt.Sprintf("Rolled back to version %d", version),
	}

	if err := s.Save(newMemory); err != nil {
		return nil, fmt.Errorf("failed to save rollback: %w", err)
	}

	return newMemory, nil
}

// ListAll returns all current memories
func (s *MemoryStore) ListAll() ([]*domain.RepoMemory, error) {
	rows, err := s.db.Query(`
		SELECT repo_id, COALESCE(version, 0), content, updated_at, updated_by
		FROM repo_memory ORDER BY repo_id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list memories: %w", err)
	}
	defer rows.Close()

	return s.scanMemories(rows)
}

func (s *MemoryStore) scanMemories(rows *sql.Rows) ([]*domain.RepoMemory, error) {
	var memories []*domain.RepoMemory

	for rows.Next() {
		mem := &domain.RepoMemory{}
		var version sql.NullInt64
		var updatedBy sql.NullString

		err := rows.Scan(&mem.RepoID, &version, &mem.Content, &mem.UpdatedAt, &updatedBy)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory: %w", err)
		}

		if version.Valid {
			mem.Version = int(version.Int64)
		}
		if updatedBy.Valid {
			mem.UpdatedBy = updatedBy.String
		}

		memories = append(memories, mem)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return memories, nil
}

func (s *MemoryStore) scanVersions(rows *sql.Rows) ([]*domain.RepoMemoryVersion, error) {
	var versions []*domain.RepoMemoryVersion

	for rows.Next() {
		v := &domain.RepoMemoryVersion{}
		var changeSummary sql.NullString

		err := rows.Scan(&v.ID, &v.RepoID, &v.Version, &v.Content, &v.UpdatedAt, &v.UpdatedBy, &changeSummary)
		if err != nil {
			return nil, fmt.Errorf("failed to scan version: %w", err)
		}

		if changeSummary.Valid {
			v.ChangeSummary = changeSummary.String
		}

		versions = append(versions, v)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return versions, nil
}

// QueueSuggestions records LLM-proposed memory updates for review. It is
// best-effort by design: a suggestion that cannot be stored must never cost the
// build its analysis, so duplicates are skipped and the count of new rows is
// returned rather than an error per row.
func (s *MemoryStore) QueueSuggestions(repoID string, buildID *int64, updates []domain.MemoryUpdate) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}

	queued := 0
	for _, u := range updates {
		// The partial unique index makes a re-proposal of still-pending text a
		// no-op, which is why this ignores rather than upserts.
		res, err := s.db.Exec(`
			INSERT OR IGNORE INTO memory_suggestions (repo_id, build_id, action, section, content, reason, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)
		`, repoID, buildID, u.Action, u.Section, u.Content, u.Reason, time.Now())
		if err != nil {
			return queued, fmt.Errorf("failed to queue memory suggestion: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			queued++
		}
	}
	return queued, nil
}

// ListSuggestions returns suggestions, newest first. An empty repoID lists
// every repo; an empty status lists every status.
func (s *MemoryStore) ListSuggestions(repoID, status string, limit int) ([]*domain.MemorySuggestion, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT id, repo_id, build_id, action, section, content, reason, status,
		       created_at, resolved_at, resolved_by, resulting_version
		FROM memory_suggestions
		WHERE 1=1`
	args := []any{}
	if repoID != "" {
		query += " AND repo_id = ?"
		args = append(args, repoID)
	}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list memory suggestions: %w", err)
	}
	defer rows.Close()

	var out []*domain.MemorySuggestion
	for rows.Next() {
		sg := &domain.MemorySuggestion{}
		var buildID sql.NullInt64
		var section, reason, resolvedBy sql.NullString
		var resolvedAt sql.NullTime
		var resultingVersion sql.NullInt64

		if err := rows.Scan(&sg.ID, &sg.RepoID, &buildID, &sg.Action, &section, &sg.Content,
			&reason, &sg.Status, &sg.CreatedAt, &resolvedAt, &resolvedBy, &resultingVersion); err != nil {
			return nil, fmt.Errorf("failed to scan memory suggestion: %w", err)
		}

		if buildID.Valid {
			id := buildID.Int64
			sg.BuildID = &id
		}
		sg.Section = section.String
		sg.Reason = reason.String
		sg.ResolvedBy = resolvedBy.String
		if resolvedAt.Valid {
			t := resolvedAt.Time
			sg.ResolvedAt = &t
		}
		if resultingVersion.Valid {
			v := int(resultingVersion.Int64)
			sg.ResultingVersion = &v
		}

		out = append(out, sg)
	}

	return out, rows.Err()
}

// GetSuggestion reads one suggestion, or nil when it does not exist.
func (s *MemoryStore) GetSuggestion(id int64) (*domain.MemorySuggestion, error) {
	sg := &domain.MemorySuggestion{}
	var buildID sql.NullInt64
	var section, reason, resolvedBy sql.NullString
	var resolvedAt sql.NullTime
	var resultingVersion sql.NullInt64

	err := s.db.QueryRow(`
		SELECT id, repo_id, build_id, action, section, content, reason, status,
		       created_at, resolved_at, resolved_by, resulting_version
		FROM memory_suggestions WHERE id = ?
	`, id).Scan(&sg.ID, &sg.RepoID, &buildID, &sg.Action, &section, &sg.Content,
		&reason, &sg.Status, &sg.CreatedAt, &resolvedAt, &resolvedBy, &resultingVersion)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get memory suggestion: %w", err)
	}

	if buildID.Valid {
		v := buildID.Int64
		sg.BuildID = &v
	}
	sg.Section = section.String
	sg.Reason = reason.String
	sg.ResolvedBy = resolvedBy.String
	if resolvedAt.Valid {
		t := resolvedAt.Time
		sg.ResolvedAt = &t
	}
	if resultingVersion.Valid {
		v := int(resultingVersion.Int64)
		sg.ResultingVersion = &v
	}

	return sg, nil
}

// CountPendingByRepo returns how many suggestions await review per repo id.
func (s *MemoryStore) CountPendingByRepo() (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT repo_id, COUNT(*) FROM memory_suggestions WHERE status = 'pending' GROUP BY repo_id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to count pending suggestions: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var repoID string
		var n int
		if err := rows.Scan(&repoID, &n); err != nil {
			return nil, fmt.Errorf("failed to scan pending count: %w", err)
		}
		counts[repoID] = n
	}
	return counts, rows.Err()
}

// ResolveSuggestion marks a suggestion accepted or rejected. It returns false
// when the row was already resolved, so a double click cannot apply twice.
func (s *MemoryStore) ResolveSuggestion(id int64, status, resolvedBy string, resultingVersion *int) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE memory_suggestions
		SET status = ?, resolved_at = ?, resolved_by = ?, resulting_version = ?
		WHERE id = ? AND status = 'pending'
	`, status, time.Now(), resolvedBy, resultingVersion, id)
	if err != nil {
		return false, fmt.Errorf("failed to resolve memory suggestion: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
