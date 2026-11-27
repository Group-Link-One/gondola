package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// CloudflareProvider implements the Provider interface for Cloudflare AI
type CloudflareProvider struct {
	apiKey     string
	accountID  string
	model      string
	baseURL    string
	httpClient *http.Client
	maxTokens  int
	debug      bool
}

// cloudflareRequest is the request structure for Cloudflare AI API
type cloudflareRequest struct {
	Messages       []cloudflareMessage    `json:"messages"`
	ResponseFormat map[string]interface{} `json:"response_format,omitempty"`
}

type cloudflareMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// cloudflareResponse is the response structure from Cloudflare AI API
type cloudflareResponse struct {
	Result struct {
		Response json.RawMessage `json:"response"`
	} `json:"result"`
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// NewCloudflareProvider creates a new Cloudflare AI provider
func NewCloudflareProvider(apiKey, accountID, model string, options map[string]string) *CloudflareProvider {
	if model == "" {
		model = "@cf/meta/llama-3.1-8b-instruct-fast"
	}

	maxTokens := 60000
	if options["max_tokens"] != "" {
		// Parse max tokens if provided
		fmt.Sscanf(options["max_tokens"], "%d", &maxTokens)
	}

	debug := os.Getenv("DEBUG") == "true" || os.Getenv("DEBUG_LLM") == "true"

	if debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] CloudflareProvider initialized:\n")
		fmt.Fprintf(os.Stderr, "[DEBUG]   Account ID: %s\n", accountID)
		fmt.Fprintf(os.Stderr, "[DEBUG]   API Key (first 10 chars): %s...\n", apiKey[:min(10, len(apiKey))])
		fmt.Fprintf(os.Stderr, "[DEBUG]   Model: %s\n", model)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Max Tokens: %d\n", maxTokens)
	}

	return &CloudflareProvider{
		apiKey:    apiKey,
		accountID: accountID,
		model:     model,
		baseURL:   "https://api.cloudflare.com/client/v4",
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		maxTokens: maxTokens,
		debug:     debug,
	}
}

// Name returns the provider name
func (p *CloudflareProvider) Name() string {
	return "cloudflare"
}

// Complete sends a completion request to Cloudflare AI
func (p *CloudflareProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	// Build messages array
	messages := make([]cloudflareMessage, 0, len(req.Messages)+1)

	// Add system message if provided
	if req.System != "" {
		messages = append(messages, cloudflareMessage{
			Role:    "system",
			Content: req.System,
		})
	}

	// Add user messages
	for _, msg := range req.Messages {
		messages = append(messages, cloudflareMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	// Build JSON schema for response format
	responseFormat := map[string]interface{}{
		"type": "json_schema",
		"json_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"risk_level": map[string]interface{}{
					"type": "string",
					"enum": []string{"low", "medium", "high", "critical"},
				},
				"risk_score": map[string]interface{}{
					"type":    "integer",
					"minimum": 0,
					"maximum": 100,
				},
				"summary": map[string]interface{}{
					"type": "string",
				},
				"concerns": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": "string",
					},
				},
				"recommendations": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": "string",
					},
				},
				"needs_review": map[string]interface{}{
					"type": "boolean",
				},
			},
			"required": []string{"risk_level", "risk_score", "summary", "concerns", "recommendations", "needs_review"},
		},
	}

	payload := cloudflareRequest{
		Messages:       messages,
		ResponseFormat: responseFormat,
	}

	url := fmt.Sprintf("%s/accounts/%s/ai/run/%s", p.baseURL, p.accountID, p.model)
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}

	if p.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Cloudflare API Request:\n")
		fmt.Fprintf(os.Stderr, "[DEBUG]   URL: %s\n", url)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Authorization: Bearer %s...\n", p.apiKey[:min(10, len(p.apiKey))])
		fmt.Fprintf(os.Stderr, "[DEBUG]   Messages count: %d\n", len(messages))
		showFull := os.Getenv("DEBUG_FULL") == "true"
		for i, m := range messages {
			content := m.Content
			if !showFull && len(content) > 200 {
				content = content[:200] + "..."
			}
			fmt.Fprintf(os.Stderr, "[DEBUG]   Message %d (%s): %s\n", i, m.Role, content)
		}
	}

	body, err := doLLMRequest(ctx, p.httpClient, url, headers, payload)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}

	if p.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Cloudflare API Response Body: %s\n", string(body))
	}

	var cfResp cloudflareResponse
	if err := json.Unmarshal(body, &cfResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if !cfResp.Success && len(cfResp.Errors) > 0 {
		return nil, fmt.Errorf("cloudflare API error: %s", cfResp.Errors[0].Message)
	}

	// Return the JSON response directly
	return &CompletionResponse{
		Content: string(cfResp.Result.Response),
		Tokens:  0, // Cloudflare doesn't return token count directly
	}, nil
}
