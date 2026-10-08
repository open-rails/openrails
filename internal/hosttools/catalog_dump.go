package hosttools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// CatalogDumpOptions exports current, unarchived offers as an editable catalog
// application. Use the merchant archive for immutable identities and history.
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
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return err
	}
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
		_, err = snapshot.Gen(ctx).GetCatalogRevisionForShare(ctx, mid.UUID())
		if err != nil {
			return err
		}
		manifest, err = dumpCatalogManifest(ctx, snapshot)
		if err == nil {
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
	merchantID := tid.UUID()
	productIDs, byID, err := dumpCatalogProducts(ctx, database, merchantID)
	if err != nil {
		return nil, err
	}
	if m.Meters, err = dumpCatalogMeters(ctx, database, merchantID); err != nil {
		return nil, err
	}
	if err := dumpCatalogPrices(ctx, database, merchantID, byID); err != nil {
		return nil, err
	}
	if err := dumpCatalogRateCards(ctx, database, merchantID, byID); err != nil {
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
	rows, err := database.Gen(ctx).ListLiveCatalogProducts(ctx, merchantID)
	if err != nil {
		return nil, nil, fmt.Errorf("list catalog products: %w", err)
	}
	var ids []uuid.UUID
	byID := map[uuid.UUID]*catalog.ApplyProduct{}
	for _, row := range rows {
		p := catalog.ApplyProduct{Key: row.Key}
		p.DisplayName = catalog.Value(row.DisplayName)
		p.Description = catalog.Value(row.Description)
		p.TierRank = catalog.Value(int(row.TierRank))
		p.Archived = catalog.Value(row.Archived)
		p.TierGroup = catalog.Null[string]()
		if row.TierGroup != nil {
			p.TierGroup = catalog.Value(*row.TierGroup)
		}
		p.CreditGrant = catalog.Null[catalog.CreditGrantSpec]()
		if len(row.CreditGrant) > 0 && string(row.CreditGrant) != "null" {
			p.CreditGrant = catalog.Field[catalog.CreditGrantSpec]{Set: true}
			if err := json.Unmarshal(row.CreditGrant, &p.CreditGrant.Value); err != nil {
				return nil, nil, fmt.Errorf("decode product %q credit grant: %w", row.Key, err)
			}
		}
		p.EntitlementsSpec.Set = true
		p.RateCards = catalog.Value([]catalog.RateCard{})
		if len(row.EntitlementsSpec) == 0 || string(row.EntitlementsSpec) == "null" {
			p.EntitlementsSpec = catalog.Null[map[string]*int]()
		} else if err := json.Unmarshal(row.EntitlementsSpec, &p.EntitlementsSpec.Value); err != nil {
			return nil, nil, err
		}
		ids = append(ids, row.ID)
		cp := p
		byID[row.ID] = &cp
	}
	return ids, byID, nil
}

func dumpCatalogMeters(ctx context.Context, database *db.DB, merchantID uuid.UUID) ([]catalog.ApplyMeter, error) {
	rows, err := database.Gen(ctx).ListCatalogMeters(ctx, merchantID)
	if err != nil {
		return nil, fmt.Errorf("list catalog meters: %w", err)
	}
	var out []catalog.ApplyMeter
	for _, row := range rows {
		m := catalog.ApplyMeter{Key: row.Key}
		m.EventType = catalog.Value(row.EventType)
		m.ValueProperty = catalog.Value(row.ValueProperty)
		m.Aggregation = catalog.Value(catalog.Aggregation(row.Aggregation))
		m.Unit = catalog.Value(row.Unit)
		m.GroupBy.Set = true
		if err := json.Unmarshal(row.GroupBy, &m.GroupBy.Value); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func dumpCatalogPrices(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.ApplyProduct) error {
	// Metered pricing dumps as rate cards (#707): legacy metered: declarations
	// are translated at push time, so no price-attached metered shape exists.
	rows, err := database.Gen(ctx).ListLiveCatalogPricesWithPSPLinks(ctx, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog prices: %w", err)
	}
	for _, row := range rows {
		p := byID[row.ProductID]
		if p == nil {
			continue
		}
		price := catalog.ApplyPrice{Key: row.Key}
		price.UnitAmount = catalog.Value(row.Amount)
		price.Currency = catalog.Value(row.Currency)
		price.BillingIntervalHours = catalog.Null[int]()
		if row.BillingIntervalHours != nil {
			price.BillingIntervalHours = catalog.Value(int(*row.BillingIntervalHours))
		}
		price.Archived = catalog.Value(row.Archived)
		price.AccessDurationHours = catalog.Null[int]()
		if row.AccessDurationHours != nil {
			price.AccessDurationHours = catalog.Value(int(*row.AccessDurationHours))
		}
		price.TrialUnitAmount, price.TrialDurationHours = catalog.Null[int64](), catalog.Null[int]()
		if row.TrialUnitAmount != nil && row.TrialDurationHours != nil {
			price.TrialUnitAmount, price.TrialDurationHours = catalog.Value(*row.TrialUnitAmount), catalog.Value(int(*row.TrialDurationHours))
		}
		price.CustomerAmount = catalog.Null[catalog.CustomerAmount]()
		if len(row.CustomerAmount) > 0 && string(row.CustomerAmount) != "null" {
			price.CustomerAmount = catalog.Field[catalog.CustomerAmount]{Set: true}
			if err := json.Unmarshal(row.CustomerAmount, &price.CustomerAmount.Value); err != nil {
				return fmt.Errorf("decode price %q customer amount: %w", row.Key, err)
			}
		}
		links, err := providerLinks(row.PspLinks)
		if err != nil {
			return fmt.Errorf("decode price %q provider links: %w", row.Key, err)
		}
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
	return nil
}

func dumpCatalogRateCards(ctx context.Context, database *db.DB, merchantID uuid.UUID, byID map[uuid.UUID]*catalog.ApplyProduct) error {
	rows, err := database.Gen(ctx).ListCatalogProductRateCards(ctx, merchantID)
	if err != nil {
		return fmt.Errorf("list catalog rate cards: %w", err)
	}
	for _, row := range rows {
		rc := catalog.RateCard{Ordinal: int(row.Ordinal), PaymentTerm: catalog.PaymentTerm(row.PaymentTerm)}
		if row.MeterKey != nil {
			rc.Meter = *row.MeterKey
		}
		if err := json.Unmarshal(row.Filter, &rc.Filter); err != nil {
			return fmt.Errorf("decode rate-card filter: %w", err)
		}
		if len(row.Allowance) > 0 {
			var a catalog.Allowance
			if err := json.Unmarshal(row.Allowance, &a); err != nil {
				return fmt.Errorf("decode rate-card allowance: %w", err)
			}
			rc.Allowance = &a
		}
		if err := json.Unmarshal(row.Price, &rc.Price); err != nil {
			return fmt.Errorf("decode rate-card price: %w", err)
		}
		if p := byID[row.ProductID]; p != nil {
			p.RateCards.Value = append(p.RateCards.Value, rc)
		}
	}
	return nil
}

func providerLinks(raw []byte) (map[string]map[string]string, error) {
	var links map[string]map[string]string
	if err := json.Unmarshal(raw, &links); err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, nil
	}
	// The stored blob is account-keyed with the rail stamped inside each
	// entry; the manifest derives the rail from the account key, so the stamp
	// is storage detail, not manifest content.
	for _, cfg := range links {
		rail := cfg[models.RailKeyRail]
		delete(cfg, models.RailKeyRail)
		if strings.EqualFold(strings.TrimSpace(rail), string(models.RailSolana)) {
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
	return links, nil
}
