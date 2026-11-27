package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// BuildStore handles persistence for build records
type BuildStore struct {
	db *DB
}

// NewBuildStore creates a new BuildStore
func NewBuildStore(db *DB) *BuildStore {
	return &BuildStore{db: db}
}

// Create creates a new build record
func (s *BuildStore) Create(build *domain.Build) (int64, error) {
	result, err := s.db.Exec(`
		INSERT INTO builds (app_name, source_repo, source_path, image_tag, image_uri, commit_sha, branch, built_at, built_by, metadata,
		                    commits, commits_extracted_at, previous_build_id, analysis_status,
		                    risk_level, risk_score, llm_analysis, analysis_generated_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, build.AppName, build.SourceRepo, build.SourcePath, build.ImageTag, build.ImageURI,
		build.CommitSHA, build.Branch, build.BuiltAt, build.BuiltBy, build.Metadata,
		build.CommitsJSON, build.CommitsExtractedAt, build.PreviousBuildID, build.AnalysisStatus,
		build.RiskLevel, build.RiskScore, build.LLMAnalysis, build.AnalysisGeneratedAt)

	if err != nil {
		return 0, fmt.Errorf("failed to create build: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// GetByID retrieves a build by ID
func (s *BuildStore) GetByID(id int64) (*domain.Build, error) {
	build := &domain.Build{}
	var metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA sql.NullString
	var analysisGenAt, commitsExtractedAt sql.NullTime
	var previousBuildID sql.NullInt64
	err := s.db.QueryRow(`
		SELECT id, app_name, source_repo, source_path, image_tag, image_uri, commit_sha, branch,
		       built_at, built_by, metadata, created_at,
		       commits, commits_extracted_at, previous_build_id, analysis_status,
		       risk_level, risk_score, llm_analysis, analysis_generated_at, updated_at,
		       changelog_text, analysis_from_sha, analysis_to_sha
		FROM builds WHERE id = ?
	`, id).Scan(
		&build.ID, &build.AppName, &build.SourceRepo, &build.SourcePath, &build.ImageTag, &build.ImageURI,
		&build.CommitSHA, &build.Branch, &build.BuiltAt, &build.BuiltBy, &metadata, &build.CreatedAt,
		&commits, &commitsExtractedAt, &previousBuildID, &build.AnalysisStatus,
		&build.RiskLevel, &build.RiskScore, &llmAnalysis, &analysisGenAt, &build.UpdatedAt,
		&changelogText, &analysisFromSHA, &analysisToSHA,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get build: %w", err)
	}

	s.deserializeBuildFields(build, metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA, analysisGenAt, commitsExtractedAt, previousBuildID)
	return build, nil
}

// GetByAppAndTag retrieves a build by app name and image tag
func (s *BuildStore) GetByAppAndTag(appName, imageTag string) (*domain.Build, error) {
	build := &domain.Build{}
	var metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA sql.NullString
	var analysisGenAt, commitsExtractedAt sql.NullTime
	var previousBuildID sql.NullInt64
	err := s.db.QueryRow(`
		SELECT id, app_name, source_repo, source_path, image_tag, image_uri, commit_sha, branch,
		       built_at, built_by, metadata, created_at,
		       commits, commits_extracted_at, previous_build_id, analysis_status,
		       risk_level, risk_score, llm_analysis, analysis_generated_at, updated_at,
		       changelog_text, analysis_from_sha, analysis_to_sha
		FROM builds WHERE app_name = ? AND image_tag = ?
	`, appName, imageTag).Scan(
		&build.ID, &build.AppName, &build.SourceRepo, &build.SourcePath, &build.ImageTag, &build.ImageURI,
		&build.CommitSHA, &build.Branch, &build.BuiltAt, &build.BuiltBy, &metadata, &build.CreatedAt,
		&commits, &commitsExtractedAt, &previousBuildID, &build.AnalysisStatus,
		&build.RiskLevel, &build.RiskScore, &llmAnalysis, &analysisGenAt, &build.UpdatedAt,
		&changelogText, &analysisFromSHA, &analysisToSHA,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get build: %w", err)
	}

	s.deserializeBuildFields(build, metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA, analysisGenAt, commitsExtractedAt, previousBuildID)
	return build, nil
}

// GetPreviousBuildForComparison finds the most recent build for the same app/branch
func (s *BuildStore) GetPreviousBuildForComparison(appName, branch string) (*domain.Build, error) {
	build := &domain.Build{}
	var metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA sql.NullString
	var analysisGenAt, commitsExtractedAt sql.NullTime
	var previousBuildID sql.NullInt64
	err := s.db.QueryRow(`
		SELECT id, app_name, source_repo, source_path, image_tag, image_uri, commit_sha, branch,
		       built_at, built_by, metadata, created_at,
		       commits, commits_extracted_at, previous_build_id, analysis_status,
		       risk_level, risk_score, llm_analysis, analysis_generated_at, updated_at,
		       changelog_text, analysis_from_sha, analysis_to_sha
		FROM builds
		WHERE app_name = ? AND branch = ?
		ORDER BY built_at DESC
		LIMIT 1
	`, appName, branch).Scan(
		&build.ID, &build.AppName, &build.SourceRepo, &build.SourcePath, &build.ImageTag, &build.ImageURI,
		&build.CommitSHA, &build.Branch, &build.BuiltAt, &build.BuiltBy, &metadata, &build.CreatedAt,
		&commits, &commitsExtractedAt, &previousBuildID, &build.AnalysisStatus,
		&build.RiskLevel, &build.RiskScore, &llmAnalysis, &analysisGenAt, &build.UpdatedAt,
		&changelogText, &analysisFromSHA, &analysisToSHA,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get previous build: %w", err)
	}

	s.deserializeBuildFields(build, metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA, analysisGenAt, commitsExtractedAt, previousBuildID)
	return build, nil
}

// UpdateAnalysis updates the LLM analysis for a build
func (s *BuildStore) UpdateAnalysis(buildID int64, riskLevel string, riskScore int, analysis []byte) error {
	_, err := s.db.Exec(`
		UPDATE builds SET risk_level = ?, risk_score = ?, llm_analysis = ?, analysis_generated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, riskLevel, riskScore, analysis, buildID)
	if err != nil {
		return fmt.Errorf("failed to update analysis: %w", err)
	}
	return nil
}

