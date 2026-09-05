package embedded

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

type CatalogDumpOptions struct {
	Config   *config.Config
	PGXPool  *pgxpool.Pool
	Merchant string
	Out      io.Writer
}

func DumpMerchantCatalog(ctx context.Context, opts CatalogDumpOptions) error {
	if opts.Config == nil {
		return fmt.Errorf("config not loaded; in-process mode requires config")
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return err
	}
	if opts.PGXPool == nil {
		defer func() { _ = database.Close() }()
	}
	mctx, err := contextForCatalogPushTarget(ctx, database, opts.Merchant)
	if err != nil {
		return err
	}
	var manifest *catalog.Manifest
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var err error
		manifest, err = dumpCatalogManifest(ctx, database)
		return err
	}); err != nil {
		return err
	}
	raw, err := yaml.Marshal(catalogPushFile{
		Version: catalog.SupportedVersion,
		Catalogs: []catalogPushFileEntry{{
			Merchant:       strings.ToLower(strings.TrimSpace(opts.Merchant)),
			Products:       manifest.Products,
			Meters:         manifest.Meters,
			CreditBalances: manifest.CreditBalances,
			UsageLimits:    manifest.UsageLimits,
		}},
	})
	if err != nil {
		return fmt.Errorf("marshal catalog manifest: %w", err)
	}
	_, err = out.Write(raw)
	return err
}

func dumpCatalogManifest(ctx context.Context, database *db.DB) (*catalog.Manifest, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	m := &catalog.Manifest{Version: catalog.SupportedVersion}
	productIDs, byID, err := dumpCatalogProducts(ctx, database, tid.UUID())
	if err != nil {
		return nil, err
	}
	if m.Meters, err = dumpCatalogMeters(ctx, database, tid.UUID()); err != nil {
		return nil, err
	}
	if m.CreditBalances, err = dumpCatalogCreditBalances(ctx, database, tid.UUID()); err != nil {
		return nil, err
	}
	if m.UsageLimits, err = dumpCatalogUsageLimits(ctx, database, tid.UUID()); err != nil {
		return nil, err
	}
	if err := dumpCatalogProductEdges(ctx, database, tid.UUID(), byID); err != nil {
		return nil, err
	}
	if err := dumpCatalogPrices(ctx, database, tid.UUID(), byID); err != nil {
		return nil, err
	}
	if err := dumpCatalogRateCards(ctx, database, tid.UUID(), byID); err != nil {
		return nil, err
	}
	if err := dumpCatalogCreditPurchasePrices(ctx, database, tid.UUID(), byID); err != nil {
		return nil, err
	}
	for _, id := range productIDs {
		if p := byID[id]; p != nil {
			for i := range p.Credits {
				if money.IsQualifiedUnit(p.Credits[i].Unit) {
					name, err := money.NewMoneyService(database).CustomUnitName(ctx, p.Credits[i].Unit)
					if err != nil {
						return nil, err
					}
					p.Credits[i].Unit = name
				}
			}
			normalizeDumpProduct(p)
			m.Products = append(m.Products, *p)
		}
	}
	warnGrantUnitMismatches(m)
	return m, nil
}

// warnGrantUnitMismatches surfaces stored credits_spec rows whose unit
// disagrees with the credit balance the grant names — the or#883 defect as it
// may already exist in data. Those grants deposit into a DIFFERENT money
// account than the balance the catalog names. Detection only: never rewritten
// here, because rewriting it moves where money lands.
func warnGrantUnitMismatches(m *catalog.Manifest) {
	units := make(map[string]string, len(m.CreditBalances))
	for _, b := range m.CreditBalances {
		units[b.Key] = b.Unit
	}
	for _, p := range m.Products {
		for _, g := range p.Credits {
			want, ok := units[g.Key]
			if !ok || strings.TrimSpace(g.Unit) == "" || strings.EqualFold(g.Unit, want) {
				continue
			}
			log.Warnf("⚠️  or#883: product %q credit grant %q is stored with unit %q but balance %q declares unit %q — "+
				"this grant deposits into a different money account than the catalog names; re-apply the manifest to correct it",
				p.Key, g.Key, g.Unit, g.Key, want)
		}
	}
}

