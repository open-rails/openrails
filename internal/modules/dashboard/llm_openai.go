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

const (
	// openaiBaseURL includes the version segment — the OpenAI-compatible
	// ecosystem convention (OPENAI_BASE_URL), so llm.base_url plugs straight
	// into Groq/Together/Ollama/vLLM docs values (e.g. http://localhost:11434/v1).
	openaiBaseURL   = "https://api.openai.com/v1"
	openaiMaxTokens = anthropicMaxTokens
)

// OpenAILLM calls the OpenAI Chat Completions API over plain HTTP (no SDK dep
// — one endpoint, one request shape; #761). A custom baseURL serves any
// OpenAI-compatible endpoint. Credentials come from deployment config; this
// package never reads the environment.
type OpenAILLM struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewOpenAILLM builds the production client. baseURL "" = the public API;
// non-empty values must include the version segment (…/v1).
func NewOpenAILLM(apiKey, model, baseURL string) *OpenAILLM {
	if baseURL == "" {
		baseURL = openaiBaseURL
	}
	return &OpenAILLM{
		apiKey:  apiKey,
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// openaiRequest is the /chat/completions body. MaxCompletionTokens, not the
// legacy max_tokens: reasoning-capable models reject max_tokens outright.
// No temperature — parity with the Anthropic client (provider default), and
// current OpenAI reasoning models reject non-default values anyway.
type openaiRequest struct {
	Model               string          `json:"model"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Messages            []openaiMessage `json:"messages"`
	Tools               []openaiTool    `json:"tools,omitempty"`
}

// openaiMessage is one chat turn: system/user/assistant text, an assistant
// turn's tool calls, or (role "tool") one tool result keyed by tool_call_id.
type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiTool struct {
	Type     string            `json:"type"` // always "function"
	Function openaiFunctionDef `json:"function"`
}

type openaiFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // always "function"
	Function openaiFunctionCall `json:"function"`
}

type openaiFunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON-ENCODED STRING (not an object) on the OpenAI wire.
	Arguments string `json:"arguments"`
}

type openaiResponse struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// send posts one chat-completions request body and parses the response envelope.
func (o *OpenAILLM) send(ctx context.Context, reqBody any) (*openaiResponse, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("dashboard llm: read response: %w", err)
	}
	var out openaiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("dashboard llm: non-JSON response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		msg := "unknown error"
		if out.Error != nil {
			msg = fmt.Sprintf("%s: %s", out.Error.Type, out.Error.Message)
		}
		return nil, fmt.Errorf("dashboard llm: openai API error (status %d): %s", resp.StatusCode, msg)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("dashboard llm: response has no choices")
	}
	return &out, nil
}

// Complete sends one chat turn and returns the assistant text.
func (o *OpenAILLM) Complete(ctx context.Context, system string, msgs []LLMMessage) (string, error) {
	req := openaiRequest{Model: o.model, MaxCompletionTokens: openaiMaxTokens}
	if system != "" {
		req.Messages = append(req.Messages, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		req.Messages = append(req.Messages, openaiMessage{Role: m.Role, Content: m.Content})
	}
	out, err := o.send(ctx, req)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("dashboard llm: empty response (finish_reason %q)", out.Choices[0].FinishReason)
	}
	return text, nil
}

// CompleteTools sends one tool-use turn. Mapping notes vs the Anthropic wire:
// tool results are individual role:"tool" messages (not blocks in a user
// turn), tool-call arguments travel as a JSON-encoded string, and there is no
// is_error flag — error results carry an explicit "ERROR: " marker in the
// body so corrective feedback stays unambiguous.
func (o *OpenAILLM) CompleteTools(ctx context.Context, system string, tools []ToolDef, msgs []ToolMessage, maxTokens int) (*ToolTurn, error) {
	if maxTokens <= 0 {
		maxTokens = openaiMaxTokens
	}
	req := openaiRequest{Model: o.model, MaxCompletionTokens: maxTokens}
	if system != "" {
		req.Messages = append(req.Messages, openaiMessage{Role: "system", Content: system})
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, openaiTool{Type: "function", Function: openaiFunctionDef{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
	}
	for _, m := range msgs {
		// Results first, then prose — same order the Anthropic client emits
		// blocks.
		for _, tr := range m.ToolResults {
			content := tr.Content
			if tr.IsError {
				content = "ERROR: " + content
			}
			req.Messages = append(req.Messages, openaiMessage{Role: "tool", ToolCallID: tr.ToolUseID, Content: content})
		}
		if m.Text == "" && len(m.ToolCalls) == 0 {
			continue
		}
		msg := openaiMessage{Role: m.Role, Content: m.Text}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openaiToolCall{
				ID: tc.ID, Type: "function",
				Function: openaiFunctionCall{Name: tc.Name, Arguments: string(tc.Input)},
			})
		}
		req.Messages = append(req.Messages, msg)
	}
	out, err := o.send(ctx, req)
	if err != nil {
		return nil, err
	}
	choice := out.Choices[0]
	turn := &ToolTurn{Text: strings.TrimSpace(choice.Message.Content), StopReason: choice.FinishReason}
	for _, tc := range choice.Message.ToolCalls {
		input := strings.TrimSpace(tc.Function.Arguments)
		if input == "" {
			input = "{}"
		}
		turn.ToolCalls = append(turn.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(input)})
	}
	return turn, nil
}