// UpdateAnalysisStatus updates just the analysis status field
func (s *BuildStore) UpdateAnalysisStatus(buildID int64, status string) error {
	_, err := s.db.Exec(`
		UPDATE builds SET analysis_status = ?
		WHERE id = ?
	`, status, buildID)
	if err != nil {
		return fmt.Errorf("failed to update analysis status: %w", err)
	}
	return nil
}

// UpdateAnalysisWithStatus atomically updates all analysis fields + status
func (s *BuildStore) UpdateAnalysisWithStatus(buildID int64, riskLevel string, riskScore int, analysis []byte, changelogText, analysisFromSHA, analysisToSHA, status string) error {
	_, err := s.db.Exec(`
		UPDATE builds SET
			risk_level = ?,
			risk_score = ?,
			llm_analysis = ?,
			changelog_text = ?,
			analysis_from_sha = ?,
			analysis_to_sha = ?,
			analysis_status = ?,
			analysis_generated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, riskLevel, riskScore, analysis, changelogText, analysisFromSHA, analysisToSHA, status, buildID)
	if err != nil {
		return fmt.Errorf("failed to update analysis with status: %w", err)
	}
	return nil
}

// buildSelectColumns is the canonical column list (in scanBuilds order) shared
// by every builds SELECT so the scan stays in sync across queries.
const buildSelectColumns = `id, app_name, source_repo, source_path, image_tag, image_uri, commit_sha, branch,
		       built_at, built_by, metadata, created_at,
		       commits, commits_extracted_at, previous_build_id, analysis_status,
		       risk_level, risk_score, llm_analysis, analysis_generated_at, updated_at,
		       changelog_text, analysis_from_sha, analysis_to_sha`

// List retrieves builds matching the filter
func (s *BuildStore) List(filter domain.BuildFilter) ([]*domain.Build, error) {
	query := `
		SELECT ` + buildSelectColumns + `
		FROM builds WHERE 1=1
	`
	args := []interface{}{}

	if filter.AppName != "" {
		query += " AND app_name = ?"
		args = append(args, filter.AppName)
	}
	if filter.Branch != "" {
		query += " AND branch = ?"
		args = append(args, filter.Branch)
	}
	if filter.BranchExclude != "" {
		query += " AND branch != ?"
		args = append(args, filter.BranchExclude)
	}

	if filter.AfterBuiltAt != nil && filter.AfterID != nil {
		query += " AND (built_at < ? OR (built_at = ? AND id < ?))"
		args = append(args, *filter.AfterBuiltAt, *filter.AfterBuiltAt, *filter.AfterID)
	}

	query += " ORDER BY built_at DESC, id DESC"

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list builds: %w", err)
	}
	defer rows.Close()

	return s.scanBuilds(rows)
}

// LatestBuildsPerApp returns up to perApp most recent builds for each requested
// app in a single windowed query, replacing the UI's one-request-per-app fan-out.
//
// Branch filtering is per app: `main` keeps only builds on the app's configured
// main branch, `experiment` keeps only the others, `all` keeps everything. The
// main branch lives in config (not the DB), so callers pass it in via AppBranch;
// it's joined as a VALUES table and the branch predicate is applied before the
// ROW_NUMBER() window, so the top-N is taken over the already-filtered set.
//
// Returns perApp+1 rows per app internally is not needed — callers detect "more"
// by comparing len(builds)==perApp, then page via the existing cursor endpoint.
func (s *BuildStore) LatestBuildsPerApp(apps []domain.AppBranch, perApp int, mode string) (map[string][]*domain.Build, error) {
	result := make(map[string][]*domain.Build)
	if len(apps) == 0 {
		return result, nil
	}
	if perApp <= 0 {
		perApp = 5
	}

	valuesParts := make([]string, len(apps))
	args := make([]interface{}, 0, len(apps)*2+1)
	for i, a := range apps {
		valuesParts[i] = "(?, ?)"
		args = append(args, a.AppName, a.MainBranch)
	}

	branchPredicate := ""
	switch mode {
	case domain.BatchBuildsModeMain:
		branchPredicate = "AND b.branch = w.main_branch"
	case domain.BatchBuildsModeExperiment:
		branchPredicate = "AND b.branch <> w.main_branch"
	}

	// Inner CTE selects b.* (table order); the outer SELECT re-projects in
	// scanBuilds order via buildSelectColumns, so column order stays correct
	// regardless of the physical table layout.
	query := fmt.Sprintf(`
		WITH wanted(app_name, main_branch) AS (VALUES %s),
		ranked AS (
			SELECT b.*,
			       ROW_NUMBER() OVER (
			           PARTITION BY b.app_name
			           ORDER BY b.built_at DESC, b.id DESC
			       ) AS rn
			FROM builds b
			JOIN wanted w ON w.app_name = b.app_name
			WHERE 1=1 %s
		)
		SELECT %s FROM ranked WHERE rn <= ?
		ORDER BY app_name, built_at DESC, id DESC
	`, strings.Join(valuesParts, ","), branchPredicate, buildSelectColumns)
	args = append(args, perApp)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list builds per app: %w", err)
	}
	defer rows.Close()

	builds, err := s.scanBuilds(rows)
	if err != nil {
		return nil, err
	}

	for _, b := range builds {
		result[b.AppName] = append(result[b.AppName], b)
	}
	return result, nil
}

func (s *BuildStore) scanBuilds(rows *sql.Rows) ([]*domain.Build, error) {
	var builds []*domain.Build

	for rows.Next() {
		build := &domain.Build{}
		var metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA sql.NullString
		var analysisGenAt, commitsExtractedAt sql.NullTime
		var previousBuildID sql.NullInt64
		err := rows.Scan(
			&build.ID, &build.AppName, &build.SourceRepo, &build.SourcePath, &build.ImageTag, &build.ImageURI,
			&build.CommitSHA, &build.Branch, &build.BuiltAt, &build.BuiltBy, &metadata, &build.CreatedAt,
			&commits, &commitsExtractedAt, &previousBuildID, &build.AnalysisStatus,
			&build.RiskLevel, &build.RiskScore, &llmAnalysis, &analysisGenAt, &build.UpdatedAt,
			&changelogText, &analysisFromSHA, &analysisToSHA,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan build: %w", err)
		}
		s.deserializeBuildFields(build, metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA, analysisGenAt, commitsExtractedAt, previousBuildID)
		builds = append(builds, build)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return builds, nil
}

// GetStuckAnalyses finds builds that have been in "in_progress" status for too long
func (s *BuildStore) GetStuckAnalyses(threshold time.Duration) ([]*domain.Build, error) {
	cutoff := time.Now().Add(-threshold)

	query := `
		SELECT ` + buildSelectColumns + `
		FROM builds
		WHERE analysis_status = 'in_progress' AND updated_at < ?
	`

	rows, err := s.db.Query(query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("failed to get stuck analyses: %w", err)
	}
	defer rows.Close()

	return s.scanBuilds(rows)
}

// deserializeBuildFields deserializes nullable fields into a Build struct
func (s *BuildStore) deserializeBuildFields(build *domain.Build, metadata, llmAnalysis, commits, changelogText, analysisFromSHA, analysisToSHA sql.NullString, analysisGenAt, commitsExtractedAt sql.NullTime, previousBuildID sql.NullInt64) {
	if metadata.Valid {
		build.Metadata = []byte(metadata.String)
	}
	if llmAnalysis.Valid {
		build.LLMAnalysis = []byte(llmAnalysis.String)
	}
	if analysisGenAt.Valid {
		build.AnalysisGeneratedAt = &analysisGenAt.Time
	}
	if commits.Valid {
		build.CommitsJSON = []byte(commits.String)
		var commitsList []domain.CommitInfo
		if err := json.Unmarshal(build.CommitsJSON, &commitsList); err == nil {
			build.Commits = commitsList
		}
	}
	if commitsExtractedAt.Valid {
		build.CommitsExtractedAt = &commitsExtractedAt.Time
	}
	if previousBuildID.Valid {
		build.PreviousBuildID = &previousBuildID.Int64
	}
	if changelogText.Valid {
		build.ChangelogText = &changelogText.String
	}
	if analysisFromSHA.Valid {
		build.AnalysisFromSHA = &analysisFromSHA.String
	}
	if analysisToSHA.Valid {
		build.AnalysisToSHA = &analysisToSHA.String
	}
}

// ListForAnalysis returns builds for the analysis backlog, ordered by id
// ascending so an `after_id` cursor can walk the whole set exactly once. Time
// cursors are avoided here on purpose: SQLite compares DATETIME as text, and
// this listing exists to be paged to completion by a script.
func (s *BuildStore) ListForAnalysis(filter domain.BuildAnalysisFilter) ([]*domain.Build, error) {
	query := `
		SELECT ` + buildSelectColumns + `
		FROM builds WHERE 1=1
	`
	args := []interface{}{}

	// analysis_status is NOT NULL with a four-value CHECK, so a never-analyzed
	// build reads as 'pending', indistinguishable from one queued right now.
	// "never" uses llm_analysis instead, which is the honest test for "this
	// build has no analysis to show".
	switch filter.Status {
	case "", "needed":
		query += " AND analysis_status IN ('pending', 'failed')"
	case "never":
		query += " AND (llm_analysis IS NULL OR llm_analysis = '')"
	case "all":
		// no filter
	default:
		query += " AND analysis_status = ?"
		args = append(args, filter.Status)
	}

	if filter.AppName != "" {
		query += " AND app_name = ?"
		args = append(args, filter.AppName)
	}
	if filter.RepoID != "" {
		query += " AND source_repo = ?"
		args = append(args, filter.RepoID)
	}
	if filter.AfterID > 0 {
		query += " AND id > ?"
		args = append(args, filter.AfterID)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	query += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list builds for analysis: %w", err)
	}
	defer rows.Close()

	return s.scanBuilds(rows)
}

// CountByAnalysisStatus reports how many builds sit in each analysis state, so
// a re-analysis run can size the work before starting it. It also reports
// "never": builds carrying no stored analysis, which "pending" alone cannot
// distinguish from one the worker is about to pick up.
func (s *BuildStore) CountByAnalysisStatus() (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT analysis_status, COUNT(*) FROM builds GROUP BY 1
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to count builds by analysis status: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("failed to scan analysis status count: %w", err)
		}
		counts[status] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts["total"] = total

	var never int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM builds WHERE llm_analysis IS NULL OR llm_analysis = ''
	`).Scan(&never); err != nil {
		return nil, fmt.Errorf("failed to count builds without analysis: %w", err)
	}
	counts["never"] = never

	return counts, nil
}
