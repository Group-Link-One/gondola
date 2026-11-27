package llm

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/genai"
)

// GeminiProvider implements the Provider interface for Google Gemini
// Token limits for Gemini 2.0 Flash:
// - Input: 1,048,576 tokens (~4M characters)
// - Output: 8,192 default, up to 65,536 with configuration
type GeminiProvider struct {
	client     *genai.Client
	model      string
	debug      bool
	maxTokens  int // Max output tokens
}

// NewGeminiProvider creates a new Gemini provider
// API key is read from GEMINI_API_KEY environment variable
// Token limits: Input ~1M tokens, Output 8K-65K depending on config
func NewGeminiProvider(model string, options map[string]string) (*GeminiProvider, error) {
	if model == "" {
		model = "gemini-2.0-flash" // Default model
	}

	debug := os.Getenv("DEBUG") == "true" || os.Getenv("DEBUG_LLM") == "true"

	// Default max tokens: 8192 (Gemini default), can go up to 65536
	maxTokens := 8192
	if options["max_tokens"] != "" {
		fmt.Sscanf(options["max_tokens"], "%d", &maxTokens)
		if maxTokens > 65536 {
			maxTokens = 65536 // Hard cap for Gemini 2.0 Flash
		}
	}

	ctx := context.Background()
	client, err := genai.NewClient(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	if debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] GeminiProvider initialized:\n")
		fmt.Fprintf(os.Stderr, "[DEBUG]   Model: %s\n", model)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Max Output Tokens: %d\n", maxTokens)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Thinking Level: %s\n", os.Getenv("GEMINI_THINKING_LEVEL"))
		fmt.Fprintf(os.Stderr, "[DEBUG]   Include Thoughts: %v\n", os.Getenv("GEMINI_INCLUDE_THOUGHTS") == "true")
		fmt.Fprintf(os.Stderr, "[DEBUG]   API Key source: GEMINI_API_KEY env\n")
	}

	return &GeminiProvider{
		client:    client,
		model:     model,
		debug:     debug,
		maxTokens: maxTokens,
	}, nil
}

// Name returns the provider name
func (p *GeminiProvider) Name() string {
	return "gemini"
}

// Complete sends a completion request to Gemini
// Respects max input size: ~1M tokens (~4M characters)
func (p *GeminiProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	// Build the prompt - combine system and user messages
	var prompt string
	if req.System != "" {
		prompt = req.System + "\n\n"
	}
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			prompt += msg.Content
		}
	}

	// Configure thinking level (MINIMAL, LOW, MEDIUM, HIGH)
	thinkingLevelStr := os.Getenv("GEMINI_THINKING_LEVEL")
	if thinkingLevelStr == "" {
		thinkingLevelStr = "LOW" // Default thinking level
	}
	includeThoughts := os.Getenv("GEMINI_INCLUDE_THOUGHTS") == "true"

	// Gemini 2.0 Flash: 1,048,576 tokens ≈ 4M characters
	// Leave some buffer for safety
	const maxInputChars = 4 * 1000000 * 9 / 10 // 90% of 4M
	if len(prompt) > maxInputChars {
		prompt = prompt[:maxInputChars] + "\n\n[Content truncated due to length limits]"
	}

	if p.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Gemini API Request:\n")
		fmt.Fprintf(os.Stderr, "[DEBUG]   Model: %s\n", p.model)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Prompt length: %d chars (est. %d tokens)\n", len(prompt), len(prompt)/4)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Max Output Tokens: %d\n", p.maxTokens)
		fmt.Fprintf(os.Stderr, "[DEBUG]   Thinking Level: %s\n", thinkingLevelStr)
		showFull := os.Getenv("DEBUG_FULL") == "true"
		if showFull {
			fmt.Fprintf(os.Stderr, "[DEBUG]   Prompt (FULL): %s\n", prompt)
		} else {
			preview := prompt
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			fmt.Fprintf(os.Stderr, "[DEBUG]   Prompt preview: %s\n", preview)
		}
	}

	// Build the full prompt with system instructions
	fullPrompt := req.System + "\n\n" + prompt

	config := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{
			ThinkingLevel:   genai.ThinkingLevel(thinkingLevelStr),
			IncludeThoughts: includeThoughts,
		},
	}

	result, err := p.client.Models.GenerateContent(
		ctx,
		p.model,
		genai.Text(fullPrompt),
		config,
	)
	if err != nil {
		return nil, fmt.Errorf("Gemini API error: %w", err)
	}

	content := result.Text()

	if p.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Gemini API Response:\n")
		fmt.Fprintf(os.Stderr, "[DEBUG]   Content: %s\n", content)
	}

	// Note: We no longer enforce JSON validation here.
	// The caller is responsible for validating the response format based on their system prompt.
	// This allows the same provider to be used for both JSON (risk analysis) and plain text (changelog) outputs.

	return &CompletionResponse{
		Content: content,
		Tokens:  0, // Gemini doesn't expose token count easily in this SDK
	}, nil
}
