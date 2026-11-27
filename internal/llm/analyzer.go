package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"deploy-api/internal/domain"
)

// DefaultAnalysisSystemPrompt is the default system prompt for risk analysis (configurable part)
const DefaultAnalysisSystemPrompt = `You are a deployment risk analyzer. Analyze the provided changelog.
Be concise but thorough. Consider: database migrations, API changes, configuration changes, security implications, and test coverage.

Treat two kinds of change as risky even when the code itself is simple, and name the exact item in "concerns":
- Contract changes: anything another service or client depends on, such as an HTTP route or response shape, a DTO, a queue message or event payload, an enum value, a protobuf or OpenAPI schema. Say whether it is backwards compatible and which side must deploy first.
- Config changes: a new or renamed environment variable, a changed default, a feature flag, a Kubernetes or Kustomize manifest. A variable the code now reads with no default fails at startup if it is missing in the target environment.`

// DefaultChangelogSystemPrompt is the default system prompt for changelog generation (configurable part)
const DefaultChangelogSystemPrompt = `You are a changelog generator. Your task is to analyze git changes and output a human-readable changelog.

Structure your response using these sections:
## Features
- Description of feature (commit hash)

## Fixes
- Description of fix (commit hash)

## Other Changes
- Description of change (commit hash)

Guidelines:
- Group related changes together by functionality, not by file/directory
- Include commit hashes in parentheses at the end of each item
- Use Portuguese if the commit messages are in Portuguese
- Be concise but descriptive

If project memory specifies a different format, follow that instead.`

// AnalysisGuardrail is the hardcoded output contract appended to the
// configurable analysis system prompt. It is a package constant so the external
// analysis endpoint can hand a proxy the exact system prompt the worker uses.
const AnalysisGuardrail = `

Respond ONLY with valid JSON matching this exact structure:
{
  "risk_level": "low|medium|high|critical",
  "risk_score": <number 0-100>,
  "summary": "brief summary of changes",
  "concerns": ["concern 1", "concern 2"],
  "recommendations": ["recommendation 1", "recommendation 2"],
  "needs_review": true|false,
  "memory_updates": []
}

About "memory_updates": these are proposed edits to the Project Memory shown
above, which is prepended to every future analysis of this repository. A human
reviews each one before it is applied.

Propose an update ONLY for a durable fact about this repository that would have
helped you judge THIS change and will still be true in six months: a convention,
an area that is dangerous to touch, a deploy step that is not automated, a
pattern that looks alarming but is routine here. Correct or remove memory that
these changes have made wrong.

Do NOT propose: a summary of this change, anything already stated in the memory,
release notes, one-off facts, or restatements of the diff. An empty list is the
correct and expected answer for most changes.

Each entry:
{
  "action": "add" | "remove" | "replace",
  "section": "markdown heading to target, or \"\" to append at the end",
  "content": "the exact text to add or remove, in the memory's own style",
  "reason": "why this is worth remembering, in one sentence"
}

IMPORTANT: Output ONLY the JSON object. No markdown, no backticks, no explanatory text.`

// ChangelogAnalyzer analyzes changelogs using an LLM
// Token limits by provider:
// - Cloudflare: 60K context
// - Gemini: 1M input, 8K-65K output
// - Ollama: Depends on model
// - OpenRouter: Depends on model
type ChangelogAnalyzer struct {
	provider              Provider
	providerName          string
	debug                 bool
	maxTokens             int    // Context window size
	analysisSystemPrompt  string // Configurable part of analysis system prompt
	changelogSystemPrompt string // Configurable part of changelog system prompt
}

// RiskAnalysis is the result of analyzing a changelog for risks
type RiskAnalysis struct {
	RiskLevel       string   `json:"risk_level"`      // "low", "medium", "high", "critical"
	RiskScore       int      `json:"risk_score"`      // 0-100
	Summary         string   `json:"summary"`         // Brief summary of changes
	Concerns        []string `json:"concerns"`        // List of specific concerns
	Recommendations []string `json:"recommendations"` // Deployment recommendations
	NeedsReview     bool     `json:"needs_review"`    // Whether human review is recommended

	// MemoryUpdates are durable facts the model proposes recording about this
	// repo. They are suggestions only: nothing is applied without a human.
	MemoryUpdates []domain.MemoryUpdate `json:"memory_updates,omitempty"`
}

