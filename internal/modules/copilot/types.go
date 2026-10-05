package copilot

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotConfigured: no LLM key or no llm.catalog_copilot_enabled consent —
// the endpoint answers 501 with a pointed message (mirrors #756's
// ErrAskNotConfigured).
var ErrNotConfigured = errors.New("copilot: catalog copilot not configured")

// RateLimitedError: the merchant exceeded its per-merchant ask budget.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("copilot: rate limit exceeded (retry after %s)", e.RetryAfter)
}

// NoAnswerError: the model never produced a text answer within the loop
// budget (kept calling tools).
type NoAnswerError struct{ ToolCalls int }

func (e *NoAnswerError) Error() string {
	return fmt.Sprintf("copilot: model produced no answer within the tool budget (%d tool calls)", e.ToolCalls)
}
