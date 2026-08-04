package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
)

// priceIntervalLabel mirrors pkg/service.PriceIntervalLabel (#774) exactly —
// duplicated rather than imported because pkg/service transitively imports
// internal/app, which imports this package (a cycle). Keep in sync with
// pkg/service/price_key.go and migrations/postgres/0001_schema.up.sql's
// trg_prices_default_key/prices_default_key backfill CASE if the interval
// buckets ever change.
func priceIntervalLabel(accessDurationHours *int, autoRenew bool) string {
	if !autoRenew || accessDurationHours == nil {
		return "onetime"
	}
	switch *accessDurationHours {
	case 168:
		return "weekly"
	case 720, 744:
		return "monthly"
	case 2160, 2184:
		return "quarterly"
	case 8760, 8784:
		return "yearly"
	default:
		return fmt.Sprintf("%dd", *accessDurationHours/24)
	}
}

// activeSubscriberCount is the per-price-row subscriber count primitive: the
// COUNT(*) query only (Limit:0 discards the items page — cheap regardless of
// cohort size, same pattern #757/#733 pagination already uses everywhere).
func (s *Service) activeSubscriberCount(ctx context.Context, priceID uuid.UUID) (int, error) {
	if s.subs == nil {
		return 0, fmt.Errorf("subscription service unavailable")
	}
	_, total, err := s.subs.GetSubscribers(ctx, query.QueryOptions[subscriptions.GetSubscriptionsFilters]{
		Filters: subscriptions.GetSubscriptionsFilters{PriceID: priceID, Status: string(models.StatusActive)},
		Limit:   0,
	})
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

// catalogRow is one LEAN projection row (AXI: key, amount, status, inline
// aggregates — never the full price row).
type catalogRow struct {
	ProductKey    string
	ProductName   string
	PriceKey      string
	PriceID       uuid.UUID
	Amount        int64
	Currency      string
	Interval      string
	ActiveSubs    int
	Grandfathered int
}

// catalogRows lists every ACTIVE product's ACTIVE (current) prices with
// inline active-subscriber + grandfathered counts — the content-first live
// summary that opens both the system prompt and the list_catalog tool
// result. productKeyFilter, when set, scopes to one product.
func (s *Service) catalogRows(ctx context.Context, productKeyFilter string) ([]catalogRow, error) {
	if s.products == nil || s.prices == nil {
		return nil, fmt.Errorf("catalog service unavailable")
	}
	var products []*models.Product
	var err error
	if productKeyFilter != "" {
		p, err := s.products.GetByKey(ctx, productKeyFilter)
		if err != nil {
			return nil, fmt.Errorf("product_key %q not found", productKeyFilter)
		}
		products = []*models.Product{p}
	} else {
		products, err = s.products.GetActive(ctx)
		if err != nil {
			return nil, err
		}
	}

	var rows []catalogRow
	for _, p := range products {
		prices, err := s.prices.GetActiveByProductID(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("list prices for product %q: %w", p.Key, err)
		}
		for _, price := range prices {
			active, err := s.activeSubscriberCount(ctx, price.ID)
			if err != nil {
				return nil, fmt.Errorf("count active subscribers for price %q: %w", price.Key, err)
			}
			grand := 0
			if s.reprices != nil {
				preview, err := s.reprices.PreviewAllPriorVersions(ctx, price.Key)
				if err == nil && preview.Matched > active {
					grand = preview.Matched - active
				}
			}
			rows = append(rows, catalogRow{
				ProductKey: p.Key, ProductName: p.DisplayName,
				PriceKey: price.Key, PriceID: price.ID,
				Amount: price.Amount, Currency: price.Currency,
				Interval:   priceIntervalLabel(price.AccessDurationHours, price.AutoRenew),
				ActiveSubs: active, Grandfathered: grand,
			})
		}
	}
	return rows, nil
}

func renderCatalogRows(rows []catalogRow) string {
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.ProductKey, r.PriceKey, moneyutil.FormatDisplay(moneyutil.Micros(r.Amount), r.Currency), r.Interval,
			itoa(r.ActiveSubs), itoa(r.Grandfathered),
		})
	}
	return renderTable(
		[]string{"product_key", "price_key", "amount", "interval", "active_subscribers", "grandfathered"},
		table, "0 active products/prices in the catalog.",
	)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// -- Tool: list_catalog -------------------------------------------------------

