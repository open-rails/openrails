package copilot

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Ask-loop caps, same doctrine as #756's dashboard.Service.Ask: every cost
// axis is bounded so a cheap model can be trusted with the loop. One more
// tool call than metrics' Ask allows, since a catalog question often chains
// list_catalog -> price_history/list_reprice_batches -> (optionally) a draft
// call.
const (
	askMaxToolCalls = 6
	askMaxTokens    = 2048
	askMaxTurns     = askMaxToolCalls + 2
)

// Ask answers a natural-language catalog question by letting the model run
// read-only lookups (Phase 1) and, when armed, drafting tools (Phase 2) as
// tool calls. The model never sees anything beyond aggregate catalog/
// subscriber-count data and never triggers a mutation — see tools_draft.go.
func (s *Service) Ask(ctx context.Context, question string) (*AskResult, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	if s.limiter != nil {
		merchantID, err := merchant.Require(ctx)
		if err != nil {
			return nil, fmt.Errorf("copilot ask: no merchant in context: %w", err)
		}
		allowed, retry, err := s.limiter.AllowAsk(ctx, uuid.UUID(merchantID).String())
		if err != nil {
			return nil, fmt.Errorf("copilot ask: rate limiter: %w", err)
		}
		if !allowed {
			return nil, &RateLimitedError{RetryAfter: retry}
		}
	}

	now := s.now().UTC()
	system := s.systemPrompt(ctx, now)
	tools := s.toolDefs()
	msgs := []dashboard.ToolMessage{{Role: "user", Text: question}}
	var evidence []Evidence
	var drafts []Draft
	attempts := 0

	for turn := 0; turn < askMaxTurns; turn++ {
		resp, err := s.llm.CompleteTools(ctx, system, tools, msgs, askMaxTokens)
		if err != nil {
			return nil, err
		}
		if len(resp.ToolCalls) == 0 {
			answer := strings.TrimSpace(resp.Text)
			if answer == "" {
				return nil, fmt.Errorf("copilot ask: empty model response (stop_reason %q)", resp.StopReason)
			}
			return &AskResult{Answer: answer, Evidence: evidence, Drafts: drafts}, nil
		}
		results := make([]dashboard.ToolResult, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			results = append(results, s.runTool(ctx, call, &evidence, &drafts, &attempts))
		}
		msgs = append(msgs,
			dashboard.ToolMessage{Role: "assistant", Text: resp.Text, ToolCalls: resp.ToolCalls},
			dashboard.ToolMessage{Role: "user", ToolResults: results},
		)
	}
	return nil, &NoAnswerError{ToolCalls: attempts}
}

// toolDefs is the tool list the model sees this turn — drafting tools are
// APPENDED only when armed (flag off = absent from the list entirely, never
// present-but-erroring).
func (s *Service) toolDefs() []dashboard.ToolDef {
	defs := []dashboard.ToolDef{
		toolDefListCatalog(), toolDefGetPrice(), toolDefPriceHistory(), toolDefListRepriceBatches(),
	}
	if s.DraftingConfigured() {
		defs = append(defs, toolDefDraftPriceChange(), toolDefDraftCatalogDiff())
	}
	return defs
}

func toolNames(defs []dashboard.ToolDef) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

// runTool dispatches one tool call, enforcing the shared budget and
// translating each tool's (content, error) — or (content, *Draft, error) for
// drafting tools — into the wire ToolResult, collecting Q&A results into
// evidence and drafts into drafts as a side effect.
func (s *Service) runTool(ctx context.Context, call dashboard.ToolCall, evidence *[]Evidence, drafts *[]Draft, attempts *int) dashboard.ToolResult {
	errResult := func(msg string) dashboard.ToolResult {
		return dashboard.ToolResult{ToolUseID: call.ID, Content: msg, IsError: true}
	}
	known := map[string]bool{
		toolListCatalog: true, toolGetPrice: true, toolPriceHistory: true, toolListRepriceBatches: true,
		toolDraftPriceChange: s.DraftingConfigured(), toolDraftCatalogDiff: s.DraftingConfigured(),
	}
	if !known[call.Name] {
		return errResult(fmt.Sprintf("unknown tool %q: available tools are %s", call.Name, strings.Join(toolNames(s.toolDefs()), ", ")))
	}
	if *attempts >= askMaxToolCalls {
		return errResult(fmt.Sprintf("tool-call budget exhausted (max %d calls per question) — answer now from what you already have, or say what you could not determine", askMaxToolCalls))
	}
	*attempts++

	qa := func(content string, err error) dashboard.ToolResult {
		if err != nil {
			return errResult(err.Error())
		}
		*evidence = append(*evidence, Evidence{Tool: call.Name, Args: string(call.Input), Summary: content})
		return dashboard.ToolResult{ToolUseID: call.ID, Content: content}
	}
	draftResult := func(content string, draft *Draft, err error) dashboard.ToolResult {
		if err != nil {
			return errResult(err.Error())
		}
		if draft != nil {
			*drafts = append(*drafts, *draft)
		}
		return dashboard.ToolResult{ToolUseID: call.ID, Content: content}
	}

	switch call.Name {
	case toolListCatalog:
		return qa(s.runListCatalog(ctx, call.Input))
	case toolGetPrice:
		return qa(s.runGetPrice(ctx, call.Input))
	case toolPriceHistory:
		return qa(s.runPriceHistory(ctx, call.Input))
	case toolListRepriceBatches:
		return qa(s.runListRepriceBatches(ctx, call.Input))
	case toolDraftPriceChange:
		return draftResult(s.runDraftPriceChange(ctx, call.Input))
	case toolDraftCatalogDiff:
		return draftResult(s.runDraftCatalogDiff(ctx, call.Input))
	default:
		return errResult(fmt.Sprintf("unknown tool %q", call.Name))
	}
}
