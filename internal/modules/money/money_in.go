package money

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// Charger performs an off-session (merchant-initiated) charge of a saved
// payment method and returns a rail transaction id. It is implemented by
// the rail layer (Stripe MIT / NMI stored rebill) and faked in tests.
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
	// AmountCents is RAIL MINOR UNITS (typed Cents, #671): cents for USD/EUR,
	// whole yen for zero-decimal JPY — always produced via NativeToRailMinor,
	// never by an inline /10_000 or /100.
	AmountCents    moneyutil.Cents
	Currency       string
	IdempotencyKey string
	Description    string
}

type ChargeResult struct {
	Rail              string
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

// AutoTopupCandidate is one account due an auto-top-up episode (#674): below
// threshold, enabled, out of cooldown. EpisodeAnchor derives from the DURABLE
// last_topup_at stamp (never wall clock), so every retry of one episode maps
// onto ONE intent; the anchor only advances when a finalized episode stamps
// the account.
type AutoTopupCandidate struct {
	MerchantID      uuid.UUID
	CustomerID      uuid.UUID
	Currency        string
	AmountNative    int64
	PaymentMethodID uuid.UUID
	Rail            string
	EpisodeAnchor   string
}

// ListDueAutoTopups scans for accounts due an auto-top-up episode. The charge
// itself is a durable topup_charge provider intent (#674) driven by the
// AutoTopupWorker; this is the read side only. (#239)
func (s *MoneyService) ListDueAutoTopups(ctx context.Context, cooldown time.Duration) ([]AutoTopupCandidate, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	rows, err := s.belowThresholdAccounts(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []AutoTopupCandidate
	for _, r := range rows {
		if !r.AutoTopup || r.TopupAmount == nil || *r.TopupAmount <= 0 || r.PaymentMethodID == nil {
			continue
		}
		if r.LastTopupAt != nil && now.Sub(*r.LastTopupAt) < cooldown {
			continue
		}
		// #474 invariant: auto-topup charges a card → external-currency-only.
		if err := RequireBillingCurrency(normalizeCurrency(r.Currency)); err != nil {
			continue
		}
		method, merr := s.db.Gen(ctx).GetPaymentMethodByID(ctx, *r.PaymentMethodID)
		if merr != nil {
			log.WithContext(ctx).WithError(merr).WithField("payment_method_id", *r.PaymentMethodID).
				Warn("auto-topup: load payment method for candidate")
			continue
		}
		anchor := "genesis"
		if r.LastTopupAt != nil {
			anchor = strconv.FormatInt(r.LastTopupAt.UTC().Unix(), 10)
		}
		out = append(out, AutoTopupCandidate{
			MerchantID:      r.MerchantID,
			CustomerID:      r.CustomerID,
			Currency:        normalizeCurrency(r.Currency),
			AmountNative:    *r.TopupAmount,
			PaymentMethodID: *r.PaymentMethodID,
			Rail:            normalizeRail(method.Rail),
			EpisodeAnchor:   anchor,
		})
	}
	return out, nil
}

// HasAutoTopupDeposit reports whether the episode's deposit (source_id) is
// already on the ledger. The deposit's idempotency key is the credit GRANT's
// (merchant, customer, source_id) — the same lookup depositTx dedupes on.
func (s *MoneyService) HasAutoTopupDeposit(ctx context.Context, customerID uuid.UUID, currency, sourceID string) (bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	_, err = s.db.Gen(ctx).GetCreditGrantBySourceID(ctx, gen.GetCreditGrantBySourceIDParams{
		MerchantID: tid.UUID(), CustomerID: customerID, SourceID: sourceID,
	})
	if err != nil {
		if repo.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FinalizeAutoTopup deposits a CONFIRMED top-up charge (idempotent on the
// unique (merchant,customer,currency,source,source_id)) and stamps
// last_topup_at, advancing the episode anchor.
func (s *MoneyService) FinalizeAutoTopup(ctx context.Context, customerID uuid.UUID, currency string, amountNative int64, sourceID string) error {
	payer := identity.CustomerID(customerID)
	deposited, err := s.HasAutoTopupDeposit(ctx, customerID, currency, sourceID)
	if err != nil {
		return err
	}
	if !deposited {
		if _, err := s.Deposit(ctx, DepositParams{
			CustomerID:                &payer,
			Invoker:                   payer.UUID().String(),
			Currency:                  currency,
			Amount:                    amountNative,
			Source:                    "auto_topup",
			SourceID:                  &sourceID,
			ApplyAccountExpiryDefault: true,
		}); err != nil {
			return err
		}
	}
	return s.StampAutoTopupAttempt(ctx, customerID, currency)
}

// StampAutoTopupAttempt stamps last_topup_at now — the cooldown after a
// finalized episode OR a declined charge (so a failing card is not hammered
// every tick; the alerter still surfaces the low balance).
func (s *MoneyService) StampAutoTopupAttempt(ctx context.Context, customerID uuid.UUID, currency string) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	return s.db.Gen(ctx).StampMoneyAccountTopupAt(ctx, gen.StampMoneyAccountTopupAtParams{
		MerchantID: tid.UUID(), CustomerID: customerID, Currency: normalizeCurrency(currency), Now: &now,
	})
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