// NewChangelogAnalyzer creates a new changelog analyzer
// Sets appropriate token limits based on provider capabilities
func NewChangelogAnalyzer(provider Provider, analysisPrompt, changelogPrompt string) *ChangelogAnalyzer {
	debug := os.Getenv("DEBUG") == "true" || os.Getenv("DEBUG_LLM") == "true"

	// Detect provider type and set appropriate limits
	providerName := "unknown"
	maxTokens := 60000 // Default conservative limit

	if provider != nil {
		providerName = provider.Name()
		switch providerName {
		case "gemini":
			// Gemini 2.0 Flash: 1M input tokens, but keep reasonable for output
			maxTokens = 120000 // 120K for input context
		case "cloudflare":
			maxTokens = 60000 // Cloudflare limit
		case "ollama":
			// Ollama models vary, use conservative default
			maxTokens = 8000
			// Check if env overrides
			if envLimit := os.Getenv("OLLAMA_MAX_TOKENS"); envLimit != "" {
				fmt.Sscanf(envLimit, "%d", &maxTokens)
			}
		case "openrouter":
			maxTokens = 60000 // OpenRouter varies by model
		}
	}

	// Use defaults if no custom prompts provided
	if analysisPrompt == "" {
		analysisPrompt = DefaultAnalysisSystemPrompt
	}
	if changelogPrompt == "" {
		changelogPrompt = DefaultChangelogSystemPrompt
	}

	if debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] ChangelogAnalyzer initialized: provider=%s, maxTokens=%d, customAnalysisPrompt=%v, customChangelogPrompt=%v\n",
			providerName, maxTokens, analysisPrompt != DefaultAnalysisSystemPrompt, changelogPrompt != DefaultChangelogSystemPrompt)
	}

	return &ChangelogAnalyzer{
		provider:              provider,
		providerName:          providerName,
		debug:                 debug,
		maxTokens:             maxTokens,
		analysisSystemPrompt:  analysisPrompt,
		changelogSystemPrompt: changelogPrompt,
	}
}

// Complete performs a raw LLM completion (exposed for custom prompts like memory updates)
func (a *ChangelogAnalyzer) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if a.provider == nil {
		return nil, fmt.Errorf("no LLM provider configured")
	}
	return a.provider.Complete(ctx, req)
}

// AnalyzeChangelog analyzes a changelog for deployment risks
func (a *ChangelogAnalyzer) AnalyzeChangelog(ctx context.Context, appName string, changelog *domain.ChangelogResponse, memory string) (*RiskAnalysis, error) {
	if a.provider == nil {
		return nil, fmt.Errorf("no LLM provider configured")
	}

	// Build the prompt with provider-specific token limits
	prompt := buildAnalysisPrompt(appName, changelog, a.maxTokens, memory)

	if a.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] AnalyzeChangelog prompt length: %d chars (est. %d tokens)\n", len(prompt), len(prompt)/4)
		if os.Getenv("DEBUG_FULL") == "true" {
			fmt.Fprintf(os.Stderr, "[DEBUG] === FULL PROMPT ===\n%s\n[DEBUG] === END PROMPT ===\n", prompt)
		}
	}

	// Build system prompt: configurable instructions + hardcoded JSON guardrail
	analysisGuardrail := AnalysisGuardrail

	req := CompletionRequest{
		System: a.analysisSystemPrompt + analysisGuardrail,
		Messages: []Message{
			{Role: "user", Content: prompt},
		},
	}

	resp, err := a.provider.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("LLM completion failed: %w", err)
	}

	// Parse the JSON response
	analysis, err := parseAnalysisResponse(resp.Content)
	if err != nil {
		// Fallback: return a basic analysis
		return &RiskAnalysis{
			RiskLevel:   "unknown",
			RiskScore:   50,
			Summary:     "Failed to parse LLM response",
			Concerns:    []string{"LLM response parsing failed: " + err.Error()},
			NeedsReview: true,
		}, nil
	}

	return analysis, nil
}

// AnalysisPrompt returns the exact system and user prompt AnalyzeChangelog
// would send for this input, so an external analyzer can reproduce the call.
func (a *ChangelogAnalyzer) AnalysisPrompt(appName string, changelog *domain.ChangelogResponse, memory string) (system, user string) {
	return a.analysisSystemPrompt + AnalysisGuardrail, buildAnalysisPrompt(appName, changelog, a.maxTokens, memory)
}

