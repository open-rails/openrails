package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/metrics"
)

// openaiStub records every request body and plays canned response bodies in
// order (last repeats). No network — the wire shape is the contract (#761).
type openaiStub struct {
	t         *testing.T
	responses []string
	requests  []map[string]any
	raw       [][]byte
	paths     []string
	auths     []string
	status    int
}

func (s *openaiStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(s.t, err)
		var decoded map[string]any
		require.NoError(s.t, json.Unmarshal(body, &decoded))
		s.requests = append(s.requests, decoded)
		s.raw = append(s.raw, body)
		s.paths = append(s.paths, r.URL.Path)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		i := len(s.requests) - 1
		if i >= len(s.responses) {
			i = len(s.responses) - 1
		}
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		_, _ = w.Write([]byte(s.responses[i]))
	})
}

func openaiTextResponse(text string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	})
	return string(b)
}

func openaiToolCallResponse(text string, calls ...[3]string) string {
	tcs := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, map[string]any{
			"id": c[0], "type": "function",
			"function": map[string]any{"name": c[1], "arguments": c[2]},
		})
	}
	msg := map[string]any{"role": "assistant", "tool_calls": tcs}
	if text != "" {
		msg["content"] = text
	} else {
		msg["content"] = nil
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "tool_calls"}},
	})
	return string(b)
}

func newStubLLM(t *testing.T, stub *openaiStub) (*OpenAILLM, *httptest.Server) {
	stub.t = t
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	return NewOpenAILLM("sk-test", "gpt-test", srv.URL+"/v1"), srv
}

func TestOpenAIComplete_WireShapeAndExtraction(t *testing.T) {
	stub := &openaiStub{responses: []string{openaiTextResponse("  {\"query\":{}}  ")}}
	llm, _ := newStubLLM(t, stub)

	got, err := llm.Complete(context.Background(), "SYSTEM PROMPT", []LLMMessage{
		{Role: "user", Content: "make a widget"},
		{Role: "assistant", Content: "bad json"},
		{Role: "user", Content: "fix it"},
	})
	require.NoError(t, err)
	require.Equal(t, `{"query":{}}`, got, "text extracted and trimmed")

	require.Equal(t, []string{"/v1/chat/completions"}, stub.paths, "joins /chat/completions onto the /v1 base")
	require.Equal(t, []string{"Bearer sk-test"}, stub.auths)

	req := stub.requests[0]
	require.Equal(t, "gpt-test", req["model"])
	require.EqualValues(t, openaiMaxTokens, req["max_completion_tokens"])
	require.NotContains(t, req, "max_tokens", "reasoning models reject legacy max_tokens")
	require.NotContains(t, req, "temperature", "provider default — parity with the Anthropic client")
	require.JSONEq(t, `[
		{"role":"system","content":"SYSTEM PROMPT"},
		{"role":"user","content":"make a widget"},
		{"role":"assistant","content":"bad json"},
		{"role":"user","content":"fix it"}
	]`, mustJSON(t, req["messages"]))
}

func TestOpenAIComplete_EmptyResponseErrors(t *testing.T) {
	stub := &openaiStub{responses: []string{openaiTextResponse("   ")}}
	llm, _ := newStubLLM(t, stub)
	_, err := llm.Complete(context.Background(), "s", []LLMMessage{{Role: "user", Content: "q"}})
	require.ErrorContains(t, err, "empty response")
	require.ErrorContains(t, err, "stop")
}

func TestOpenAI_APIErrorSurfaced(t *testing.T) {
	stub := &openaiStub{
		status:    http.StatusBadRequest,
		responses: []string{`{"error":{"type":"invalid_request_error","message":"model does not exist"}}`},
	}
	llm, _ := newStubLLM(t, stub)
	_, err := llm.Complete(context.Background(), "s", []LLMMessage{{Role: "user", Content: "q"}})
	require.ErrorContains(t, err, "status 400")
	require.ErrorContains(t, err, "invalid_request_error: model does not exist")

	_, err = llm.CompleteTools(context.Background(), "s", nil, []ToolMessage{{Role: "user", Text: "q"}}, 0)
	require.ErrorContains(t, err, "status 400")
}

func TestOpenAI_BaseURLJoining(t *testing.T) {
	require.Equal(t, "https://api.openai.com/v1", NewOpenAILLM("k", "m", "").baseURL, "empty = public API")
	require.Equal(t, "http://localhost:11434/v1", NewOpenAILLM("k", "m", "http://localhost:11434/v1/").baseURL, "trailing slash trimmed — no double slash in the join")

	stub := &openaiStub{responses: []string{openaiTextResponse("ok")}}
	stub.t = t
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	llm := NewOpenAILLM("k", "m", srv.URL+"/openai/v1") // Groq-style nested base path
	_, err := llm.Complete(context.Background(), "", []LLMMessage{{Role: "user", Content: "q"}})
	require.NoError(t, err)
	require.Equal(t, []string{"/openai/v1/chat/completions"}, stub.paths)
	require.NotContains(t, stub.requests[0], "messages_system")
	require.JSONEq(t, `[{"role":"user","content":"q"}]`, mustJSON(t, stub.requests[0]["messages"]), "empty system prompt sends no system message")
}

