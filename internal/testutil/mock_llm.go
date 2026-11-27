package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"deploy-api/internal/llm"
)

// MockProvider is a mock LLM provider for testing
type MockProvider struct {
	mu sync.Mutex

	// Responses to return
	Response    *llm.CompletionResponse
	ResponseErr error

	// Recorded calls for verification
	Calls []llm.CompletionRequest

	// Custom response generator based on input
	ResponseGenerator func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error)
}

// NewMockProvider creates a new mock LLM provider
func NewMockProvider() *MockProvider {
	return &MockProvider{
		Calls: make([]llm.CompletionRequest, 0),
		Response: &llm.CompletionResponse{
			Content: `{
				"risk_level": "low",
				"risk_score": 25,
				"summary": "Mock analysis: Low risk changes with minor updates",
				"concerns": ["Minor dependency updates"],
				"recommendations": ["Proceed with deployment"],
				"needs_review": false
			}`,
			Tokens: 100,
		},
	}
}

// Complete implements the llm.Provider interface
func (m *MockProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Calls = append(m.Calls, req)

	if m.ResponseErr != nil {
		return nil, m.ResponseErr
	}

	if m.ResponseGenerator != nil {
		return m.ResponseGenerator(ctx, req)
	}

	return m.Response, nil
}

// Name returns the provider name
func (m *MockProvider) Name() string {
	return "mock"
}

// SetResponse sets a fixed response for all calls
func (m *MockProvider) SetResponse(response *llm.CompletionResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Response = response
	m.ResponseGenerator = nil
}

// SetResponseContent sets just the content string
func (m *MockProvider) SetResponseContent(content string) {
	m.SetResponse(&llm.CompletionResponse{
		Content: content,
		Tokens:  100,
	})
}

// SetResponseObject sets a response from a llm.RiskAnalysis
func (m *MockProvider) SetResponseObject(analysis *llm.RiskAnalysis) {
	data, _ := json.Marshal(analysis)
	m.SetResponseContent(string(data))
}

// SetError sets an error to return
func (m *MockProvider) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ResponseErr = err
}

// SetResponseGenerator sets a custom response generator
func (m *MockProvider) SetResponseGenerator(fn func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ResponseGenerator = fn
}

// GetCalls returns all recorded calls
func (m *MockProvider) GetCalls() []llm.CompletionRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]llm.CompletionRequest, len(m.Calls))
	copy(result, m.Calls)
	return result
}

// Reset clears all recorded calls
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = make([]llm.CompletionRequest, 0)
	m.ResponseErr = nil
}

// IntelligentMockProvider creates a provider that analyzes commit messages
type IntelligentMockProvider struct {
	*MockProvider
}

// NewIntelligentMockProvider creates an intelligent mock that analyzes commit messages
func NewIntelligentMockProvider() *IntelligentMockProvider {
	base := NewMockProvider()

	base.SetResponseGenerator(func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
		analysis := analyzeRequestIntelligently(req)
		data, _ := json.Marshal(analysis)
		return &llm.CompletionResponse{
			Content: string(data),
			Tokens:  len(data) / 4, // Rough estimate
		}, nil
	})

	return &IntelligentMockProvider{MockProvider: base}
}