// buildBasePrompt writes the shared prompt sections: memory, context files, metadata, commits, and diff.
// The header parameter is the task-specific opening line (e.g. "Analyze the deployment risk for application: X").
func buildBasePrompt(sb *strings.Builder, header string, changelog *domain.ChangelogResponse, maxTokens int, memory string) {
	// Add project memory if available
	if memory != "" {
		sb.WriteString("## Project Memory / Instructions\n")
		sb.WriteString(memory)
		sb.WriteString("\n\n---\n\n")
	}

	// Add context files if available
	if len(changelog.ContextFiles) > 0 {
		sb.WriteString("## Project Reference Files\n")
		sb.WriteString("These files are from the project and provide context for evaluating change relevance:\n\n")
		for path, content := range changelog.ContextFiles {
			sb.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", path, content))
		}
		sb.WriteString("---\n\n")
	}

	// This block is metadata for your reasoning, not content for the output: an
	// earlier version labelled the repo id "Environment", and the model dutifully
	// printed the repo id as an environment heading.
	sb.WriteString(header)
	sb.WriteString("Build metadata (context only, do not restate in the output):\n")
	if changelog.SourceRepo != "" {
		sb.WriteString(fmt.Sprintf("- Source repository: %s\n", changelog.SourceRepo))
	}
	if changelog.SourcePath != "" {
		sb.WriteString(fmt.Sprintf("- Path within the repository: %s\n", changelog.SourcePath))
	}
	sb.WriteString(fmt.Sprintf("- Commit range: %s..%s\n", domain.TruncateSHA(changelog.FromCommit), domain.TruncateSHA(changelog.ToCommit)))
	sb.WriteString(fmt.Sprintf("- Total commits: %d\n", changelog.CommitCount))
	sb.WriteString(fmt.Sprintf("- Diff summary: %s\n\n", changelog.DiffSummary))
}

// appendCommitsAndDiff adds commit listing and truncated diff to the prompt.
func appendCommitsAndDiff(sb *strings.Builder, changelog *domain.ChangelogResponse, maxTokens int) {
	sb.WriteString("Commits:\n")
	maxCommits := 50
	if len(changelog.Commits) > maxCommits {
		sb.WriteString(fmt.Sprintf("(Showing first %d of %d commits)\n", maxCommits, len(changelog.Commits)))
	}
	for i, commit := range changelog.Commits {
		if i >= maxCommits {
			break
		}
		sb.WriteString(fmt.Sprintf("- %s: %s (%s)\n", domain.TruncateSHA(commit.SHA), strings.Split(commit.Message, "\n")[0], commit.Author))
	}

	// Add raw diff if available and we have room
	// Rough estimate: 1 token ~= 4 characters
	currentLen := sb.Len()
	maxChars := maxTokens * 4 * 3 / 4 // Leave 25% buffer for response
	remainingChars := maxChars - currentLen

	if changelog.RawDiff != "" && remainingChars > 1000 {
		sb.WriteString("\n\nFile Changes:\n```\n")
		diff := changelog.RawDiff
		if len(diff) > remainingChars-100 {
			diff = diff[:remainingChars-100] + "\n... [diff truncated]\n"
		}
		sb.WriteString(diff)
		sb.WriteString("\n```\n")
	}
}

// buildAnalysisPrompt builds the analysis prompt with a max token limit.
func buildAnalysisPrompt(appName string, changelog *domain.ChangelogResponse, maxTokens int, memory string) string {
	var sb strings.Builder

	header := fmt.Sprintf("Analyze the deployment risk for application: %s\n\n", appName)
	buildBasePrompt(&sb, header, changelog, maxTokens, memory)

	// Add risk indicators from heuristics
	ri := changelog.RiskIndicators
	if ri.Unavailable {
		sb.WriteString("Heuristic Risk Indicators: not computed (source repo unavailable), judge from the commits alone.\n\n")
	} else {
		sb.WriteString("Heuristic Risk Indicators (matched on file paths and added lines, confirm against the diff):\n")
		sb.WriteString(fmt.Sprintf("- Has database migration: %v%s\n", ri.HasMigration, fileList(ri.MigrationFiles)))
		sb.WriteString(fmt.Sprintf("- Has contract change: %v%s\n", ri.HasContractChange, fileList(ri.ContractFiles)))
		sb.WriteString(fmt.Sprintf("- Has config change: %v%s\n", ri.HasConfigChange, fileList(ri.ConfigFiles)))
		if len(ri.EnvVarsAdded) > 0 {
			sb.WriteString(fmt.Sprintf("- Environment variables newly read: %s\n", strings.Join(ri.EnvVarsAdded, ", ")))
		}
		sb.WriteString(fmt.Sprintf("- Touches shared library: %v\n", ri.TouchesSharedLib))
		sb.WriteString(fmt.Sprintf("- Has breaking change marker: %v\n\n", ri.HasBreakingChange))
	}

	appendCommitsAndDiff(&sb, changelog, maxTokens)
	return sb.String()
}

