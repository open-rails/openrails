package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMMessage is one conversation turn for the generator loop.
type LLMMessage struct {
	Role    string // "user" | "assistant"
	Content string
}

// LLM is the provider seam for natural-language widget generation. The
// production implementation is AnthropicLLM; tests inject a deterministic
// stub. Implementations return the model's text response verbatim.
type LLM interface {
	Complete(ctx context.Context, system string, msgs []LLMMessage) (string, error)
}

const (
	anthropicBaseURL    = "https://api.anthropic.com"
	anthropicAPIVersion = "2023-06-01"
	// generation output is one small JSON object; cap generously.
	anthropicMaxTokens = 2048
)

// AnthropicLLM calls the Anthropic Messages API over plain HTTP (no SDK dep —
// one endpoint, one request shape). Credentials come from deployment config;
// this package never reads the environment.
type AnthropicLLM struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewAnthropicLLM builds the production client. baseURL "" = the public API.
func NewAnthropicLLM(apiKey, model, baseURL string) *AnthropicLLM {
	if baseURL == "" {
		baseURL = anthropicBaseURL
	}
	return &AnthropicLLM{
		apiKey:  apiKey,
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends one Messages-API turn and returns the concatenated text
// content.
func (a *AnthropicLLM) Complete(ctx context.Context, system string, msgs []LLMMessage) (string, error) {
	req := anthropicRequest{Model: a.model, MaxTokens: anthropicMaxTokens, System: system}
	for _, m := range msgs {
		req.Messages = append(req.Messages, anthropicMessage(m))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("dashboard llm: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("dashboard llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("dashboard llm: request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("dashboard llm: read response: %w", err)
	}
	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("dashboard llm: non-JSON response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		msg := "unknown error"
		if out.Error != nil {
			msg = fmt.Sprintf("%s: %s", out.Error.Type, out.Error.Message)
		}
		return "", fmt.Errorf("dashboard llm: anthropic API error (status %d): %s", resp.StatusCode, msg)
	}
	var b strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", fmt.Errorf("dashboard llm: empty response (stop_reason %q)", out.StopReason)
	}
	return text, nil
}
