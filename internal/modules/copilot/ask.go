package copilot

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/dashboard"
)

// Ask-loop caps bound every cost axis so a cheap model can run the loop. One
// more tool call than dashboard's Ask: a catalog question often chains
// list_catalog, price_history or list_reprice_batches, then a draft.
const (
	askMaxToolCalls = 6
	askMaxTokens    = 2048
	askMaxTurns     = askMaxToolCalls + 2
)

// Ask answers a catalog question with read-only lookups and, when armed,
// drafting tools. The model sees only aggregate catalog and subscriber-count
// data and never triggers a mutation.
func (s *Service) Ask(ctx context.Context, question string) (*billing.CatalogAnswer, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}

	now := s.now().UTC()
	system := s.systemPrompt(ctx, now)
	tools := s.toolDefs()
	msgs := []dashboard.ToolMessage{{Role: "user", Text: question}}
	evidence := []billing.CatalogEvidence{}
	drafts := []billing.CatalogDraft{}
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
			return &billing.CatalogAnswer{Answer: answer, Evidence: evidence, Drafts: drafts}, nil
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

// toolDefs appends drafting tools only when armed: off means absent, never
// present-but-erroring.
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

// runTool dispatches one tool call against the shared budget, collecting Q&A
// results into evidence and drafts into drafts.
func (s *Service) runTool(ctx context.Context, call dashboard.ToolCall, evidence *[]billing.CatalogEvidence, drafts *[]billing.CatalogDraft, attempts *int) dashboard.ToolResult {
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
		*evidence = append(*evidence, billing.CatalogEvidence{Tool: call.Name, Args: string(call.Input), Summary: content})
		return dashboard.ToolResult{ToolUseID: call.ID, Content: content}
	}
	draftResult := func(content string, draft *billing.CatalogDraft, err error) dashboard.ToolResult {
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