func normalizeDumpProduct(p *catalog.Product) {
	if p == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(p.TierGroup), "default") {
		p.TierGroup = ""
		p.TierRank = nil
	}
	if len(p.RateCards) == 0 && !dumpProductIsCreditTopUp(*p) {
		return
	}
	p.TierGroup = ""
	p.TierRank = nil
}

func dumpProductIsCreditTopUp(p catalog.Product) bool {
	return len(p.Credits) == 1 && p.Credits[0].Amount == nil
}

func dumpCatalogProducts(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]uuid.UUID, map[uuid.UUID]*catalog.Product, error) {
	rows, err := database.Qx(ctx).Query(ctx, `
	SELECT id, key, display_name, COALESCE(description, ''), COALESCE(entitlements_spec, '{}'::jsonb),
	       COALESCE(credits_spec, '{}'::jsonb), tier_group, tier_rank, archived
	FROM openrails.products
	WHERE merchant_id = $1
	ORDER BY COALESCE(tier_group, ''), tier_rank, key`, merchantID)
	if err != nil {
		return nil, nil, fmt.Errorf("list catalog products: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	byID := map[uuid.UUID]*catalog.Product{}
	for rows.Next() {
		var (
			id                          uuid.UUID
			p                           catalog.Product
			entitlementsRaw, creditsRaw []byte
			tierGroup                   sql.NullString
		)
		if err := rows.Scan(&id, &p.Key, &p.DisplayName, &p.Description, &entitlementsRaw, &creditsRaw, &tierGroup, &p.TierRank, &p.Archived); err != nil {
			return nil, nil, err
		}
		if tierGroup.Valid {
			p.TierGroup = tierGroup.String
		}
		p.Entitlements = entitlementKeys(entitlementsRaw)
		p.Credits = creditGrants(creditsRaw)
		ids = append(ids, id)
		cp := p
		byID[id] = &cp
	}
	return ids, byID, rows.Err()
}

func dumpCatalogMeters(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]catalog.Meter, error) {
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT key, COALESCE(event_type, ''), COALESCE(value_property, ''),
       COALESCE(aggregation, ''), COALESCE(unit, ''), COALESCE(group_by, '{}'::jsonb)
FROM openrails.catalog_meters
WHERE merchant_id = $1
ORDER BY key`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("list catalog meters: %w", err)
	}
	defer rows.Close()
	var out []catalog.Meter
	for rows.Next() {
		var m catalog.Meter
		var groupBy []byte
		if err := rows.Scan(&m.Key, &m.EventType, &m.ValueProperty, &m.Aggregation, &m.Unit, &groupBy); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(groupBy, &m.GroupBy)
		out = append(out, m)
	}
	return out, rows.Err()
}

func dumpCatalogCreditBalances(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]catalog.CreditBalance, error) {
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT b.key, b.unit, c.name, b.expires_hours
FROM openrails.catalog_credit_balances b
LEFT JOIN openrails.custom_credit_types c ON c.merchant_id=b.merchant_id AND 'credit:'||c.id::text=b.unit
WHERE b.merchant_id = $1
ORDER BY b.key`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("list catalog credit balances: %w", err)
	}
	defer rows.Close()
	var out []catalog.CreditBalance
	for rows.Next() {
		var b catalog.CreditBalance
		var expires sql.NullInt64
		var name sql.NullString
		if err := rows.Scan(&b.Key, &b.Unit, &name, &expires); err != nil {
			return nil, err
		}
		if strings.HasPrefix(b.Unit, "credit:") {
			if !name.Valid {
				return nil, fmt.Errorf("catalog credit balance %q has no owned unit identity", b.Key)
			}
			b.Unit = name.String
		}
		if expires.Valid {
			b.ExpiresDefault = hoursSpec(int(expires.Int64))
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func dumpCatalogUsageLimits(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]catalog.UsageLimit, error) {
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT key, measure, windows
FROM openrails.catalog_usage_limits
WHERE merchant_id = $1
ORDER BY key`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("list catalog usage limits: %w", err)
	}
	defer rows.Close()
	var out []catalog.UsageLimit
	for rows.Next() {
		var l catalog.UsageLimit
		var windows []byte
		if err := rows.Scan(&l.Key, &l.Measure, &windows); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(windows, &l.Windows)
		out = append(out, l)
	}
	return out, rows.Err()
}

func dumpCatalogProductEdges(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.Product) error {
	rows, err := database.Qx(ctx).Query(ctx, `
	SELECT pi.product_id, p.key
	FROM openrails.product_includes pi
	JOIN openrails.products p ON p.id = pi.included_product_id
	WHERE pi.merchant_id = $1
	ORDER BY pi.product_id, p.key`, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog product includes: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return err
		}
		if p := byID[id]; p != nil {
			p.Includes = append(p.Includes, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = database.Qx(ctx).Query(ctx, `
SELECT product_id, usage_limit_key
FROM openrails.product_usage_limits
WHERE merchant_id = $1
ORDER BY product_id, usage_limit_key`, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog product usage limits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return err
		}
		if p := byID[id]; p != nil {
			p.UsageLimits = append(p.UsageLimits, key)
		}
	}
	return rows.Err()
}

func dumpCatalogPrices(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.Product) error {
	// Metered pricing dumps as rate cards (#707): legacy metered: declarations
	// are translated at push time, so no price-attached metered shape exists.
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT p.product_id, p.amount, p.currency, p.access_duration_hours, p.auto_renew,
       p.trial_unit_amount, p.trial_duration_hours, COALESCE(p.psp_links, '{}'::jsonb),
       p.archived
FROM openrails.prices p
WHERE p.merchant_id = $1
ORDER BY p.product_id, p.amount, p.currency`, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog prices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			productID               uuid.UUID
			price                   catalog.Price
			accessHours, trialHours sql.NullInt64
			trialAmount             sql.NullInt64
			railsRaw                []byte
		)
		if err := rows.Scan(&productID, &price.UnitAmount, &price.Currency, &accessHours, &price.AutoRenew, &trialAmount, &trialHours, &railsRaw, &price.Archived); err != nil {
			return err
		}
		if p := byID[productID]; p != nil {
			if accessHours.Valid {
				price.Duration = hoursSpec(int(accessHours.Int64))
			}
			if trialAmount.Valid && trialHours.Valid {
				price.Trial = &catalog.PriceTrial{UnitAmount: trialAmount.Int64, Duration: hoursSpec(int(trialHours.Int64))}
			}
			price.PSPLinks = providerLinks(railsRaw)
			for provider := range price.PSPLinks {
				price.PSPs = append(price.PSPs, provider)
			}
			sort.Strings(price.PSPs)
			p.Prices = append(p.Prices, price)
		}
	}
	return rows.Err()
}

