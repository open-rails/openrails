package money

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

type CatalogCreditPurchaseQuoteInput struct {
	ProductKey  string
	Currency    string
	Provider    string
	SpendMicros int64
	Credits     int64
}

type CatalogCreditPurchaseQuote struct {
	ProductKey          string
	CreditType          string
	Unit                string
	Currency            string
	PaidAmount          int64
	BaseCredits         int64
	BonusCredits        int64
	TotalCredits        int64
	EffectiveUnitAmount int64
	ExpiresAt           *time.Time
}

type catalogCreditPurchaseRow struct {
	ProductKey   string
	CreditType   string
	Unit         string
	Currency     string
	ExpiresHours *int
	InputMin     int64
	InputMax     int64
	Price        pricing.RatePrice
}

func (s *MoneyService) QuoteCatalogCreditPurchase(ctx context.Context, input CatalogCreditPurchaseQuoteInput) (*CatalogCreditPurchaseQuote, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	productKey := strings.TrimSpace(input.ProductKey)
	if productKey == "" {
		return nil, fmt.Errorf("product_key required")
	}
	if input.SpendMicros < 0 || input.Credits < 0 {
		return nil, fmt.Errorf("spend and credits must be >= 0")
	}
	if (input.SpendMicros == 0) == (input.Credits == 0) {
		return nil, fmt.Errorf("provide exactly one of spend or credits")
	}
	row, err := s.loadCatalogCreditPurchase(ctx, productKey, input.Currency, input.Provider)
	if err != nil {
		return nil, err
	}
	cm := row.Price.ToChargeModel()
	var paid, credits int64
	if input.SpendMicros > 0 {
		credits, paid, err = pricing.QuoteUnitsForSpend(input.SpendMicros, cm)
		if err != nil {
			return nil, err
		}
	} else {
		credits = input.Credits
		paid, err = cm.Rate(credits)
		if err != nil {
			return nil, err
		}
	}
	// input_min/input_max bound the SPEND in micros, regardless of entry
	// direction: for a spend entry that's the entered amount; for a credits entry
	// it's the computed charge. (Checking the raw credit count against a
	// micros bound would be a unit mismatch.)
	spendForBounds := input.SpendMicros
	if spendForBounds == 0 {
		spendForBounds = paid
	}
	if err := checkCreditPurchaseSpend(row, spendForBounds); err != nil {
		return nil, err
	}
	baseCredits, err := baseCreditQuantity(paid, row.Price)
	if err != nil {
		return nil, err
	}
	if baseCredits > credits {
		baseCredits = credits
	}
	bonus := credits - baseCredits
	effective := int64(0)
	if credits > 0 {
		effective = paid / credits
	}
	var expiresAt *time.Time
	if row.ExpiresHours != nil && *row.ExpiresHours > 0 {
		t := s.now().Add(time.Duration(*row.ExpiresHours) * time.Hour)
		expiresAt = &t
	}
	return &CatalogCreditPurchaseQuote{
		ProductKey:          row.ProductKey,
		CreditType:          row.CreditType,
		Unit:                row.Unit,
		Currency:            row.Currency,
		PaidAmount:          paid,
		BaseCredits:         baseCredits,
		BonusCredits:        bonus,
		TotalCredits:        credits,
		EffectiveUnitAmount: effective,
		ExpiresAt:           expiresAt,
	}, nil
}

func (s *MoneyService) DepositCatalogCreditPurchase(ctx context.Context, payer identity.CustomerID, invoker string, input CatalogCreditPurchaseQuoteInput, sourceID string) (*CatalogCreditPurchaseQuote, *models.MoneyTransaction, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, nil, fmt.Errorf("source_id required")
	}
	quote, err := s.QuoteCatalogCreditPurchase(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	desc := fmt.Sprintf("credit_purchase product=%s paid=%d base=%d bonus=%d total=%d", quote.ProductKey, quote.PaidAmount, quote.BaseCredits, quote.BonusCredits, quote.TotalCredits)
	trx, err := s.Deposit(ctx, DepositParams{
		CustomerID:  &payer,
		Invoker:     invoker,
		Currency:    quote.Unit,
		Amount:      quote.TotalCredits,
		Source:      "credit_purchase",
		SourceID:    &sourceID,
		ExpiresAt:   quote.ExpiresAt,
		Description: &desc,
	})
	if err != nil {
		return nil, nil, err
	}
	return quote, trx, nil
}

