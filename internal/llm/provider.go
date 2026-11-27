package llm

import (
	"context"
	"fmt"
)

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest is a request for text completion
type CompletionRequest struct {
	Messages []Message
	System   string
}

// CompletionResponse is the response from a completion request
type CompletionResponse struct {
	Content string
	Tokens  int
	Error   error
}

// Provider defines the interface for LLM providers
type Provider interface {
	// Complete sends a completion request to the LLM
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
	// Name returns the provider name
	Name() string
}

// NewProvider creates a new LLM provider based on configuration
func NewProvider(providerType, apiKey, baseURL, model string, options map[string]string) (Provider, error) {
	switch providerType {
	case "cloudflare":
		return NewCloudflareProvider(apiKey, baseURL, model, options), nil
	case "ollama":
		return NewOllamaProvider(baseURL, model, options), nil
	case "openrouter":
		return NewOpenRouterProvider(apiKey, baseURL, model, options), nil
	case "gemini":
		return NewGeminiProvider(model, options)
	default:
		return nil, fmt.Errorf("unknown LLM provider type: %s", providerType)
	}
}
