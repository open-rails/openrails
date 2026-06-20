package money

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Charger performs an off-session (merchant-initiated) charge of a saved
// payment method and returns a processor transaction id. It is implemented by
// the processor layer (Stripe MIT / NMI stored rebill) and faked in tests.
// Issues #239 (prepaid auto-top-up) and #241 (arrears settlement) depend on it.
type Charger interface {
	ChargeSavedMethod(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

type ChargeRequest struct {
	MerchantID      uuid.UUID
	Payer           identity.CustomerID
	Invoker         string
	InvoiceID       *uuid.UUID
	PaymentMethodID uuid.UUID
	AmountCents     int64
	Currency        string
	IdempotencyKey  string
	Description     string
}

type ChargeResult struct {
	Processor         string
	TransactionID     string
	ExternalInvoiceID string
	Declined          bool // true = hard decline (don't keep retrying); false+err = transient
	FailureCode       *string
	FailureMessage    *string
}

// Alerter delivers a low-balance notification. Implemented by the notification
// layer; faked in tests (#240).
type Alerter interface {
	LowBalanceAlert(ctx context.Context, payer identity.CustomerID, available, threshold int64) error
}

// moneyInAccount is a scanned (settings ⨝ balance) row for the money-in workers.
type moneyInAccount struct {
	MerchantID      uuid.UUID
	CustomerID      uuid.UUID
	Currency        string
	Available       int64
	Threshold       int64
	AutoTopup       bool
	TopupAmount     *int64
	PaymentMethodID *uuid.UUID
	LastAlertAt     *time.Time
	LastTopupAt     *time.Time
}

// belowThresholdAccounts returns accounts whose available balance
// (balance - held) is under their configured low-balance threshold.
func (s *MoneyService) belowThresholdAccounts(ctx context.Context) ([]moneyInAccount, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	rows, err := s.db.Gen(ctx).ListBelowThresholdMoneyAccounts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]moneyInAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, moneyInAccount{
			MerchantID:      r.MerchantID,
			CustomerID:      r.CustomerID,
			Currency:        r.Currency,
			Available:       r.Available,
			Threshold:       r.Threshold,
			AutoTopup:       r.AutoTopupEnabled,
			TopupAmount:     r.AutoTopupAmountCents,
			PaymentMethodID: r.AutoTopupPaymentMethodID,
			LastAlertAt:     r.LastAlertAt,
			LastTopupAt:     r.LastTopupAt,
		})
	}
	return out, nil
}