func (s *MoneyService) loadCatalogCreditPurchase(ctx context.Context, productKey, currency, provider string) (catalogCreditPurchaseRow, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return catalogCreditPurchaseRow{}, err
	}
	currency = strings.ToLower(strings.TrimSpace(currency))
	provider = strings.ToLower(strings.TrimSpace(provider))
	var row catalogCreditPurchaseRow
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Pool().Query(ctx, `
SELECT p.key, cpp.credit_key, cb.unit, cpp.currency, cb.expires_hours, cpp.input_min, cpp.input_max, cpp.price
FROM openrails.catalog_credit_purchase_prices cpp
JOIN openrails.products p
  ON p.id = cpp.product_id AND p.merchant_id = cpp.merchant_id
JOIN openrails.catalog_credit_balances cb
  ON cb.merchant_id = cpp.merchant_id AND cb.key = cpp.credit_key
WHERE cpp.merchant_id = $1
  AND p.key = $2
  AND ($3 = '' OR lower(cpp.currency) = lower($3))
  AND ($4 = '' OR $4 = ANY(cpp.providers))
ORDER BY cpp.ordinal`, tid.UUID(), productKey, currency, provider)
		if err != nil {
			return err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var priceJSON []byte
			if err := rows.Scan(&row.ProductKey, &row.CreditType, &row.Unit, &row.Currency, &row.ExpiresHours, &row.InputMin, &row.InputMax, &priceJSON); err != nil {
				return err
			}
			if err := json.Unmarshal(priceJSON, &row.Price); err != nil {
				return fmt.Errorf("decode credit_purchase %q price: %w", productKey, err)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if count == 0 {
			return pgx.ErrNoRows
		}
		if count > 1 {
			return fmt.Errorf("credit purchase %q has multiple matching offers; specify currency/provider", productKey)
		}
		return nil
	})
	if err != nil {
		return catalogCreditPurchaseRow{}, err
	}
	row.Unit, err = s.qualifyCatalogCreditUnit(ctx, row.Unit)
	if err != nil {
		return catalogCreditPurchaseRow{}, err
	}
	row.Currency = normalizeCurrency(row.Currency)
	return row, nil
}

func (s *MoneyService) qualifyCatalogCreditUnit(ctx context.Context, unit string) (string, error) {
	unit = strings.ToLower(strings.TrimSpace(unit))
	if unit == "" {
		return "", fmt.Errorf("credit purchase unit required")
	}
	if !IsQualifiedUnit(unit) {
		if _, ok := CurrencyScale(unit); ok {
			return normalizeUnit(unit), nil
		}
		tid, err := merchant.Require(ctx)
		if err != nil {
			return "", err
		}
		var slug string
		if err := s.db.Pool().QueryRow(ctx, `SELECT slug FROM openrails.merchants WHERE id = $1`, tid.UUID()).Scan(&slug); err != nil {
			return "", err
		}
		unit = slug + "/" + unit
	}
	return normalizeUnit(unit), nil
}

func checkCreditPurchaseSpend(row catalogCreditPurchaseRow, spend int64) error {
	if spend <= 0 {
		return fmt.Errorf("credit purchase spend must be positive")
	}
	if row.InputMin > 0 && spend < row.InputMin {
		return fmt.Errorf("credit purchase spend %d below minimum %d", spend, row.InputMin)
	}
	if row.InputMax > 0 && spend > row.InputMax {
		return fmt.Errorf("credit purchase spend %d above maximum %d", spend, row.InputMax)
	}
	return nil
}

func baseCreditQuantity(paid int64, price pricing.RatePrice) (int64, error) {
	base := price.ToChargeModel()
	switch price.Model {
	case pricing.ModelPerUnit:
	case pricing.ModelTiered:
		if price.Tiered == nil || len(price.Tiered.Tiers) == 0 {
			return 0, fmt.Errorf("tiered credit purchase requires tiers")
		}
		base = pricing.ChargeModel{
			Kind:       pricing.ModelPerUnit,
			UnitAmount: price.Tiered.Tiers[0].UnitAmount,
			Round:      pricing.RoundDown,
		}
	default:
		return 0, fmt.Errorf("credit purchase model %q is not quotable", price.Model)
	}
	units, _, err := pricing.QuoteUnitsForSpend(paid, base)
	return units, err
}