const toolListCatalog = "list_catalog"

func toolDefListCatalog() dashboard.ToolDef {
	return dashboard.ToolDef{
		Name:        toolListCatalog,
		Description: "List every active product and its current (live) prices, 3-4 fields each, with the active-subscriber count and grandfathered (still on a prior version) count already computed inline — never call another tool to learn the blast radius of a price. Optionally scope to one product. Amounts are in MICROS (1,000,000 micros = 1 currency unit). Idempotent, safe to retry.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"product_key": {"type": "string", "description": "Optional: scope to one product's key."}
			},
			"additionalProperties": false
		}`),
	}
}

type listCatalogArgs struct {
	ProductKey string `json:"product_key,omitempty"`
}

func (s *Service) runListCatalog(ctx context.Context, raw json.RawMessage) (string, error) {
	var args listCatalogArgs
	if err := strictDecode(raw, &args); err != nil {
		return "", err
	}
	rows, err := s.catalogRows(ctx, strings.TrimSpace(args.ProductKey))
	if err != nil {
		return "", err
	}
	return renderCatalogRows(rows), nil
}

// -- Tool: get_price -----------------------------------------------------------

const toolGetPrice = "get_price"

func toolDefGetPrice() dashboard.ToolDef {
	return dashboard.ToolDef{
		Name:        toolGetPrice,
		Description: "Full detail for ONE price key: amount, currency, interval, product, active-subscriber count, grandfathered count, and whether a reprice migration is currently pending for it. The escape hatch beyond list_catalog's lean rows. Idempotent, safe to retry.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"price_key": {"type": "string"}},
			"required": ["price_key"],
			"additionalProperties": false
		}`),
	}
}

type getPriceArgs struct {
	PriceKey string `json:"price_key"`
}

func (s *Service) runGetPrice(ctx context.Context, raw json.RawMessage) (string, error) {
	var args getPriceArgs
	if err := strictDecode(raw, &args); err != nil {
		return "", err
	}
	key := strings.TrimSpace(args.PriceKey)
	if key == "" {
		return "", fmt.Errorf("price_key is required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	price, err := s.prices.GetCurrentByKey(ctx, tid.UUID(), key)
	if err != nil {
		return "", fmt.Errorf("price_key %q not found — call list_catalog to see valid keys", key)
	}
	product, err := s.products.GetByID(ctx, price.ProductID)
	if err != nil {
		return "", fmt.Errorf("resolve product for price_key %q: %w", key, err)
	}
	active, err := s.activeSubscriberCount(ctx, price.ID)
	if err != nil {
		return "", err
	}
	grand := 0
	pending := "no pending migration"
	if s.reprices != nil {
		if preview, err := s.reprices.PreviewAllPriorVersions(ctx, key); err == nil && preview.Matched > active {
			grand = preview.Matched - active
		}
		if batches, err := s.reprices.ListBatchesForKey(ctx, key, 1, 0); err == nil && len(batches) > 0 {
			b := batches[0]
			pending = fmt.Sprintf("pending migration: batch effective %s, %d/%d scheduled, %d skipped",
				b.EffectiveAt.Format("2006-01-02"), b.SubscriptionsScheduled, b.SubscriptionsMatched, b.SubscriptionsSkipped)
		}
	}
	lines := []string{
		fmt.Sprintf("price_key: %s", price.Key),
		fmt.Sprintf("product: %s (%s)", product.DisplayName, product.Key),
		fmt.Sprintf("amount: %s", moneyutil.FormatDisplay(moneyutil.Micros(price.Amount), price.Currency)),
		fmt.Sprintf("interval: %s", priceIntervalLabel(price.AccessDurationHours, price.AutoRenew)),
		fmt.Sprintf("active_subscribers: %d", active),
		fmt.Sprintf("grandfathered (on prior versions): %d", grand),
		pending,
	}
	return strings.Join(lines, "\n"), nil
}

// -- Tool: price_history -------------------------------------------------------

const toolPriceHistory = "price_history"

func toolDefPriceHistory() dashboard.ToolDef {
	return dashboard.ToolDef{
		Name:        toolPriceHistory,
		Description: "The full version-chain history for a price key, most-recent-first, resolved from the durable pointer-movement log (not each row's own creation date — a reactivated amount shows its most recent movement). Each entry carries the active-subscriber count STILL on that specific historical amount (\"who's still on the old price?\"). Idempotent, safe to retry.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"price_key": {"type": "string"}},
			"required": ["price_key"],
			"additionalProperties": false
		}`),
	}
}

func (s *Service) runPriceHistory(ctx context.Context, raw json.RawMessage) (string, error) {
	var args getPriceArgs
	if err := strictDecode(raw, &args); err != nil {
		return "", err
	}
	key := strings.TrimSpace(args.PriceKey)
	if key == "" {
		return "", fmt.Errorf("price_key is required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	movements, err := s.prices.ListKeyMovements(ctx, tid.UUID(), key)
	if err != nil || len(movements) == 0 {
		return "", fmt.Errorf("price_key %q not found — call list_catalog to see valid keys", key)
	}
	table := make([][]string, 0, len(movements))
	for _, m := range movements {
		price, err := s.prices.GetByID(ctx, m.PriceID)
		if err != nil {
			return "", fmt.Errorf("resolve historical price %s: %w", m.PriceID, err)
		}
		active, err := s.activeSubscriberCount(ctx, price.ID)
		if err != nil {
			return "", err
		}
		current := "prior"
		if !price.Archived {
			current = "current"
		}
		table = append(table, []string{
			m.EffectiveAt.Format("2006-01-02"), moneyutil.FormatDisplay(moneyutil.Micros(price.Amount), price.Currency),
			current, itoa(active),
		})
	}
	return renderTable([]string{"effective_at", "amount", "status", "active_subscribers"}, table, "no history"), nil
}

// -- Tool: list_reprice_batches -------------------------------------------------

const toolListRepriceBatches = "list_reprice_batches"

func toolDefListRepriceBatches() dashboard.ToolDef {
	return dashboard.ToolDef{
		Name:        toolListRepriceBatches,
		Description: "List a price key's bulk reprice (migration) operations, most-recent-first, with progress (applied/scheduled/canceled counts out of the total matched). \"0 pending migrations\" is stated explicitly, never an empty list to interpret. Idempotent, safe to retry.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"price_key": {"type": "string"}},
			"required": ["price_key"],
			"additionalProperties": false
		}`),
	}
}

