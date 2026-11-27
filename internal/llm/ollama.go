package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OllamaProvider implements the Provider interface for Ollama
type OllamaProvider struct {
	baseURL    string
	model      string
	httpClient *http.Client
	options    map[string]string
}

// ollamaRequest is the request structure for Ollama API
type ollamaRequest struct {
	Model    string         `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaResponse is the response structure from Ollama API
type ollamaResponse struct {
	Model     string `json:"model"`
	Message   ollamaMessage `json:"message"`
	Done      bool   `json:"done"`
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount int    `json:"eval_count"`
}

// NewOllamaProvider creates a new Ollama provider
func NewOllamaProvider(baseURL, model string, options map[string]string) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "llama3.2:1b"
	}

	return &OllamaProvider{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: 300 * time.Second,
		},
		options: options,
	}
}

// Name returns the provider name
func (p *OllamaProvider) Name() string {
	return "ollama"
}

// Complete sends a completion request to Ollama
func (p *OllamaProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	// Build messages array
	messages := make([]ollamaMessage, 0, len(req.Messages)+1)
	
	// Add system message as first message if provided
	if req.System != "" {
		messages = append(messages, ollamaMessage{
			Role:    "system",
			Content: req.System,
		})
	}
	
	// Add user messages
	for _, msg := range req.Messages {
		messages = append(messages, ollamaMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	ollamaOpts := make(map[string]interface{})
	if p.options["temperature"] != "" {
		ollamaOpts["temperature"] = p.options["temperature"]
	}
	if p.options["num_ctx"] != "" {
		ollamaOpts["num_ctx"] = p.options["num_ctx"]
	}

	payload := ollamaRequest{
		Model:    p.model,
		Messages: messages,
		Stream:   false,
		Options:  ollamaOpts,
	}

	url := fmt.Sprintf("%s/api/chat", p.baseURL)

	body, err := doLLMRequest(ctx, p.httpClient, url, nil, payload)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}

	var ollamaResp ollamaResponse
	if err := json.Unmarshal(body, &ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	totalTokens := ollamaResp.PromptEvalCount + ollamaResp.EvalCount

	return &CompletionResponse{
		Content: ollamaResp.Message.Content,
		Tokens:  totalTokens,
	}, nil
}
