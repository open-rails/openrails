package hosttools

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

type CatalogDumpOptions struct {
	NameAuthority merchant.NameAuthority
	Config        *config.Config
	PGXPool       *pgxpool.Pool
	Merchant      string
	ApplicationID string
	Out           io.Writer
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
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return err
	}
	directory.WithNameAuthority(opts.NameAuthority)
	mctx, _, err := catalogMerchantContext(ctx, directory, opts.Merchant)
	if err != nil {
		return err
	}
	var manifest *catalog.Application
	if err := database.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
		snapshot := database.NewWithPgxTx(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		var revision int64
		if err := snapshot.Qx(ctx).QueryRow(ctx, `SELECT catalog_revision FROM openrails.merchants WHERE id=$1 FOR SHARE`, mid.UUID()).Scan(&revision); err != nil {
			return err
		}
		manifest, err = dumpCatalogManifest(ctx, snapshot)
		if err == nil {
			manifest.ExpectedRevision = &revision
			manifest.ApplicationID = opts.ApplicationID
			if manifest.ApplicationID == "" {
				manifest.ApplicationID = uuid.NewString()
			}
			err = manifest.Validate()
		}
		return err
	}); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	raw, err := yaml.JSONToYAML(encoded)
	if err != nil {
		return fmt.Errorf("marshal catalog manifest: %w", err)
	}
	_, err = out.Write(raw)
	return err
}

func dumpCatalogManifest(ctx context.Context, database *db.DB) (*catalog.Application, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	m := &catalog.Application{SchemaVersion: catalog.ApplicationSchemaVersion}
	productIDs, byID, err := dumpCatalogProducts(ctx, database, tid.UUID())
	if err != nil {
		return nil, err
	}
	if m.Meters, err = dumpCatalogMeters(ctx, database, tid.UUID()); err != nil {
		return nil, err
	}
	if err := dumpCatalogPrices(ctx, database, tid.UUID(), byID); err != nil {
		return nil, err
	}
	if err := dumpCatalogRateCards(ctx, database, tid.UUID(), byID); err != nil {
		return nil, err
	}
	for _, id := range productIDs {
		if p := byID[id]; p != nil {
			m.Products = append(m.Products, *p)
		}
	}
	return m, nil
}

