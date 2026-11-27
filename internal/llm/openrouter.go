package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OpenRouterProvider implements the Provider interface for OpenRouter
type OpenRouterProvider struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

// openRouterRequest is the request structure for OpenRouter API
type openRouterRequest struct {
	Model    string               `json:"model"`
	Messages []openRouterMessage  `json:"messages"`
}

type openRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openRouterResponse is the response structure from OpenRouter API
type openRouterResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message openRouterMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// NewOpenRouterProvider creates a new OpenRouter provider
func NewOpenRouterProvider(apiKey, baseURL, model string, options map[string]string) *OpenRouterProvider {
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	if model == "" {
		model = "meta-llama/llama-3.2-1b-instruct"
	}

	return &OpenRouterProvider{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Name returns the provider name
func (p *OpenRouterProvider) Name() string {
	return "openrouter"
}

// Complete sends a completion request to OpenRouter
func (p *OpenRouterProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	// Build messages array
	messages := make([]openRouterMessage, 0, len(req.Messages)+1)
	
	// Add system message if provided
	if req.System != "" {
		messages = append(messages, openRouterMessage{
			Role:    "system",
			Content: req.System,
		})
	}
	
	// Add user messages
	for _, msg := range req.Messages {
		messages = append(messages, openRouterMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	payload := openRouterRequest{
		Model:    p.model,
		Messages: messages,
	}

	url := fmt.Sprintf("%s/chat/completions", p.baseURL)
	headers := map[string]string{"Authorization": "Bearer " + p.apiKey}

	body, err := doLLMRequest(ctx, p.httpClient, url, headers, payload)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}

	var orResp openRouterResponse
	if err := json.Unmarshal(body, &orResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if orResp.Error != nil {
		return nil, fmt.Errorf("openrouter API error: %s", orResp.Error.Message)
	}

	if len(orResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	return &CompletionResponse{
		Content: orResp.Choices[0].Message.Content,
		Tokens:  orResp.Usage.TotalTokens,
	}, nil
}