const maxIndicatorFiles = 10

func fileList(files []string) string {
	if len(files) == 0 {
		return ""
	}
	shown := files
	if len(shown) > maxIndicatorFiles {
		shown = shown[:maxIndicatorFiles]
	}
	out := " (" + strings.Join(shown, ", ")
	if extra := len(files) - len(shown); extra > 0 {
		out += fmt.Sprintf(", +%d more", extra)
	}
	return out + ")"
}

// parseAnalysisResponse parses the LLM's JSON response
func parseAnalysisResponse(content string) (*RiskAnalysis, error) {
	// Clean up the response - sometimes LLMs wrap JSON in markdown
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var analysis RiskAnalysis
	if err := json.Unmarshal([]byte(content), &analysis); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	analysis.MemoryUpdates = sanitizeMemoryUpdates(analysis.MemoryUpdates)
	return &analysis, nil
}

// maxMemoryUpdatesPerBuild caps how much one analysis can queue for review. A
// model that ignores the "empty list is expected" instruction should not be
// able to bury the reviewer.
const maxMemoryUpdatesPerBuild = 5

// sanitizeMemoryUpdates drops entries we cannot apply. The memory-update part
// of the response is best-effort: a malformed entry must never cost us the risk
// analysis that came back in the same object.
func sanitizeMemoryUpdates(updates []domain.MemoryUpdate) []domain.MemoryUpdate {
	var out []domain.MemoryUpdate
	for _, u := range updates {
		u.Action = strings.ToLower(strings.TrimSpace(u.Action))
		if u.Action == "" {
			u.Action = domain.MemoryActionAdd
		}
		if !domain.IsValidMemoryAction(u.Action) {
			continue
		}

		u.Section = strings.TrimSpace(u.Section)
		u.Content = strings.TrimSpace(u.Content)
		u.Reason = strings.TrimSpace(u.Reason)

		// Empty content is only meaningful as "remove this whole section".
		if u.Content == "" && !(u.Action == domain.MemoryActionRemove && u.Section != "") {
			continue
		}

		out = append(out, u)
		if len(out) == maxMemoryUpdatesPerBuild {
			break
		}
	}
	return out
}

// GenerateChangelog generates a structured changelog from git changes
func (a *ChangelogAnalyzer) GenerateChangelog(ctx context.Context, appName string, changelog *domain.ChangelogResponse, memory string) (string, error) {
	if a.provider == nil {
		return "", fmt.Errorf("no LLM provider configured")
	}

	// Build the prompt with provider-specific token limits
	prompt := buildChangelogPrompt(appName, changelog, a.maxTokens, memory)

	if a.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] GenerateChangelog prompt length: %d chars (est. %d tokens)\n", len(prompt), len(prompt)/4)
	}

	// Build system prompt: configurable instructions + hardcoded format guardrail
	changelogGuardrail := `

IMPORTANT: Output ONLY plain text in markdown format. Do NOT output JSON.`

	req := CompletionRequest{
		System: a.changelogSystemPrompt + changelogGuardrail,
		Messages: []Message{
			{Role: "user", Content: prompt},
		},
	}

	resp, err := a.provider.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("LLM completion failed: %w", err)
	}

	return strings.TrimSpace(resp.Content), nil
}

// buildChangelogPrompt builds the changelog generation prompt with a max token limit.
func buildChangelogPrompt(appName string, changelog *domain.ChangelogResponse, maxTokens int, memory string) string {
	var sb strings.Builder

	header := fmt.Sprintf("Generate a changelog for application: %s\n\n", appName)
	buildBasePrompt(&sb, header, changelog, maxTokens, memory)
	appendCommitsAndDiff(&sb, changelog, maxTokens)
	return sb.String()
}
