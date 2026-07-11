package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Phase 2 (#779, flag-gated behind llm.catalog_drafting_enabled — see
// service.go's DraftingConfigured): the copilot's ONLY write-shaped output is
// a DRAFT of the exact payload the human console already produces (a #777
// wizard plan or a create-price catalog diff). Neither tool below ever calls
// a mutating service method — CreatePrice/RepriceAllPriorVersions are never
// referenced here. The draft is handed to the console to open, pre-filled,
// in the corresponding review step; the human's confirm click is the only
// path that actually calls those APIs, which enforce every constraint anew.

// noticeWindowDaysDoctrine mirrors web/admin/src/pages/catalog/price-wizard-logic.ts's
// NOTICE_WINDOW_DAYS (#777) — duplicated, not imported (frontend/backend
// boundary). It is a DEFAULT the copilot proposes, never an enforced floor:
// #773/#777 shipped no server-side minimum-notice check, and #781 (in
// flight) is what will make the API itself refuse an early increase. Once
// #781 ships, this const should be replaced by reading the merchant's
// configured window from the API.
const noticeWindowDaysDoctrine = 30

func priceDirection(newAmount, currentAmount int64) string {
	switch {
	case newAmount > currentAmount:
		return "increase"
	case newAmount < currentAmount:
		return "decrease"
	default:
		return "unchanged"
	}
}

// defaultMigrationMode mirrors price-wizard-logic.ts's defaultMigrationMode:
// increases default to grandfather (zero-risk, today's behavior with no new
// action); decreases default to migrate-now (never grandfather a decrease).
func defaultMigrationMode(direction string) string {
	if direction == "decrease" {
		return "migrate"
	}
	return "grandfather"
}

// defaultEffectiveDate mirrors price-wizard-logic.ts's defaultEffectiveDate.
func defaultEffectiveDate(direction string, now time.Time) time.Time {
	if direction == "increase" {
		return now.AddDate(0, 0, noticeWindowDaysDoctrine)
	}
	return now
}

// buildPriceChangeReviewText mirrors price-wizard-logic.ts's buildReviewText
// phrasing exactly (independently implemented server-side; the console's own
// TS copy renders the same words when a human edits the draft further).
func buildPriceChangeReviewText(currency string, currentAmount, newAmount int64, affected int, mode string, effectiveAt, now time.Time) string {
	lead := fmt.Sprintf("New subscribers pay %s immediately.", moneyutil.FormatDisplay(newAmount, currency))
	if affected == 0 {
		return lead + " No existing subscribers are on a prior version of this price."
	}
	subj := fmt.Sprintf("%d existing subscriber", affected)
	if affected != 1 {
		subj += "s"
	}
	if mode == "grandfather" {
		return fmt.Sprintf("%s %s keep %s forever (grandfathered).", lead, subj, moneyutil.FormatDisplay(currentAmount, currency))
	}
	if !effectiveAt.After(now) {
		return fmt.Sprintf("%s %s move to %s at their next renewal. Notices go out on confirm.", lead, subj, moneyutil.FormatDisplay(newAmount, currency))
	}
	return fmt.Sprintf("%s %s keep %s until %s, then move to %s at their next renewal. Notices go out on confirm.",
		lead, subj, moneyutil.FormatDisplay(currentAmount, currency), effectiveAt.Format("Jan 2, 2006"), moneyutil.FormatDisplay(newAmount, currency))
}

// crossConstraintRefusal mirrors subscriptions.validateRepriceConstraints'
// three checks EXACTLY (same order, same sentinel codes), independently
// implemented read-only here — the copilot never imports the reprice
// service's mutating methods, only its own reads (GetCurrentByKey etc.) to
// pre-flight the same business rule the API would enforce if this ever
// became a real reprice call.
func crossConstraintRefusal(from, to *models.Price) *DraftRefusal {
	if to.Archived {
		return &DraftRefusal{
			Code:       "inactive_price",
			Reason:     "the target price is archived and cannot receive a migration",
			Workaround: "point at the key's CURRENT price (call get_price to confirm which one that is) instead of an archived version.",
		}
	}
	if to.ProductID != from.ProductID {
		return &DraftRefusal{
			Code:       "cross_product",
			Reason:     "the target price belongs to a DIFFERENT product — moving subscribers onto a pre-existing, distinct product is plan consolidation, not a price change",
			Workaround: "cross-product migration is deferred (#778) until a real consolidation need appears. Workaround: archive the old product's prices (stops new sales) and leave the existing cohort grandfathered on it; a genuine consolidation needs a human to plan the entitlement/invoice-naming implications.",
		}
	}
	if !strings.EqualFold(strings.TrimSpace(to.Currency), strings.TrimSpace(from.Currency)) {
		return &DraftRefusal{
			Code:       "cross_currency",
			Reason:     "the target price is in a different currency — repricing never crosses currencies (no FX surprises on an existing billing agreement)",
			Workaround: "pick a target price in the same currency, or draft a same-currency successor under this key instead.",
		}
	}
	return nil
}

// -- Tool: draft_price_change --------------------------------------------------

const toolDraftPriceChange = "draft_price_change"

func toolDefDraftPriceChange() dashboard.ToolDef {
	return dashboard.ToolDef{
		Name:        toolDraftPriceChange,
		Description: "Draft a #777 wizard price-change plan for an EXISTING price key: a version bump (new amount, same key) plus an optional migration. NEVER mutates anything — returns a DRAFT payload for human review/confirm in the console wizard, with the affected-subscriber count and review text already computed. migration_mode defaults per direction (increase->grandfather, decrease->migrate); effective_date defaults to the notice-window floor for an increase, or now for a decrease/grandfather. Pass migrate_to_price_key ONLY if the merchant is asking to move subscribers onto a DIFFERENT, pre-existing product/price (plan consolidation) — that request is refused with a typed reason and workaround, never drafted. Amounts are in MICROS.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"price_key": {"type": "string", "description": "The key to change (its CURRENT version is the edit target)."},
				"new_amount": {"type": "integer", "description": "New amount in micros (1,000,000 = 1 currency unit)."},
				"migration_mode": {"type": "string", "enum": ["grandfather", "migrate"], "description": "Optional; defaults per direction."},
				"effective_date": {"type": "string", "description": "Optional YYYY-MM-DD; only meaningful when migration_mode=migrate. Defaults per doctrine."},
				"migrate_to_price_key": {"type": "string", "description": "Optional: a DIFFERENT existing key the merchant wants to move price_key's subscribers onto. Out of scope (#778) — always refused; included only so the refusal can be typed and explained."}
			},
			"required": ["price_key", "new_amount"],
			"additionalProperties": false
		}`),
	}
}

type draftPriceChangeArgs struct {
	PriceKey          string `json:"price_key"`
	NewAmount         int64  `json:"new_amount"`
	MigrationMode     string `json:"migration_mode,omitempty"`
	EffectiveDate     string `json:"effective_date,omitempty"`
	MigrateToPriceKey string `json:"migrate_to_price_key,omitempty"`
}

func (s *Service) runDraftPriceChange(ctx context.Context, raw json.RawMessage) (string, *Draft, error) {
	var args draftPriceChangeArgs
	if err := strictDecode(raw, &args); err != nil {
		return "", nil, err
	}
	key := strings.TrimSpace(args.PriceKey)
	if key == "" {
		return "", nil, fmt.Errorf("price_key is required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", nil, err
	}
	current, err := s.prices.GetCurrentByKey(ctx, tid.UUID(), key)
	if err != nil {
		return "", nil, fmt.Errorf("price_key %q not found — call list_catalog to see valid keys", key)
	}

	if toKey := strings.TrimSpace(args.MigrateToPriceKey); toKey != "" && toKey != key {
		to, err := s.prices.GetCurrentByKey(ctx, tid.UUID(), toKey)
		if err != nil {
			return "", nil, fmt.Errorf("migrate_to_price_key %q not found", toKey)
		}
		refusal := crossConstraintRefusal(current, to)
		if refusal == nil {
			// Same product+currency: this is NOT #778 territory — it is an
			// ordinary same-key version bump under a different name, which
			// this tool does not support ambiguously. Ask for a plain
			// draft_price_change on price_key instead.
			refusal = &DraftRefusal{
				Code:       "same_product_migration",
				Reason:     "migrate_to_price_key resolves to the same product/currency — this tool only bumps price_key's OWN version; there is no separate 'move to a same-product key' operation",
				Workaround: fmt.Sprintf("call draft_price_change with price_key=%q and the new amount directly.", key),
			}
		}
		return fmt.Sprintf("refused (%s): %s\nworkaround: %s\nnext: relay this to the merchant — no draft was created, nothing was changed.",
				refusal.Code, refusal.Reason, refusal.Workaround),
			&Draft{Kind: "refused", Refusal: refusal}, nil
	}

	if args.NewAmount <= 0 {
		return "", nil, fmt.Errorf("new_amount must be a positive number of micros")
	}
	direction := priceDirection(args.NewAmount, current.Amount)
	if direction == "unchanged" {
		return "", nil, fmt.Errorf("new_amount equals the current amount (%d) — nothing to draft", current.Amount)
	}

	mode := strings.TrimSpace(args.MigrationMode)
	if mode == "" {
		mode = defaultMigrationMode(direction)
	} else if mode != "grandfather" && mode != "migrate" {
		return "", nil, fmt.Errorf(`migration_mode must be "grandfather" or "migrate"`)
	}

	now := s.now().UTC()
	effectiveAt := defaultEffectiveDate(direction, now)
	if raw := strings.TrimSpace(args.EffectiveDate); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return "", nil, fmt.Errorf("effective_date must be YYYY-MM-DD")
		}
		effectiveAt = parsed.UTC()
	}

	affected := 0
	if s.reprices != nil {
		if preview, err := s.reprices.PreviewAllPriorVersions(ctx, key); err == nil {
			affected = preview.Matched
		}
	}

	reviewText := buildPriceChangeReviewText(current.Currency, current.Amount, args.NewAmount, affected, mode, effectiveAt, now)

	providers := make([]string, 0, len(current.PSPLinks))
	for provider := range current.PSPLinks {
		providers = append(providers, provider)
	}

	draft := &PriceChangeDraft{
		DraftID:       uuid.NewString(),
		DraftedBy:     DraftedBy,
		PriceKey:      key,
		CurrentAmount: current.Amount,
		NewAmount:     args.NewAmount,
		Currency:      current.Currency,
		Direction:     direction,
		MigrationMode: mode,
		AffectedCount: affected,
		ReviewText:    reviewText,
		CreatePrice: CreatePriceDraft{
			ProductID: current.ProductID.String(), Key: key,
			UnitAmount: args.NewAmount, Currency: current.Currency,
			AccessDurationHours: current.AccessDurationHours, AutoRenew: current.AutoRenew,
			TrialUnitAmount: current.TrialUnitAmount, TrialDurationHours: current.TrialDurationHours,
			Providers: providers,
		},
	}
	if mode == "migrate" {
		draft.Reprice = &RepriceDraft{PriceKey: key, EffectiveAt: effectiveAt}
	}

	content := fmt.Sprintf("draft ready (draft_id=%s): %s\nnext: requires human confirm via the price-change wizard — nothing has been changed yet.", draft.DraftID, reviewText)
	return content, &Draft{Kind: "price_change", PriceChange: draft}, nil
}

// -- Tool: draft_catalog_diff ---------------------------------------------------

const toolDraftCatalogDiff = "draft_catalog_diff"

func toolDefDraftCatalogDiff() dashboard.ToolDef {
	return dashboard.ToolDef{
		Name:        toolDraftCatalogDiff,
		Description: "Draft a NEW price (a new tier/plan variant, e.g. \"add a with-ads tier at $6\") on an EXISTING product. NEVER mutates anything — returns a create-price DRAFT for human review/confirm in the console. Only adds a price to a product that already exists; drafting a brand-new PRODUCT is out of scope for v1 (create the product first, then ask again). Amounts are in MICROS.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"product_key": {"type": "string", "description": "An EXISTING product's key to add the price to."},
				"new_price_key": {"type": "string", "description": "Optional; auto-defaults to \"<product_key>-<interval>\" (refused if that key is already in use — pass an explicit distinct key, e.g. \"...-with-ads\")."},
				"unit_amount": {"type": "integer", "description": "Amount in micros (1,000,000 = 1 currency unit)."},
				"currency": {"type": "string", "description": "Optional; defaults to the product's existing price currency if it has one."},
				"access_duration_hours": {"type": "integer", "description": "Billing period in hours (720=~monthly, 8760=~yearly). Omit for a durable one-time purchase."},
				"auto_renew": {"type": "boolean", "description": "Whether it recurs. Requires access_duration_hours."}
			},
			"required": ["product_key", "unit_amount"],
			"additionalProperties": false
		}`),
	}
}