func dumpCatalogRateCards(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.Product) error {
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT product_id, meter_key, payment_term, filter, allowance, price
FROM openrails.catalog_rate_cards
WHERE merchant_id = $1
ORDER BY product_id, ordinal`, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog rate cards: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			productID    uuid.UUID
			rc           catalog.RateCard
			meterKey     sql.NullString
			filterRaw    []byte
			allowanceRaw []byte
			priceRaw     []byte
		)
		if err := rows.Scan(&productID, &meterKey, &rc.PaymentTerm, &filterRaw, &allowanceRaw, &priceRaw); err != nil {
			return err
		}
		if meterKey.Valid {
			rc.Meter = meterKey.String
		}
		_ = json.Unmarshal(filterRaw, &rc.Filter)
		if len(allowanceRaw) > 0 {
			var a catalog.Allowance
			if err := json.Unmarshal(allowanceRaw, &a); err != nil {
				return fmt.Errorf("decode rate-card allowance: %w", err)
			}
			rc.Allowance = &a
		}
		if err := json.Unmarshal(priceRaw, &rc.Price); err != nil {
			return fmt.Errorf("decode rate-card price: %w", err)
		}
		if p := byID[productID]; p != nil {
			p.RateCards = append(p.RateCards, rc)
		}
	}
	return rows.Err()
}

func dumpCatalogCreditPurchasePrices(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.Product) error {
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT product_id, credit_key, currency, rails, input_min, input_max, price
FROM openrails.catalog_credit_purchase_prices
WHERE merchant_id = $1
ORDER BY product_id, ordinal`, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog credit purchase prices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			productID uuid.UUID
			creditKey string
			price     catalog.Price
			priceRaw  []byte
		)
		if err := rows.Scan(&productID, &creditKey, &price.Currency, &price.PSPs, &price.InputMin, &price.InputMax, &priceRaw); err != nil {
			return err
		}
		if err := json.Unmarshal(priceRaw, &price); err != nil {
			return fmt.Errorf("decode credit purchase price: %w", err)
		}
		if p := byID[productID]; p != nil {
			hasCredit := false
			for _, c := range p.Credits {
				if c.Key == creditKey {
					hasCredit = true
					break
				}
			}
			if !hasCredit {
				p.Credits = append(p.Credits, catalog.CreditGrant{Key: creditKey})
			}
			p.Prices = append(p.Prices, price)
		}
	}
	return rows.Err()
}

