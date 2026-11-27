package domain

// ChangelogRequest is used to request a changelog between two commits
type ChangelogRequest struct {
	App     string `json:"app" validate:"required"`
	FromSHA string `json:"from_sha" validate:"required"`
	ToSHA   string `json:"to_sha" validate:"required"`
}

// ChangelogResponse contains the structured changelog
type ChangelogResponse struct {
	App            string            `json:"app"`
	SourceRepo     string            `json:"source_repo,omitempty"`
	SourcePath     string            `json:"source_path,omitempty"`
	FromCommit     string            `json:"from_commit"`
	ToCommit       string            `json:"to_commit"`
	Commits        []CommitInfo      `json:"commits"`
	CommitCount    int               `json:"commit_count"`
	DiffSummary    string            `json:"diff_summary"`
	RawDiff        string            `json:"raw_diff,omitempty"`
	RiskIndicators RiskIndicators    `json:"risk_indicators"`
	ContextFiles   map[string]string `json:"context_files,omitempty"`
}