type draftCatalogDiffArgs struct {
	ProductKey          string `json:"product_key"`
	NewPriceKey         string `json:"new_price_key,omitempty"`
	UnitAmount          int64  `json:"unit_amount"`
	Currency            string `json:"currency,omitempty"`
	AccessDurationHours *int   `json:"access_duration_hours,omitempty"`
	AutoRenew           bool   `json:"auto_renew,omitempty"`
}

func (s *Service) runDraftCatalogDiff(ctx context.Context, raw json.RawMessage) (string, *Draft, error) {
	var args draftCatalogDiffArgs
	if err := strictDecode(raw, &args); err != nil {
		return "", nil, err
	}
	productKey := strings.TrimSpace(args.ProductKey)
	if productKey == "" {
		return "", nil, fmt.Errorf("product_key is required")
	}
	if args.UnitAmount <= 0 {
		return "", nil, fmt.Errorf("unit_amount must be a positive number of micros")
	}
	product, err := s.products.GetByKey(ctx, productKey)
	if err != nil {
		return "", nil, fmt.Errorf("product_key %q not found — create the product first (out of scope for this tool)", productKey)
	}

	currency := strings.ToLower(strings.TrimSpace(args.Currency))
	if currency == "" {
		existing, err := s.prices.GetActiveByProductID(ctx, product.ID)
		if err == nil && len(existing) > 0 {
			currency = existing[0].Currency
		} else {
			return "", nil, fmt.Errorf("currency is required: product %q has no existing prices to infer it from", productKey)
		}
	}

	interval := priceIntervalLabel(args.AccessDurationHours, args.AutoRenew)
	key := strings.TrimSpace(args.NewPriceKey)
	if key == "" {
		key = productKey + "-" + interval
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.prices.GetCurrentByKey(ctx, tid.UUID(), key); err == nil {
		return "", nil, fmt.Errorf("key %q is already in use by an existing price — pass a distinct new_price_key", key)
	}

	reviewText := fmt.Sprintf("New tier %q on %s: %s / %s. No existing subscribers are affected until you create it.",
		key, product.DisplayName, moneyutil.FormatDisplay(args.UnitAmount, currency), interval)

	draft := &CatalogDiffDraft{
		DraftID: uuid.NewString(), DraftedBy: DraftedBy,
		ProductKey: productKey, ReviewText: reviewText,
		CreatePrice: CreatePriceDraft{
			ProductID: product.ID.String(), Key: key,
			UnitAmount: args.UnitAmount, Currency: currency,
			AccessDurationHours: args.AccessDurationHours, AutoRenew: args.AutoRenew,
		},
	}
	content := fmt.Sprintf("draft ready (draft_id=%s): %s\nnext: requires human confirm via the console's new-price form — nothing has been changed yet.", draft.DraftID, reviewText)
	return content, &Draft{Kind: "catalog_diff", CatalogDiff: draft}, nil
}
