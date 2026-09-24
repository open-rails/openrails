package dashboard

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

// A conversation exercising every message kind: text, assistant tool calls, ok and error results.
var toolConversation = []ToolMessage{
	{Role: "user", Text: "how much revenue?"},
	{Role: "assistant", Text: "let me check", ToolCalls: []ToolCall{{ID: "call_1", Name: askToolName, Input: json.RawMessage(`{"measures":["bogus"]}`)}}},
	{Role: "user", ToolResults: []ToolResult{{ToolUseID: "call_1", Content: "query failed validation", IsError: true}}},
	{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_2", Name: askToolName, Input: json.RawMessage(`{"measures":["net_revenue"]}`)}}},
	{Role: "user", ToolResults: []ToolResult{{ToolUseID: "call_2", Content: `{"rows":[[42]]}`}}},
}

func TestOpenAIWireShape(t *testing.T) {
	stub, url := newProviderStub(t, openaiText("  {\"query\":{}}  "), openaiText("final answer"))
	llm := NewOpenAILLM("sk-test", "gpt-test", url+"/openai/v1/") // nested base path, trailing slash

	got, err := llm.Complete(context.Background(), "SYS", []LLMMessage{{Role: "user", Content: "make a widget"}, {Role: "assistant", Content: "bad"}})
	require.NoError(t, err)
	require.Equal(t, `{"query":{}}`, got)
	require.Equal(t, "/openai/v1/chat/completions", stub.paths[0])
	require.Equal(t, "Bearer sk-test", stub.headers[0].Get("Authorization"))
	req := stub.bodies[0]
	require.Equal(t, "gpt-test", req["model"])
	require.EqualValues(t, openaiMaxTokens, req["max_completion_tokens"])
	require.NotContains(t, req, "max_tokens", "reasoning models reject legacy max_tokens")
	require.NotContains(t, req, "temperature")
	require.JSONEq(t, `[{"role":"system","content":"SYS"},{"role":"user","content":"make a widget"},{"role":"assistant","content":"bad"}]`, mustJSON(t, req["messages"]))

	turn, err := llm.CompleteTools(context.Background(), "", []ToolDef{askToolDef()}, toolConversation, 512)
	require.NoError(t, err)
	require.Equal(t, &ToolTurn{Text: "final answer", StopReason: "stop"}, turn)
	req = stub.bodies[1]
	require.EqualValues(t, 512, req["max_completion_tokens"])
	require.JSONEq(t, mustJSON(t, []map[string]any{{"type": "function", "function": map[string]any{
		"name": askToolName, "description": askToolDef().Description, "parameters": json.RawMessage(askToolInputSchema),
	}}}), mustJSON(t, req["tools"]))
	// Arguments travel as a JSON-encoded string; results are role:"tool" messages; no is_error flag, so errors carry a marker.
	require.JSONEq(t, `[
		{"role":"user","content":"how much revenue?"},
		{"role":"assistant","content":"let me check","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_metrics_query","arguments":"{\"measures\":[\"bogus\"]}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ERROR: query failed validation"},
		{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"run_metrics_query","arguments":"{\"measures\":[\"net_revenue\"]}"}}]},
		{"role":"tool","tool_call_id":"call_2","content":"{\"rows\":[[42]]}"}
	]`, mustJSON(t, req["messages"]), "empty system prompt sends no system message")

	require.Equal(t, "https://api.openai.com/v1", NewOpenAILLM("k", "m", "").baseURL)
}

func TestOpenAIResponseDecoding(t *testing.T) {
	_, url := newProviderStub(t, openaiToolCalls([3]string{"call_a", askToolName, `{"measures":["net_revenue"]}`}, [3]string{"call_b", askToolName, ""}))
	turn, err := NewOpenAILLM("k", "m", url).CompleteTools(context.Background(), "S", nil, []ToolMessage{{Role: "user", Text: "q"}}, 0)
	require.NoError(t, err)
	require.Equal(t, "", turn.Text, "null content is empty text")
	require.Equal(t, "tool_calls", turn.StopReason)
	require.Len(t, turn.ToolCalls, 2)
	require.Equal(t, ToolCall{ID: "call_a", Name: askToolName, Input: json.RawMessage(`{"measures":["net_revenue"]}`)}, turn.ToolCalls[0])
	require.JSONEq(t, `{}`, string(turn.ToolCalls[1].Input), "empty arguments normalize to {}")

	_, url = newProviderStub(t, openaiText("   "))
	_, err = NewOpenAILLM("k", "m", url).Complete(context.Background(), "s", []LLMMessage{{Role: "user", Content: "q"}})
	require.ErrorContains(t, err, `empty response (finish_reason "stop")`)
}

func TestAnthropicWireShape(t *testing.T) {
	stub, url := newProviderStub(t, `{"content":[{"type":"text","text":"checking "},{"type":"tool_use","id":"tu_1","name":"run_metrics_query","input":{"measures":["mrr"]}}],"stop_reason":"tool_use"}`)
	llm := NewAnthropicLLM("ak-test", "claude-test", url+"/")

	turn, err := llm.CompleteTools(context.Background(), "SYS", []ToolDef{askToolDef()}, toolConversation, 0)
	require.NoError(t, err)
	require.Equal(t, "checking", turn.Text)
	require.Equal(t, "tool_use", turn.StopReason)
	require.Len(t, turn.ToolCalls, 1)
	require.Equal(t, "tu_1", turn.ToolCalls[0].ID)
	require.JSONEq(t, `{"measures":["mrr"]}`, string(turn.ToolCalls[0].Input))

	require.Equal(t, "/v1/messages", stub.paths[0])
	require.Equal(t, "ak-test", stub.headers[0].Get("x-api-key"))
	require.Equal(t, anthropicAPIVersion, stub.headers[0].Get("anthropic-version"))
	req := stub.bodies[0]
	require.EqualValues(t, anthropicMaxTokens, req["max_tokens"], "maxTokens<=0 falls back to the default cap")
	require.Equal(t, "SYS", req["system"])
	require.JSONEq(t, mustJSON(t, []map[string]any{{"name": askToolName, "description": askToolDef().Description, "input_schema": json.RawMessage(askToolInputSchema)}}), mustJSON(t, req["tools"]))
	require.JSONEq(t, `[
		{"role":"user","content":[{"type":"text","text":"how much revenue?"}]},
		{"role":"assistant","content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"call_1","name":"run_metrics_query","input":{"measures":["bogus"]}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"query failed validation","is_error":true}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_2","name":"run_metrics_query","input":{"measures":["net_revenue"]}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2","content":"{\"rows\":[[42]]}"}]}
	]`, mustJSON(t, req["messages"]))
}

func TestProviderErrorsAreSurfaced(t *testing.T) {
	stub, url := newProviderStub(t, `{"error":{"type":"invalid_request_error","message":"model does not exist"}}`)
	stub.status = http.StatusBadRequest
	for name, llm := range map[string]LLM{"openai": NewOpenAILLM("k", "m", url), "anthropic": NewAnthropicLLM("k", "m", url)} {
		_, err := llm.Complete(context.Background(), "s", []LLMMessage{{Role: "user", Content: "q"}})
		require.ErrorContains(t, err, "status 400", name)
		require.ErrorContains(t, err, "invalid_request_error: model does not exist", name)
		_, err = llm.CompleteTools(context.Background(), "s", nil, []ToolMessage{{Role: "user", Text: "q"}}, 0)
		require.ErrorContains(t, err, "status 400", name)
	}
}

// The whole ask loop through the real OpenAI client: invalid call, corrective
// ERROR-marked feedback on the wire, corrected call, answer.
func TestAskOverOpenAIWire(t *testing.T) {
	stub, url := newProviderStub(t,
		openaiToolCalls([3]string{"call_1", askToolName, `{"measures":["no_such_measure"],"range":{"last":"7d"}}`}),
		openaiToolCalls([3]string{"call_2", askToolName, askQ1}),
		openaiText("Net revenue was 42."),
	)
	res, err := ask(t, NewOpenAILLM("k", "m", url), &fakeExecutor{value: 42})
	require.NoError(t, err)
	require.Equal(t, "Net revenue was 42.", res.Answer)
	require.Len(t, res.Evidence, 1, "only the executed query is evidence")
	require.Len(t, stub.bodies, 3)
	turn2 := mustJSON(t, stub.bodies[1]["messages"])
	require.Contains(t, turn2, `"tool_call_id":"call_1"`)
	require.Contains(t, turn2, "ERROR: query failed validation")
	require.Contains(t, turn2, "no_such_measure")
	require.Contains(t, mustJSON(t, stub.bodies[2]["messages"]), `"tool_call_id":"call_2"`)
}
