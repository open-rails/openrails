package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/metrics"
)

// generateMaxAttempts = first candidate + 2 corrective retries.
const generateMaxAttempts = 3

// ErrLLMNotConfigured: the deployment has no LLM credentials — the endpoint
// answers 501 and the console hides the NL box (fail-closed feature gate).
var ErrLLMNotConfigured = errors.New("dashboard: llm not configured")

// GenerateInvalidError: the LLM never produced a valid query within the retry
// budget; Errors carries the last round of corrective validation errors.
type GenerateInvalidError struct {
	Errors []metrics.FieldError
}

func (e *GenerateInvalidError) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Message
	}
	return "dashboard: generated query failed validation: " + strings.Join(msgs, "; ")
}

// GenerateResult is a VALIDATED widget suggestion: the query already passed
// the metrics compiler. The UI previews it live before the merchant saves.
type GenerateResult struct {
	Query metrics.Query `json:"query"`
	Title string        `json:"title"`
	Viz   string        `json:"viz"`
}

// Generate turns a natural-language prompt into a validated widget. The LLM
// only ever sees the metrics /schema document and the prompt — never merchant
// data — and its output is validated by the same compiler that gates client
// queries; on errors, the compiler's corrective messages are fed back verbatim
// (max 2 retries). It never executes queries.
//
// base, when non-nil, is an existing (already validated) widget query the
// prompt refines — it rides into the user turn as "current query to modify"
// so instructions like "make it weekly" edit rather than start over.
func (s *Service) Generate(ctx context.Context, prompt string, base *metrics.Query) (*GenerateResult, error) {
	if s.llm == nil {
		return nil, ErrLLMNotConfigured
	}
	system := generateSystemPrompt(s.clock.Now().UTC())
	userTurn := prompt
	if base != nil {
		baseJSON, err := json.Marshal(base)
		if err != nil {
			return nil, fmt.Errorf("dashboard: marshal base query: %w", err)
		}
		userTurn = "Current query to modify:\n" + string(baseJSON) + "\n\nInstruction: " + prompt
	}
	msgs := []LLMMessage{{Role: "user", Content: userTurn}}

	var lastErrs []metrics.FieldError
	for attempt := 0; attempt < generateMaxAttempts; attempt++ {
		raw, err := s.llm.Complete(ctx, system, msgs)
		if err != nil {
			return nil, err
		}
		res, verrs := parseCandidate(raw)
		if len(verrs) == 0 {
			return res, nil
		}
		lastErrs = verrs
		feedback, _ := json.MarshalIndent(struct {
			Errors []metrics.FieldError `json:"errors"`
		}{verrs}, "", "  ")
		msgs = append(msgs,
			LLMMessage{Role: "assistant", Content: raw},
			LLMMessage{Role: "user", Content: "That failed validation. Fix ALL of these errors and respond with the corrected JSON object only:\n" + string(feedback)},
		)
	}
	return nil, &GenerateInvalidError{Errors: lastErrs}
}

// parseCandidate strictly decodes and validates one LLM response. All errors
// come back at once so a single retry can fix everything.
func parseCandidate(raw string) (*GenerateResult, []metrics.FieldError) {
	text := stripFences(raw)
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	var candidate struct {
		Query json.RawMessage `json:"query"`
		Title string          `json:"title"`
		Viz   string          `json:"viz"`
	}
	if err := dec.Decode(&candidate); err != nil {
		return nil, []metrics.FieldError{{Code: "invalid_response", Param: "body",
			Message: fmt.Sprintf(`response must be exactly one JSON object {"query":{...},"title":"...","viz":"..."}: %v`, err)}}
	}

	var errs []metrics.FieldError
	if len(candidate.Query) == 0 {
		errs = append(errs, metrics.FieldError{Code: "missing_query", Param: "query", Message: `"query" is required`})
	}
	if strings.TrimSpace(candidate.Title) == "" {
		errs = append(errs, metrics.FieldError{Code: "missing_title", Param: "title", Message: `"title" is required: a short human widget title`})
	}
	if fe, ok := validateViz(candidate.Viz); !ok {
		fe.Param = "viz"
		errs = append(errs, fe)
	}
	var query *metrics.Query
	if len(candidate.Query) > 0 {
		q, verr := metrics.DecodeQuery(bytes.NewReader(candidate.Query))
		if verr != nil {
			for _, fe := range verr.Errors {
				fe.Param = "query." + fe.Param
				errs = append(errs, fe)
			}
		} else if _, verr := metrics.Validate(q); verr != nil {
			for _, fe := range verr.Errors {
				fe.Param = "query." + fe.Param
				errs = append(errs, fe)
			}
		} else {
			query = q
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return &GenerateResult{Query: *query, Title: candidate.Title, Viz: candidate.Viz}, nil
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// generateSystemPrompt builds the LLM context: the metrics /schema document IS
// the query documentation (#733 one-source-of-truth), plus the output contract.
func generateSystemPrompt(now time.Time) string {
	schemaJSON, _ := json.Marshal(metrics.Schema())
	return fmt.Sprintf(`You translate a merchant's natural-language request into ONE analytics query for the OpenRails metrics API, plus a short widget title and a visualization type.

Today's date (UTC): %s.

The complete metrics schema (measures, dimensions, grains, query shape, worked examples) is the JSON below. Use ONLY measure/dimension/grain/filter names that appear in it; the "examples" array pairs intents with correct queries — imitate them.

<schema>
%s
</schema>

Respond with ONLY one JSON object — no prose, no code fences:
{"query": <query body per the schema's query_shape>, "title": "<short widget title>", "viz": "<stat|line|area|bar|donut|table>"}

Viz guidance: "stat" = a single number (no time dimension; add "compare":"previous_period" for a delta); "line"/"area"/"bar" = time series ("by" includes "time"); "donut" = share across ONE non-time dimension; "table" = ranked lists or multi-column breakdowns.
Prefer relative ranges ({"range":{"last":"7d"}}) so saved widgets stay current; use explicit dates only when the request names them.
When the request includes a "current query to modify", treat the instruction as an EDIT to that query: change only what the instruction asks for and keep everything else as-is.
If the request is ambiguous, pick the closest supported query instead of asking questions.`,
		now.Format("2006-01-02"), schemaJSON)
}