func entitlementKeys(raw []byte) []string {
	var m map[string]*int
	_ = json.Unmarshal(raw, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func creditGrants(raw []byte) []catalog.CreditGrant {
	var m map[string]struct {
		Unit        string `json:"unit"`
		Amount      int64  `json:"amount"`
		ExpiryHours *int   `json:"expiry_hours"`
		Cadence     string `json:"cadence"`
	}
	_ = json.Unmarshal(raw, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]catalog.CreditGrant, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		amount := v.Amount
		expires := ""
		if v.ExpiryHours != nil {
			expires = hoursSpec(*v.ExpiryHours)
		}
		out = append(out, catalog.CreditGrant{
			Key:         k,
			Unit:        v.Unit,
			Amount:      &amount,
			ExpiryHours: v.ExpiryHours,
			Expires:     expires,
			Cadence:     v.Cadence,
		})
	}
	return out
}

func providerLinks(raw []byte) map[string]map[string]string {
	var links map[string]map[string]string
	_ = json.Unmarshal(raw, &links)
	if len(links) == 0 {
		return nil
	}
	// The stored blob is account-keyed with the rail stamped inside each
	// entry; the manifest derives the rail from the account key, so the stamp
	// is storage detail, not manifest content.
	for psp, cfg := range links {
		delete(cfg, models.RailKeyRail)
		if strings.EqualFold(strings.TrimSpace(psp), string(models.RailSolana)) {
			// mint_symbol is the resolved on-chain snapshot. The push manifest
			// declares token only when selecting a new non-default plan, so
			// never emit snapshot metadata as input. A stored plan_pda is
			// authoritative for an attached plan and resolves its token from
			// chain, so emitting token beside it would duplicate that fact.
			delete(cfg, "mint_symbol")
			if strings.TrimSpace(cfg["plan_pda"]) != "" ||
				strings.EqualFold(strings.TrimSpace(cfg["token"]), "USDC") {
				delete(cfg, "token")
			}
		}
	}
	return links
}

func hoursSpec(hours int) string {
	if hours == 0 {
		return ""
	}
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dh", hours)
}

func secondsSpec(seconds int64) string {
	if seconds == 0 {
		return ""
	}
	if seconds%86400 == 0 {
		return fmt.Sprintf("%dd", seconds/86400)
	}
	if seconds%3600 == 0 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	if seconds%60 == 0 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}