// analyzeRequestIntelligently analyzes commit messages in the request
func analyzeRequestIntelligently(req llm.CompletionRequest) *llm.RiskAnalysis {
	// Extract content from messages
	var fullContent string
	for _, msg := range req.Messages {
		fullContent += msg.Content + "\n"
	}

	commits := extractCommitsFromContent(fullContent)

	if len(commits) == 0 {
		return &llm.RiskAnalysis{
			RiskLevel:       "low",
			RiskScore:       10,
			Summary:         "No commits to analyze",
			Concerns:        []string{},
			Recommendations: []string{"Safe to deploy"},
			NeedsReview:     false,
		}
	}

	var riskScore int
	var concerns []string
	var recommendations []string

	highRiskKeywords := map[string]int{
		"breaking": 18, "major": 12, "critical": 15, "security": 12,
		"vulnerability": 12, "cve": 12, "panic": 12, "crash": 12,
	}
	mediumRiskKeywords := map[string]int{
		"refactor": 10, "database": 10, "api": 8, "schema": 8,
		"migration": 10, "config": 6, "auth": 8,
	}

	for _, commit := range commits {
		msg := strings.ToLower(commit)

		for kw, score := range highRiskKeywords {
			if strings.Contains(msg, kw) {
				riskScore += score
				concerns = append(concerns, fmt.Sprintf("Found '%s' in: %s", kw, commit))
			}
		}

		for kw, score := range mediumRiskKeywords {
			if strings.Contains(msg, kw) {
				riskScore += score
			}
		}
	}

	// Cap risk score at 100
	if riskScore > 100 {
		riskScore = 100
	}

	// Determine risk level
	riskLevel := "low"
	if riskScore >= 70 {
		riskLevel = "critical"
	} else if riskScore >= 50 {
		riskLevel = "high"
	} else if riskScore >= 25 {
		riskLevel = "medium"
	}

	recommendations = generateRecommendations(riskLevel)

	return &llm.RiskAnalysis{
		RiskLevel:       riskLevel,
		RiskScore:       riskScore,
		Summary:         fmt.Sprintf("Analyzed %d commits, risk score: %d/100", len(commits), riskScore),
		Concerns:        concerns,
		Recommendations: recommendations,
		NeedsReview:     riskScore >= 50,
	}
}

func extractCommitsFromContent(content string) []string {
	var commits []string
	lines := strings.Split(content, "\n")
	inCommits := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Look for commit section (Commits: or CHANGELOG)
		if strings.Contains(trimmed, "Commits:") || strings.Contains(trimmed, "CHANGELOG") ||
			strings.Contains(trimmed, "Commit Messages") || strings.Contains(trimmed, "Recent commits") {
			inCommits = true
			continue
		}

		// Stop if we hit another section after collecting commits
		if inCommits && len(commits) > 0 {
			if strings.Contains(trimmed, "File Changes:") || 
			   strings.Contains(trimmed, "Analysis:") ||
			   strings.Contains(trimmed, "---") ||
			   strings.HasPrefix(trimmed, "##") {
				break
			}
		}

		if inCommits && trimmed != "" {
			// The prompt format is: "- <sha>: <message> (<author>)"
			// Extract the commit message from this format
			if strings.HasPrefix(trimmed, "-") {
				// Parse: "- bbb222ccc: Refactor database queries (Author)"
				commitText := strings.TrimPrefix(trimmed, "-")
				commitText = strings.TrimSpace(commitText)
				
				// Extract message part between ": " and " ("
				if idx := strings.Index(commitText, ": "); idx > 0 {
					afterSha := commitText[idx+2:]
					// Remove "(Author)" suffix
					if parenIdx := strings.LastIndex(afterSha, " ("); parenIdx > 0 {
						commits = append(commits, afterSha[:parenIdx])
					} else {
						commits = append(commits, afterSha)
					}
				} else {
					commits = append(commits, commitText)
				}
			}
		}
	}

	return commits
}

func generateRecommendations(riskLevel string) []string {
	switch riskLevel {
	case "critical":
		return []string{
			"DO NOT DEPLOY without manual review",
			"Schedule deployment during low-traffic hours",
			"Have rollback plan ready",
		}
	case "high":
		return []string{
			"Review with team lead before deploying",
			"Test thoroughly in staging first",
			"Monitor closely after deployment",
		}
	case "medium":
		return []string{
			"Standard deployment process",
			"Monitor for any issues",
		}
	default:
		return []string{
			"Safe to deploy",
			"Proceed with standard process",
		}
	}
}

// Ensure MockProvider implements llm.Provider interface
var _ llm.Provider = (*MockProvider)(nil)
