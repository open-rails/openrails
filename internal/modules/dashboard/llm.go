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

// ToolDef declares one tool for a tool-use turn (#756 ask loop).
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for the tool arguments
}

// ToolCall is one tool invocation the model requested.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is the outcome fed back for one ToolCall.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// ToolMessage is one conversation turn in a tool loop. Exactly one of the
// optional fields is populated per role: user turns carry Text or ToolResults;
// assistant turns replay the model's Text + ToolCalls verbatim.
type ToolMessage struct {
	Role        string // "user" | "assistant"
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ToolTurn is one model response in a tool loop: prose and/or tool calls.
type ToolTurn struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
}

// LLM is the provider seam for the dashboard module's model calls: text
// completion (widget generation, #741) and tool-use turns (metrics Q&A, #756).
// The production implementation is AnthropicLLM; tests inject deterministic
// stubs via Service.SetLLM.
type LLM interface {
	Complete(ctx context.Context, system string, msgs []LLMMessage) (string, error)
	CompleteTools(ctx context.Context, system string, tools []ToolDef, msgs []ToolMessage, maxTokens int) (*ToolTurn, error)
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

type anthropicToolsRequest struct {
	Model     string                  `json:"model"`
	MaxTokens int                     `json:"max_tokens"`
	System    string                  `json:"system,omitempty"`
	Tools     []anthropicToolDef      `json:"tools,omitempty"`
	Messages  []anthropicBlockMessage `json:"messages"`
}

type anthropicToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicBlockMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one content block: text, tool_use (ID/Name/Input) or
// tool_result (ToolUseID/Content/IsError).
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// send posts one Messages-API request body and parses the response envelope.
func (a *AnthropicLLM) send(ctx context.Context, reqBody any) (*anthropicResponse, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: read response: %w", err)
	}
	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("dashboard llm: non-JSON response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		msg := "unknown error"
		if out.Error != nil {
			msg = fmt.Sprintf("%s: %s", out.Error.Type, out.Error.Message)
		}
		return nil, fmt.Errorf("dashboard llm: anthropic API error (status %d): %s", resp.StatusCode, msg)
	}
	return &out, nil
}

// Complete sends one Messages-API turn and returns the concatenated text
// content.
func (a *AnthropicLLM) Complete(ctx context.Context, system string, msgs []LLMMessage) (string, error) {
	req := anthropicRequest{Model: a.model, MaxTokens: anthropicMaxTokens, System: system}
	for _, m := range msgs {
		req.Messages = append(req.Messages, anthropicMessage(m))
	}
	out, err := a.send(ctx, req)
	if err != nil {
		return "", err
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

// CompleteTools sends one tool-use turn: the conversation (with prior
// tool_use/tool_result blocks replayed verbatim) plus the tool definitions,
// returning the model's text and requested tool calls.
func (a *AnthropicLLM) CompleteTools(ctx context.Context, system string, tools []ToolDef, msgs []ToolMessage, maxTokens int) (*ToolTurn, error) {
	if maxTokens <= 0 {
		maxTokens = anthropicMaxTokens
	}
	req := anthropicToolsRequest{Model: a.model, MaxTokens: maxTokens, System: system}
	for _, t := range tools {
		req.Tools = append(req.Tools, anthropicToolDef(t))
	}
	for _, m := range msgs {
		blocks := make([]anthropicBlock, 0, 1+len(m.ToolCalls)+len(m.ToolResults))
		for _, tr := range m.ToolResults {
			blocks = append(blocks, anthropicBlock{Type: "tool_result", ToolUseID: tr.ToolUseID, Content: tr.Content, IsError: tr.IsError})
		}
		if m.Text != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Text})
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Input})
		}
		req.Messages = append(req.Messages, anthropicBlockMessage{Role: m.Role, Content: blocks})
	}
	out, err := a.send(ctx, req)
	if err != nil {
		return nil, err
	}
	turn := &ToolTurn{StopReason: out.StopReason}
	var b strings.Builder
	for _, block := range out.Content {
		switch block.Type {
		case "text":
			b.WriteString(block.Text)
		case "tool_use":
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	turn.Text = strings.TrimSpace(b.String())
	return turn, nil
}