// RunLowBalanceAlerts finds accounts below their low-balance threshold and emits
// one alert per account, deduped by last_alert_at within `cooldown`. Returns the
// number of alerts sent. (#240)
func (s *MoneyService) RunLowBalanceAlerts(ctx context.Context, alerter Alerter, cooldown time.Duration) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if alerter == nil {
		return 0, fmt.Errorf("alerter required")
	}
	rows, err := s.belowThresholdAccounts(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	sent := 0
	for _, r := range rows {
		if r.LastAlertAt != nil && now.Sub(*r.LastAlertAt) < cooldown {
			continue
		}
		payer := identity.CustomerID(r.CustomerID)
		if err := alerter.LowBalanceAlert(ctx, payer, r.Available, r.Threshold); err != nil {
			continue // best-effort; try again next tick
		}
		if err := s.stampMoneyInTimestamp(ctx, r, "last_alert_at", now); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// RunAutoTopups finds accounts with auto-top-up enabled whose available balance
// is below the threshold, charges the saved payment method off-session, and
// deposits the purchased credits. Deduped by last_topup_at within `cooldown`,
// and idempotent per top-up episode so a retry cannot double-charge. Returns the
// number of successful top-ups. (#239)
func (s *MoneyService) RunAutoTopups(ctx context.Context, charger Charger, cooldown time.Duration) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if charger == nil {
		return 0, fmt.Errorf("charger required")
	}
	rows, err := s.belowThresholdAccounts(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	count := 0
	for _, r := range rows {
		if !r.AutoTopup || r.TopupAmount == nil || *r.TopupAmount <= 0 || r.PaymentMethodID == nil {
			continue
		}
		if r.LastTopupAt != nil && now.Sub(*r.LastTopupAt) < cooldown {
			continue
		}
		ok, err := s.topUpAccount(ctx, charger, r, cooldown, now)
		if err != nil {
			return count, err
		}
		if ok {
			count++
		}
	}
	return count, nil
}

// topUpAccount performs one account's top-up. The episode key is stable within a
// cooldown window so the charge idempotency key and deposit source_id are
// deterministic: if the deposit already exists for this episode the charge is
// skipped entirely; the Charger also receives the idempotency key so a retry
// after a charge-but-before-deposit crash does not double-charge.
func (s *MoneyService) topUpAccount(ctx context.Context, charger Charger, r moneyInAccount, cooldown time.Duration, now time.Time) (bool, error) {
	payer := identity.CustomerID(r.CustomerID)
	// #474 invariant: auto-topup charges a card → external-currency-only.
	if err := RequireBillingCurrency(normalizeCurrency(r.Currency)); err != nil {
		return false, nil
	}
	bucket := now.Truncate(max(cooldown, time.Minute)).Unix()
	episode := fmt.Sprintf("autotopup:%s:%d", r.CustomerID, bucket)
	// #491: source_id is the natural key string itself (uuidv7 pk + UNIQUE
	// (merchant,customer,currency,source,source_id)); no uuidv5 derivation.
	depositSourceID := episode

	// If this episode already deposited, we're done (idempotent).
	existing, err := s.GetTransactionBySource(ctx, payer.UUID().String(), r.Currency, "deposit", "auto_topup", depositSourceID)
	if err == nil && existing != nil {
		return false, nil
	}

	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		MerchantID:      r.MerchantID,
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     nativeAmountToProcessorMinor(r.Currency, *r.TopupAmount),
		Currency:        normalizeCurrency(r.Currency),
		IdempotencyKey:  episode,
		Description:     "auto top-up",
	})
	if err != nil || res.Declined {
		// Stamp last_topup_at to apply the cooldown so we don't hammer a failing
		// card every tick; the alerter still surfaces the low balance.
		_ = s.stampMoneyInTimestamp(ctx, r, "last_topup_at", now)
		if res.Declined {
			return false, nil
		}
		return false, err
	}

	if _, err := s.Deposit(ctx, DepositParams{
		CustomerID:                &payer,
		Invoker:                   payer.UUID().String(),
		Currency:                  r.Currency,
		Amount:                    *r.TopupAmount,
		Source:                    "auto_topup",
		SourceID:                  &depositSourceID,
		ApplyAccountExpiryDefault: true,
	}); err != nil {
		return false, err
	}
	if err := s.stampMoneyInTimestamp(ctx, r, "last_topup_at", now); err != nil {
		return false, err
	}
	return true, nil
}

func nativeAmountToProcessorMinor(currency string, amount int64) int64 {
	scale, ok := CurrencyScale(currency)
	if !ok {
		return amount
	}
	if scale <= 2 {
		for range 2 - scale {
			amount *= 10
		}
		return amount
	}
	for range scale - 2 {
		amount /= 10
	}
	return amount
}

// stampMoneyInTimestamp sets a single timestamp column on the settings row.
func (s *MoneyService) stampMoneyInTimestamp(ctx context.Context, r moneyInAccount, column string, now time.Time) error {
	switch column {
	case "last_alert_at":
		return s.db.Gen(ctx).StampMoneyAccountAlertAt(ctx, gen.StampMoneyAccountAlertAtParams{
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: normalizeCurrency(r.Currency), Now: &now,
		})
	case "last_topup_at":
		return s.db.Gen(ctx).StampMoneyAccountTopupAt(ctx, gen.StampMoneyAccountTopupAtParams{
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: normalizeCurrency(r.Currency), Now: &now,
		})
	default:
		return fmt.Errorf("money: unknown money-in timestamp column %q", column)
	}
}
