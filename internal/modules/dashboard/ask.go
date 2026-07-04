package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Ask-loop caps (#756): every cost axis is bounded — tool calls (queries per
// question), response tokens per model turn, model turns, and the total bytes
// of query results fed into the model's context.
const (
	askToolName = "run_metrics_query"
	// askMaxToolCalls caps tool-call ATTEMPTS per question (invalid calls
	// consume budget too, like generate's fixed retry budget).
	askMaxToolCalls = 5
	// askMaxTokens is the per-turn response-token cap.
	askMaxTokens = 2048
	// askMaxTurns bounds the loop: worst case one tool call per turn for the
	// whole budget, one budget-exhausted grace turn, one final answer turn.
	askMaxTurns = askMaxToolCalls + 2
	// askToolResultMaxBytes caps one tool result fed to the model (rows are
	// truncated with an explicit marker; evidence keeps the verbatim result).
	askToolResultMaxBytes = 48 << 10
	// askContextMaxBytes caps the cumulative tool-result bytes fed to the model.
	askContextMaxBytes = 192 << 10
)

// ErrAskNotConfigured: no LLM key or no llm.ask_enabled consent — the endpoint
// answers 501 with a pointed message.
var ErrAskNotConfigured = errors.New("dashboard: metrics ask not configured")

// AskRateLimitedError: the merchant exceeded the per-merchant ask budget.
type AskRateLimitedError struct{ RetryAfter time.Duration }

func (e *AskRateLimitedError) Error() string {
	return fmt.Sprintf("dashboard: ask rate limit exceeded (retry after %s)", e.RetryAfter)
}

// AskNoAnswerError: the model never produced a text answer within the loop
// budget (kept asking for tools).
type AskNoAnswerError struct{ ToolCalls int }

func (e *AskNoAnswerError) Error() string {
	return fmt.Sprintf("dashboard: model produced no answer within the tool budget (%d tool calls)", e.ToolCalls)
}

// AskEvidence is one executed tool call: the query plus its VERBATIM result
// (the embedded metrics.Result flattens to grain/range/columns/rows/...). The
// UI renders these as tables — on-screen numbers come from here, never from
// the model's prose.
type AskEvidence struct {
	Query metrics.Query `json:"query"`
	metrics.Result
}

// AskResult is the POST /v1/merchant/metrics/ask response.
type AskResult struct {
	Answer   string        `json:"answer"`
	Evidence []AskEvidence `json:"evidence"`
}

// Ask answers a natural-language metrics question by letting the model run
// compiler-validated #733 queries as tools (executed through the normal
// metrics service on the caller's RLS-pinned merchant context) and answering
// from the results. Unlike Generate, the model DOES see aggregate query
// results — hence the separate llm.ask_enabled consent.
func (s *Service) Ask(ctx context.Context, question string) (*AskResult, error) {
	if !s.AskConfigured() {
		return nil, ErrAskNotConfigured
	}
	if s.askLimiter != nil {
		merchantID, err := merchant.Require(ctx)
		if err != nil {
			return nil, fmt.Errorf("dashboard ask: no merchant in context: %w", err)
		}
		allowed, retry, err := s.askLimiter.AllowAsk(ctx, uuid.UUID(merchantID).String())
		if err != nil {
			return nil, fmt.Errorf("dashboard ask: rate limiter: %w", err)
		}
		if !allowed {
			return nil, &AskRateLimitedError{RetryAfter: retry}
		}
	}

	system := askSystemPrompt(s.clock.Now().UTC())
	tools := []ToolDef{askToolDef()}
	msgs := []ToolMessage{{Role: "user", Text: question}}
	var evidence []AskEvidence
	attempts := 0
	fedBytes := 0

	for turn := 0; turn < askMaxTurns; turn++ {
		resp, err := s.llm.CompleteTools(ctx, system, tools, msgs, askMaxTokens)
		if err != nil {
			return nil, err
		}
		if len(resp.ToolCalls) == 0 {
			answer := strings.TrimSpace(resp.Text)
			if answer == "" {
				return nil, fmt.Errorf("dashboard ask: empty model response (stop_reason %q)", resp.StopReason)
			}
			return &AskResult{Answer: answer, Evidence: evidence}, nil
		}
		results := make([]ToolResult, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			results = append(results, s.runAskTool(ctx, call, &evidence, &attempts, &fedBytes))
		}
		msgs = append(msgs,
			ToolMessage{Role: "assistant", Text: resp.Text, ToolCalls: resp.ToolCalls},
			ToolMessage{Role: "user", ToolResults: results},
		)
	}
	return nil, &AskNoAnswerError{ToolCalls: attempts}
}

// runAskTool handles one tool call: budget checks, strict decode,
// compiler validation (corrective errors go back to the model, all at once),
// then execution through the metrics service. Executed calls append their
// verbatim result to evidence.
func (s *Service) runAskTool(ctx context.Context, call ToolCall, evidence *[]AskEvidence, attempts, fedBytes *int) ToolResult {
	errResult := func(msg string) ToolResult {
		return ToolResult{ToolUseID: call.ID, Content: msg, IsError: true}
	}
	if call.Name != askToolName {
		return errResult(fmt.Sprintf("unknown tool %q: only %q is available", call.Name, askToolName))
	}
	if *attempts >= askMaxToolCalls {
		return errResult(fmt.Sprintf("tool-call budget exhausted (max %d queries per question) — answer now from the results you already have, or say what you could not determine", askMaxToolCalls))
	}
	if *fedBytes >= askContextMaxBytes {
		return errResult("context budget exhausted — answer now from the results you already have")
	}
	*attempts++

	q, verr := metrics.DecodeQuery(bytes.NewReader(call.Input))
	var plan *metrics.Plan
	if verr == nil {
		plan, verr = metrics.ValidateAt(q, s.clock.Now().UTC())
	}
	if verr != nil {
		feedback, _ := json.Marshal(struct {
			Errors []metrics.FieldError `json:"errors"`
		}{verr.Errors})
		return errResult("query failed validation. Fix ALL of these errors and call the tool again:\n" + string(feedback))
	}
	if s.metrics == nil {
		return errResult("metrics engine unavailable on this deployment")
	}
	result, err := s.metrics.Execute(ctx, plan)
	if err != nil {
		log.WithError(err).Warn("dashboard ask: metrics query execution failed")
		return errResult("query execution failed — try a different query")
	}
	*evidence = append(*evidence, AskEvidence{Query: *q, Result: *result})
	payload := askToolPayload(result)
	*fedBytes += len(payload)
	return ToolResult{ToolUseID: call.ID, Content: payload}
}

// askFeedPayload is the token-lean tool-result document fed to the model.
type askFeedPayload struct {
	Grain        string            `json:"grain,omitempty"`
	Range        metrics.RangeOut  `json:"range"`
	Columns      []metrics.Column  `json:"columns"`
	Rows         [][]any           `json:"rows"`
	CompareRange *metrics.RangeOut `json:"compare_range,omitempty"`
	CompareRows  [][]any           `json:"compare_rows,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"`
	RowsTotal    int               `json:"rows_total,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// askToolPayload serializes a result for the model, truncating rows (with an
// explicit marker — the model must know it is not seeing everything) when the
// per-result feed cap is exceeded. Evidence keeps the full result regardless.
func askToolPayload(res *metrics.Result) string {
	p := askFeedPayload{
		Grain: res.Grain, Range: res.Range, Columns: res.Columns, Rows: res.Rows,
		CompareRange: res.CompareRange, CompareRows: res.CompareRows,
	}
	b, _ := json.Marshal(p)
	for len(b) > askToolResultMaxBytes && len(p.Rows) > 0 {
		p.Rows = p.Rows[:len(p.Rows)/2]
		p.CompareRows = nil
		p.Truncated = true
		p.RowsTotal = len(res.Rows)
		p.Note = "result truncated for context — narrow the query (filters, coarser grain, limit) if you need the rest"
		b, _ = json.Marshal(p)
	}
	return string(b)
}

// askToolInputSchema is the JSON Schema for run_metrics_query's arguments —
// the #733 query body. Vocabulary (measure/dimension/filter names) lives in
// the metrics schema document riding in the system prompt; this only pins the
// SHAPE. The same definition is published for external agents in
// docs/metrics-for-llms.md.
const askToolInputSchema = `{
  "type": "object",
  "properties": {
    "measures": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Measure names from the metrics schema."},
    "by": {"type": "array", "items": {"type": "string"}, "description": "Group-by dimensions; include \"time\" for a time series."},
    "grain": {"type": "string", "enum": ["day", "week", "month", "quarter", "year"], "description": "Time bucket size; required when \"by\" includes \"time\"."},
    "range": {
      "type": "object",
      "properties": {
        "from": {"type": "string", "description": "YYYY-MM-DD (inclusive UTC day) or RFC3339."},
        "to": {"type": "string", "description": "YYYY-MM-DD (inclusive UTC day) or RFC3339."},
        "last": {"type": "string", "description": "Relative trailing window ending today: <N>d|<N>w|<N>m|<N>y, e.g. \"7d\". Mutually exclusive with from/to."}
      },
      "additionalProperties": false
    },
    "filters": {"type": "object", "additionalProperties": {"type": "array", "items": {"type": "string"}}, "description": "Dimension name -> allowed values (OR within a dimension, AND across)."},
    "order": {"type": "array", "items": {"type": "object", "properties": {"measure": {"type": "string"}, "dimension": {"type": "string"}, "dir": {"type": "string", "enum": ["asc", "desc"]}}, "additionalProperties": false}},
    "limit": {"type": "integer", "minimum": 1},
    "compare": {"type": "string", "enum": ["previous_period"], "description": "Also return the same query over the immediately preceding period."}
  },
  "required": ["measures", "range"],
  "additionalProperties": false
}`

func askToolDef() ToolDef {
	return ToolDef{
		Name:        askToolName,
		Description: "Run one OpenRails metrics query (the POST /v1/merchant/metrics/query body) against the merchant's own data. Measures, dimensions, grains and worked examples are defined in the metrics schema in the system prompt — use only names from it. Returns tabular {columns, rows}; time series are zero-filled over the whole range (a zero means genuinely zero). Validation errors come back all at once with corrective context.",
		InputSchema: json.RawMessage(askToolInputSchema),
	}
}

// askSystemPrompt builds the Q&A context: the metrics /schema document (the
// one source of truth, #733) plus the answering rules — including the honesty
// path: what the schema cannot answer is named as missing, never improvised.
func askSystemPrompt(now time.Time) string {
	schemaJSON, _ := json.Marshal(metrics.Schema())
	return fmt.Sprintf(`You answer a merchant's questions about their billing and revenue data by querying the OpenRails metrics API with the %s tool, then answering from the results.

Today's date (UTC): %s.

The complete metrics schema (measures, dimensions, grains, query shape, worked examples) is the JSON below. Use ONLY measure/dimension/grain/filter names that appear in it; the "examples" array pairs intents with correct queries — imitate them.

<schema>
%s
</schema>

Rules:
- Query first, answer second: every number in your answer must come from a tool result. NEVER improvise, estimate or extrapolate numbers.
- If the schema cannot answer the question, say plainly what is missing (the schema's descriptions state what does not exist) — do not answer with unrelated numbers.
- You have at most %d queries per question — plan deliberately; prefer one query with a group-by over many single queries.
- If a tool call returns validation errors, fix ALL of them in the next call.
- Money values in results are micros (1,000,000 micros = 1 currency unit); state amounts in whole currency units with the currency.
- Answer concisely with the numbers you retrieved. The UI shows every query result as a table next to your answer, so summarize and interpret — do not repeat whole tables in prose.`,
		askToolName, now.Format("2006-01-02"), schemaJSON, askMaxToolCalls)
}