func dumpCatalogProducts(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]uuid.UUID, map[uuid.UUID]*catalog.ApplyProduct, error) {
	rows, err := database.Qx(ctx).Query(ctx, `
	SELECT id, key, display_name, COALESCE(description, ''), entitlements_spec,
	       tier_group, COALESCE(tier_rank, 0), archived
	FROM openrails.products
	WHERE merchant_id = $1 AND NOT archived
	  AND catalog_id IN (SELECT id FROM openrails.catalogs WHERE merchant_id=$1 AND owner_subject IS NULL)
	ORDER BY COALESCE(tier_group, ''), tier_rank, key`, merchantID)
	if err != nil {
		return nil, nil, fmt.Errorf("list catalog products: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	byID := map[uuid.UUID]*catalog.ApplyProduct{}
	for rows.Next() {
		var (
			id              uuid.UUID
			p               catalog.ApplyProduct
			entitlementsRaw []byte
			tierGroup       sql.NullString
		)
		if err := rows.Scan(&id, &p.Key, &p.DisplayName.Value, &p.Description.Value, &entitlementsRaw, &tierGroup, &p.TierRank.Value, &p.Archived.Value); err != nil {
			return nil, nil, err
		}
		p.DisplayName.Set, p.Description.Set, p.TierRank.Set, p.Archived.Set = true, true, true, true
		p.TierGroup = catalog.Null[string]()
		if tierGroup.Valid {
			p.TierGroup = catalog.Value(tierGroup.String)
		}
		p.EntitlementsSpec.Set = true
		p.RateCards = catalog.Value([]catalog.RateCard{})
		if len(entitlementsRaw) == 0 || string(entitlementsRaw) == "null" {
			p.EntitlementsSpec = catalog.Null[map[string]*int]()
		} else if err := json.Unmarshal(entitlementsRaw, &p.EntitlementsSpec.Value); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		cp := p
		byID[id] = &cp
	}
	return ids, byID, rows.Err()
}

func dumpCatalogMeters(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]catalog.ApplyMeter, error) {
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
	var out []catalog.ApplyMeter
	for rows.Next() {
		var m catalog.ApplyMeter
		var groupBy []byte
		if err := rows.Scan(&m.Key, &m.EventType.Value, &m.ValueProperty.Value, &m.Aggregation.Value, &m.Unit.Value, &groupBy); err != nil {
			return nil, err
		}
		m.EventType.Set, m.ValueProperty.Set, m.Aggregation.Set, m.Unit.Set, m.GroupBy.Set = true, true, true, true, true
		if err := json.Unmarshal(groupBy, &m.GroupBy.Value); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func dumpCatalogPrices(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.ApplyProduct) error {
	// Metered pricing dumps as rate cards (#707): legacy metered: declarations
	// are translated at push time, so no price-attached metered shape exists.
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT p.product_id, p.key, p.amount, p.currency, p.access_duration_hours, p.auto_renew,
       p.trial_unit_amount, p.trial_duration_hours, COALESCE((SELECT jsonb_object_agg(COALESCE(psp.key, psp.id::text), binding.configuration || jsonb_strip_nulls(jsonb_build_object(
           'psp_id', psp.id::text, 'rail', psp.rail, 'plan_id', binding.plan_id, 'price_id', binding.price_ref,
           'recurring_billing_option_id', binding.recurring_billing_option_id, 'plan_pda', binding.plan_pda, 'flex_id', binding.flex_id)))
           FROM openrails.price_psp_bindings binding JOIN openrails.psps psp ON psp.id = binding.psp_id AND psp.merchant_id = binding.merchant_id
           WHERE binding.price_id = p.id AND binding.merchant_id = p.merchant_id), '{}'::jsonb),
       p.archived
FROM openrails.prices p
WHERE p.merchant_id = $1 AND NOT p.archived
ORDER BY p.product_id, p.amount, p.currency`, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog prices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			productID               uuid.UUID
			price                   catalog.ApplyPrice
			accessHours, trialHours sql.NullInt64
			trialAmount             sql.NullInt64
			railsRaw                []byte
		)
		if err := rows.Scan(&productID, &price.Key, &price.UnitAmount.Value, &price.Currency.Value, &accessHours, &price.AutoRenew.Value, &trialAmount, &trialHours, &railsRaw, &price.Archived.Value); err != nil {
			return err
		}
		if p := byID[productID]; p != nil {
			price.UnitAmount.Set, price.Currency.Set, price.AutoRenew.Set, price.Archived.Set = true, true, true, true
			price.AccessDurationHours = catalog.Null[int]()
			if accessHours.Valid {
				price.AccessDurationHours = catalog.Value(int(accessHours.Int64))
			}
			price.TrialUnitAmount, price.TrialDurationHours = catalog.Null[int64](), catalog.Null[int]()
			if trialAmount.Valid && trialHours.Valid {
				price.TrialUnitAmount, price.TrialDurationHours = catalog.Value(trialAmount.Int64), catalog.Value(int(trialHours.Int64))
			}
			links := providerLinks(railsRaw)
			if links == nil {
				links = map[string]map[string]string{}
			}
			price.PSPLinks = catalog.Value(links)
			price.PSPs = catalog.Value([]string{})
			for provider := range price.PSPLinks.Value {
				price.PSPs.Value = append(price.PSPs.Value, provider)
			}
			sort.Strings(price.PSPs.Value)
			p.Prices = append(p.Prices, price)
		}
	}
	return rows.Err()
}

func dumpCatalogRateCards(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.ApplyProduct) error {
	rows, err := database.Qx(ctx).Query(ctx, `
SELECT product_id, ordinal, meter_key, payment_term, filter, allowance, price
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
		if err := rows.Scan(&productID, &rc.Ordinal, &meterKey, &rc.PaymentTerm, &filterRaw, &allowanceRaw, &priceRaw); err != nil {
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
			p.RateCards.Value = append(p.RateCards.Value, rc)
		}
	}
	return rows.Err()
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
