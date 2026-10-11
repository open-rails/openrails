package copilot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// providerStub is a fake provider HTTP endpoint: it records requests and plays
// canned bodies in order (the last repeating).
type providerStub struct {
	status    int
	responses []string
	bodies    []map[string]any
	paths     []string
	headers   []http.Header
}

func newProviderStub(t *testing.T, responses ...string) (*providerStub, string) {
	s := &providerStub{responses: responses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		s.bodies = append(s.bodies, body)
		s.paths = append(s.paths, r.URL.Path)
		s.headers = append(s.headers, r.Header.Clone())
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		_, _ = w.Write([]byte(s.responses[min(len(s.bodies), len(s.responses))-1]))
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func openaiText(text string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + jsonString(text) + `},"finish_reason":"stop"}]}`
}

func openaiToolCalls(calls ...[3]string) string {
	var tcs []map[string]any
	for _, c := range calls {
		tcs = append(tcs, map[string]any{"id": c[0], "type": "function", "function": map[string]any{"name": c[1], "arguments": c[2]}})
	}
	b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": tcs}, "finish_reason": "tool_calls"}}})
	return string(b)
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

const providerToolName = "lookup_catalog"
const providerToolInputSchema = `{"type":"object","properties":{"fields":{"type":"array","items":{"type":"string"}}}}`

func providerToolDef() ToolDef {
	return ToolDef{Name: providerToolName, Description: "Read the catalog", InputSchema: json.RawMessage(providerToolInputSchema)}
}

// A conversation exercising every message kind: text, assistant tool calls, ok and error results.
var toolConversation = []ToolMessage{
	{Role: "user", Text: "which products?"},
	{Role: "assistant", Text: "let me check", ToolCalls: []ToolCall{{ID: "call_1", Name: providerToolName, Input: json.RawMessage(`{"fields":["bogus"]}`)}}},
	{Role: "user", ToolResults: []ToolResult{{ToolUseID: "call_1", Content: "query failed validation", IsError: true}}},
	{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_2", Name: providerToolName, Input: json.RawMessage(`{"fields":["product_count"]}`)}}},
	{Role: "user", ToolResults: []ToolResult{{ToolUseID: "call_2", Content: `{"rows":[[42]]}`}}},
}

func TestOpenAIWireShape(t *testing.T) {
	stub, url := newProviderStub(t, openaiText("  {\"query\":{}}  "), openaiText("final answer"))
	llm := NewOpenAILLM("sk-test", "gpt-test", url+"/openai/v1/") // nested base path, trailing slash

	got, err := llm.CompleteTools(context.Background(), "SYS", nil, []ToolMessage{{Role: "user", Text: "ask catalog"}, {Role: "assistant", Text: "checking"}}, 0)
	require.NoError(t, err)
	require.Equal(t, `{"query":{}}`, got.Text)
	require.Equal(t, "/openai/v1/chat/completions", stub.paths[0])
	require.Equal(t, "Bearer sk-test", stub.headers[0].Get("Authorization"))
	req := stub.bodies[0]
	require.Equal(t, "gpt-test", req["model"])
	require.EqualValues(t, openaiMaxTokens, req["max_completion_tokens"])
	require.NotContains(t, req, "max_tokens", "reasoning models reject legacy max_tokens")
	require.NotContains(t, req, "temperature")
	require.JSONEq(t, `[{"role":"system","content":"SYS"},{"role":"user","content":"ask catalog"},{"role":"assistant","content":"checking"}]`, mustJSON(t, req["messages"]))

	turn, err := llm.CompleteTools(context.Background(), "", []ToolDef{providerToolDef()}, toolConversation, 512)
	require.NoError(t, err)
	require.Equal(t, &ToolTurn{Text: "final answer", StopReason: "stop"}, turn)
	req = stub.bodies[1]
	require.EqualValues(t, 512, req["max_completion_tokens"])
	require.JSONEq(t, mustJSON(t, []map[string]any{{"type": "function", "function": map[string]any{
		"name": providerToolName, "description": providerToolDef().Description, "parameters": json.RawMessage(providerToolInputSchema),
	}}}), mustJSON(t, req["tools"]))
	// Arguments travel as a JSON-encoded string; results are role:"tool" messages; no is_error flag, so errors carry a marker.
	require.JSONEq(t, `[
		{"role":"user","content":"which products?"},
		{"role":"assistant","content":"let me check","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup_catalog","arguments":"{\"fields\":[\"bogus\"]}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ERROR: query failed validation"},
		{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"lookup_catalog","arguments":"{\"fields\":[\"product_count\"]}"}}]},
		{"role":"tool","tool_call_id":"call_2","content":"{\"rows\":[[42]]}"}
	]`, mustJSON(t, req["messages"]), "empty system prompt sends no system message")

	require.Equal(t, "https://api.openai.com/v1", NewOpenAILLM("k", "m", "").baseURL)
}

func TestOpenAIResponseDecoding(t *testing.T) {
	_, url := newProviderStub(t, openaiToolCalls([3]string{"call_a", providerToolName, `{"fields":["product_count"]}`}, [3]string{"call_b", providerToolName, ""}))
	turn, err := NewOpenAILLM("k", "m", url).CompleteTools(context.Background(), "S", nil, []ToolMessage{{Role: "user", Text: "q"}}, 0)
	require.NoError(t, err)
	require.Equal(t, "", turn.Text, "null content is empty text")
	require.Equal(t, "tool_calls", turn.StopReason)
	require.Len(t, turn.ToolCalls, 2)
	require.Equal(t, ToolCall{ID: "call_a", Name: providerToolName, Input: json.RawMessage(`{"fields":["product_count"]}`)}, turn.ToolCalls[0])
	require.JSONEq(t, `{}`, string(turn.ToolCalls[1].Input), "empty arguments normalize to {}")

	_, url = newProviderStub(t, openaiText("   "))
	turn, err = NewOpenAILLM("k", "m", url).CompleteTools(context.Background(), "s", nil, []ToolMessage{{Role: "user", Text: "q"}}, 0)
	require.NoError(t, err)
	require.Empty(t, turn.Text, "the tool loop handles a turn without an answer")
	require.Empty(t, turn.ToolCalls)
}

func TestAnthropicWireShape(t *testing.T) {
	stub, url := newProviderStub(t, `{"content":[{"type":"text","text":"checking "},{"type":"tool_use","id":"tu_1","name":"lookup_catalog","input":{"fields":["active"]}}],"stop_reason":"tool_use"}`)
	llm := NewAnthropicLLM("ak-test", "claude-test", url+"/")

	turn, err := llm.CompleteTools(context.Background(), "SYS", []ToolDef{providerToolDef()}, toolConversation, 0)
	require.NoError(t, err)
	require.Equal(t, "checking", turn.Text)
	require.Equal(t, "tool_use", turn.StopReason)
	require.Len(t, turn.ToolCalls, 1)
	require.Equal(t, "tu_1", turn.ToolCalls[0].ID)
	require.JSONEq(t, `{"fields":["active"]}`, string(turn.ToolCalls[0].Input))

	require.Equal(t, "/v1/messages", stub.paths[0])
	require.Equal(t, "ak-test", stub.headers[0].Get("x-api-key"))
	require.Equal(t, anthropicAPIVersion, stub.headers[0].Get("anthropic-version"))
	req := stub.bodies[0]
	require.EqualValues(t, anthropicMaxTokens, req["max_tokens"], "maxTokens<=0 falls back to the default cap")
	require.Equal(t, "SYS", req["system"])
	require.JSONEq(t, mustJSON(t, []map[string]any{{"name": providerToolName, "description": providerToolDef().Description, "input_schema": json.RawMessage(providerToolInputSchema)}}), mustJSON(t, req["tools"]))
	require.JSONEq(t, `[
		{"role":"user","content":[{"type":"text","text":"which products?"}]},
		{"role":"assistant","content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"call_1","name":"lookup_catalog","input":{"fields":["bogus"]}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"query failed validation","is_error":true}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_2","name":"lookup_catalog","input":{"fields":["product_count"]}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2","content":"{\"rows\":[[42]]}"}]}
	]`, mustJSON(t, req["messages"]))
}

func TestProviderErrorsAreSurfaced(t *testing.T) {
	stub, url := newProviderStub(t, `{"error":{"type":"invalid_request_error","message":"model does not exist"}}`)
	stub.status = http.StatusBadRequest
	for name, llm := range map[string]LLM{"openai": NewOpenAILLM("k", "m", url), "anthropic": NewAnthropicLLM("k", "m", url)} {
		_, err := llm.CompleteTools(context.Background(), "s", nil, []ToolMessage{{Role: "user", Text: "q"}}, 0)
		require.ErrorContains(t, err, "status 400", name)
		require.ErrorContains(t, err, "invalid_request_error: model does not exist", name)
	}
}