func TestOpenAICompleteTools_RequestShape(t *testing.T) {
	stub := &openaiStub{responses: []string{openaiTextResponse("final answer")}}
	llm, _ := newStubLLM(t, stub)

	msgs := []ToolMessage{
		{Role: "user", Text: "how much revenue?"},
		{Role: "assistant", Text: "let me check", ToolCalls: []ToolCall{
			{ID: "call_1", Name: askToolName, Input: json.RawMessage(`{"measures":["bogus"],"range":{"last":"7d"}}`)},
		}},
		{Role: "user", ToolResults: []ToolResult{
			{ToolUseID: "call_1", Content: "query failed validation. Fix ALL of these errors", IsError: true},
		}},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_2", Name: askToolName, Input: json.RawMessage(`{"measures":["net_revenue"],"range":{"last":"7d"}}`)},
		}},
		{Role: "user", ToolResults: []ToolResult{
			{ToolUseID: "call_2", Content: `{"rows":[[42]]}`},
		}},
	}
	turn, err := llm.CompleteTools(context.Background(), "SYS", []ToolDef{askToolDef()}, msgs, 512)
	require.NoError(t, err)
	require.Equal(t, "final answer", turn.Text)
	require.Empty(t, turn.ToolCalls)
	require.Equal(t, "stop", turn.StopReason)

	req := stub.requests[0]
	require.EqualValues(t, 512, req["max_completion_tokens"], "explicit cap rides the wire")

	// Tool definition: nested {type:"function", function:{name,description,parameters}}.
	require.JSONEq(t, mustJSON(t, []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        askToolName,
			"description": askToolDef().Description,
			"parameters":  json.RawMessage(askToolInputSchema),
		},
	}}), mustJSON(t, req["tools"]))

	// Conversation: system first; assistant tool calls replay arguments as a
	// JSON-encoded STRING; each tool result is its own role:"tool" message
	// keyed by tool_call_id; is_error becomes an explicit ERROR marker.
	require.JSONEq(t, `[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"how much revenue?"},
		{"role":"assistant","content":"let me check","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"run_metrics_query","arguments":"{\"measures\":[\"bogus\"],\"range\":{\"last\":\"7d\"}}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"ERROR: query failed validation. Fix ALL of these errors"},
		{"role":"assistant","tool_calls":[
			{"id":"call_2","type":"function","function":{"name":"run_metrics_query","arguments":"{\"measures\":[\"net_revenue\"],\"range\":{\"last\":\"7d\"}}"}}
		]},
		{"role":"tool","tool_call_id":"call_2","content":"{\"rows\":[[42]]}"}
	]`, mustJSON(t, req["messages"]))
}

func TestOpenAICompleteTools_ToolCallExtractionAndDefaultCap(t *testing.T) {
	stub := &openaiStub{responses: []string{openaiToolCallResponse("",
		[3]string{"call_a", askToolName, `{"measures":["net_revenue"],"range":{"last":"7d"}}`},
		[3]string{"call_b", askToolName, ""}, // empty arguments normalize to {}
	)}}
	llm, _ := newStubLLM(t, stub)

	turn, err := llm.CompleteTools(context.Background(), "SYS", []ToolDef{askToolDef()}, []ToolMessage{{Role: "user", Text: "q"}}, 0)
	require.NoError(t, err)
	require.Equal(t, "", turn.Text, "null content decodes to empty text")
	require.Equal(t, "tool_calls", turn.StopReason)
	require.Len(t, turn.ToolCalls, 2)
	require.Equal(t, "call_a", turn.ToolCalls[0].ID)
	require.Equal(t, askToolName, turn.ToolCalls[0].Name)
	require.JSONEq(t, `{"measures":["net_revenue"],"range":{"last":"7d"}}`, string(turn.ToolCalls[0].Input))
	require.JSONEq(t, `{}`, string(turn.ToolCalls[1].Input))

	require.EqualValues(t, openaiMaxTokens, stub.requests[0]["max_completion_tokens"], "maxTokens<=0 falls back to the default cap")
}

// TestAsk_OpenAIWireRoundTrip drives the whole #756 ask loop through the real
// OpenAI client against a scripted server: invalid tool call → corrective
// errors fed back on the wire (ERROR-marked tool message) → corrected call →
// final answer, with evidence intact.
func TestAsk_OpenAIWireRoundTrip(t *testing.T) {
	stub := &openaiStub{responses: []string{
		openaiToolCallResponse("", [3]string{"call_1", askToolName, `{"measures":["no_such_measure"],"range":{"last":"7d"}}`}),
		openaiToolCallResponse("", [3]string{"call_2", askToolName, askQ1}),
		openaiTextResponse("Net revenue was 42 micros."),
	}}
	llm, _ := newStubLLM(t, stub)

	exec := &fakeExecutor{results: []*metrics.Result{cannedResult("net_revenue", 42)}}
	svc := askService(llm, exec)
	res, err := svc.Ask(context.Background(), "how much revenue?")
	require.NoError(t, err)
	require.Equal(t, "Net revenue was 42 micros.", res.Answer)
	require.Len(t, res.Evidence, 1, "only the executed query becomes evidence")

	require.Len(t, stub.requests, 3)
	// Turn 2 carries the corrective validation errors as an ERROR-marked tool message.
	msgs2 := mustJSON(t, stub.requests[1]["messages"])
	require.Contains(t, msgs2, `"tool_call_id":"call_1"`)
	require.Contains(t, msgs2, "ERROR: query failed validation")
	require.Contains(t, msgs2, "no_such_measure")
	// Turn 3 carries the executed result verbatim for call_2.
	msgs3 := mustJSON(t, stub.requests[2]["messages"])
	require.Contains(t, msgs3, `"tool_call_id":"call_2"`)
	require.Contains(t, msgs3, "net_revenue")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
