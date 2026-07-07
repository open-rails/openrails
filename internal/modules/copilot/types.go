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

// Evidence is one executed tool call: the tool name, its args, and the exact
// compact-table text fed to the model — "trust = show your work" (#756):
// on-screen evidence is byte-identical to what the model saw, never a
// separate re-summarization.
type Evidence struct {
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	Summary string `json:"summary"`
}

// Draft is the copilot's ONLY write-shaped output (Phase 2, flag-gated): a
// proposal shaped exactly like a #777 wizard confirm or a catalog-diff
// create-price call. It is never executed by the tool layer — the console
// opens it, pre-filled, in the corresponding human review step. Exactly one
// of PriceChange / CatalogDiff / Refusal is set.
type Draft struct {
	Kind        string            `json:"kind"` // "price_change" | "catalog_diff" | "refused"
	PriceChange *PriceChangeDraft `json:"price_change,omitempty"`
	CatalogDiff *CatalogDiffDraft `json:"catalog_diff,omitempty"`
	Refusal     *DraftRefusal     `json:"refusal,omitempty"`
}

// DraftedBy marks every non-refused draft's provenance — forwarded by the
// console on wizard confirm so the confirm event is audit-logged as
// copilot-originated (see ConfirmDraft).
const DraftedBy = "copilot"

// CreatePriceDraft mirrors billingservice.CreatePriceRequest's WIRE shape
// (field-for-field, not the Go type — importing pkg/service would cycle:
// it transitively imports internal/app, which imports this package). This
// is exactly the body the #777 wizard's confirm step POSTs to
// /v1/merchant/catalog/prices.
type CreatePriceDraft struct {
	ProductID           string   `json:"product_id"`
	Key                 string   `json:"key"`
	UnitAmount          int64    `json:"unit_amount"`
	Currency            string   `json:"currency"`
	AccessDurationHours *int     `json:"access_duration_hours,omitempty"`
	AutoRenew           bool     `json:"auto_renew"`
	TrialUnitAmount     *int64   `json:"trial_unit_amount,omitempty"`
	TrialDurationHours  *int     `json:"trial_duration_hours,omitempty"`
	Providers           []string `json:"providers,omitempty"`
}

// RepriceDraft mirrors the #773 bulk reprice-all-prior-versions body
// (subscriptions.RepriceAllPriorVersionsRequest's wire shape).
type RepriceDraft struct {
	PriceKey    string    `json:"price_key"`
	EffectiveAt time.Time `json:"effective_at"`
}

// PriceChangeDraft is the #777 wizard payload: a version bump on PriceKey
// plus an optional migration plan. CreatePrice is ALWAYS populated (the
// version-bump call); Reprice is populated only when MigrationMode is
// "migrate". AffectedCount and ReviewText are computed inline (AXI: the
// model never needs a second call to know the blast radius).
type PriceChangeDraft struct {
	DraftID       string           `json:"draft_id"`
	DraftedBy     string           `json:"drafted_by"`
	PriceKey      string           `json:"price_key"`
	CurrentAmount int64            `json:"current_amount"`
	NewAmount     int64            `json:"new_amount"`
	Currency      string           `json:"currency"`
	Direction     string           `json:"direction"` // increase | decrease | unchanged
	MigrationMode string           `json:"migration_mode"`
	AffectedCount int              `json:"affected_count"`
	ReviewText    string           `json:"review_text"`
	CreatePrice   CreatePriceDraft `json:"create_price"`
	Reprice       *RepriceDraft    `json:"reprice,omitempty"`
}

// CatalogDiffDraft is a new-price ("add a tier") proposal on an EXISTING
// product — never a new product (out of scope for v1; see doctrine.go).
type CatalogDiffDraft struct {
	DraftID     string           `json:"draft_id"`
	DraftedBy   string           `json:"drafted_by"`
	ProductKey  string           `json:"product_key"`
	ReviewText  string           `json:"review_text"`
	CreatePrice CreatePriceDraft `json:"create_price"`
}

// DraftRefusal is a typed, corrective refusal — the copilot's mirror of
// #773's RepriceConstraintError sentinels, surfaced BEFORE a draft is even
// built (never a mutation attempt). Code is machine-stable
// ("cross_product" | "cross_currency" | "inactive_price"); Workaround is the
// human-facing next step (#778's archive-and-grandfather for cross_product).
type DraftRefusal struct {
	Code       string `json:"code"`
	Reason     string `json:"reason"`
	Workaround string `json:"workaround"`
}

// AskResult is the POST /v1/merchant/catalog/ask response.
type AskResult struct {
	Answer   string     `json:"answer"`
	Evidence []Evidence `json:"evidence"`
	Drafts   []Draft    `json:"drafts,omitempty"`
}