// repriceBatchCountLimit caps the per-batch row fetch used to tally
// applied/scheduled/canceled — generous for a Q&A summary; a batch beyond
// this is noted as truncated rather than silently under-counted.
const repriceBatchCountLimit = 2000

func (s *Service) runListRepriceBatches(ctx context.Context, raw json.RawMessage) (string, error) {
	var args getPriceArgs
	if err := strictDecode(raw, &args); err != nil {
		return "", err
	}
	key := strings.TrimSpace(args.PriceKey)
	if key == "" {
		return "", fmt.Errorf("price_key is required")
	}
	if s.reprices == nil {
		return "", fmt.Errorf("reprice service unavailable")
	}
	batches, err := s.reprices.ListBatchesForKey(ctx, key, 20, 0)
	if err != nil {
		return "", err
	}
	table := make([][]string, 0, len(batches))
	for _, b := range batches {
		rows, err := s.reprices.List(ctx, subscriptions.SubscriptionRepriceFilter{RepriceBatchID: &b.ID}, repriceBatchCountLimit, 0)
		if err != nil {
			return "", err
		}
		var applied, scheduled, canceled int
		for _, r := range rows {
			switch r.Status {
			case models.RepriceStatusApplied:
				applied++
			case models.RepriceStatusScheduled:
				scheduled++
			case models.RepriceStatusCanceled:
				canceled++
			}
		}
		note := ""
		if len(rows) >= repriceBatchCountLimit {
			note = " (truncated count)"
		}
		table = append(table, []string{
			b.EffectiveAt.Format("2006-01-02"),
			fmt.Sprintf("%d matched", b.SubscriptionsMatched),
			fmt.Sprintf("%d applied / %d scheduled / %d canceled%s", applied, scheduled, canceled, note),
			b.CreatedAt.Format("2006-01-02"),
		})
	}
	return renderTable([]string{"effective_at", "matched", "progress", "created_at"}, table,
		fmt.Sprintf("0 pending migrations for price key %q.", key)), nil
}

// strictDecode fails loudly on unknown fields (AXI + house DisallowUnknownField
// posture, extended to tool args).
func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}
