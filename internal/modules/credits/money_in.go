package credits

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Charger performs an off-session (merchant-initiated) charge of a saved
// payment method and returns a processor transaction id. It is implemented by
// the processor layer (Stripe MIT / NMI stored rebill) and faked in tests.
// Issues #239 (prepaid auto-top-up) and #241 (arrears settlement) depend on it.
type Charger interface {
	ChargeSavedMethod(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

type ChargeRequest struct {
	Payer           identity.TenantSubjectID
	Actor           string
	PaymentMethodID uuid.UUID
	AmountCents     int64
	IdempotencyKey  string
	Description     string
}

type ChargeResult struct {
	TransactionID string
	Declined      bool // true = hard decline (don't keep retrying); false+err = transient
}

// Alerter delivers a low-balance notification. Implemented by the notification
// layer; faked in tests (#240).
type Alerter interface {
	LowBalanceAlert(ctx context.Context, payer identity.TenantSubjectID, creditType string, available, threshold int64) error
}

// moneyInAccount is a scanned (settings ⨝ balance ⨝ credit_type) row for the
// money-in workers.
type moneyInAccount struct {
	TenantID        uuid.UUID
	TenantSubjectID uuid.UUID
	CreditTypeID    uuid.UUID
	CreditTypeName  string
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
func (s *CreditsService) belowThresholdAccounts(ctx context.Context) ([]moneyInAccount, error) {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	rows, err := s.db.Gen(ctx).ListBelowThresholdCreditAccounts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]moneyInAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, moneyInAccount{
			TenantID:        r.TenantID,
			TenantSubjectID: r.TenantSubjectID,
			CreditTypeID:    r.CreditTypeID,
			CreditTypeName:  r.CreditTypeName,
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
func (s *CreditsService) RunLowBalanceAlerts(ctx context.Context, alerter Alerter, cooldown time.Duration) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
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
		payer := identity.TenantSubjectID(r.TenantSubjectID)
		if err := alerter.LowBalanceAlert(ctx, payer, r.CreditTypeName, r.Available, r.Threshold); err != nil {
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
func (s *CreditsService) RunAutoTopups(ctx context.Context, charger Charger, cooldown time.Duration) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
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
func (s *CreditsService) topUpAccount(ctx context.Context, charger Charger, r moneyInAccount, cooldown time.Duration, now time.Time) (bool, error) {
	payer := identity.TenantSubjectID(r.TenantSubjectID)
	bucket := now.Truncate(max(cooldown, time.Minute)).Unix()
	episode := fmt.Sprintf("autotopup:%s:%s:%d", r.TenantSubjectID, r.CreditTypeID, bucket)
	depositSourceID := uuid.NewSHA1(uuid.Nil, []byte(episode))

	// If this episode already deposited, we're done (idempotent).
	existing, err := s.GetTransactionBySource(ctx, payer.UUID().String(), r.CreditTypeName, "deposit", "auto_topup", depositSourceID.String())
	if err == nil && existing != nil {
		return false, nil
	}

	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		Payer:           payer,
		Actor:           payer.UUID().String(),
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     *r.TopupAmount,
		IdempotencyKey:  episode,
		Description:     "auto top-up: " + r.CreditTypeName,
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

	// auto_topup_amount_cents is the card charge in cents; the ledger is
	// micro-dollars (1 cent = 10,000 micros).
	if _, err := s.Deposit(ctx, CreditDepositParams{
		TenantSubjectID:           &payer,
		Actor:                     payer.UUID().String(),
		CreditType:                r.CreditTypeName,
		Amount:                    *r.TopupAmount * 10_000,
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

// stampMoneyInTimestamp sets a single timestamp column on the settings row.
func (s *CreditsService) stampMoneyInTimestamp(ctx context.Context, r moneyInAccount, column string, now time.Time) error {
	switch column {
	case "last_alert_at":
		return s.db.Gen(ctx).StampCreditAccountAlertAt(ctx, gen.StampCreditAccountAlertAtParams{
			TenantID: r.TenantID, TenantSubjectID: r.TenantSubjectID, CreditTypeID: r.CreditTypeID, Now: &now,
		})
	case "last_topup_at":
		return s.db.Gen(ctx).StampCreditAccountTopupAt(ctx, gen.StampCreditAccountTopupAtParams{
			TenantID: r.TenantID, TenantSubjectID: r.TenantSubjectID, CreditTypeID: r.CreditTypeID, Now: &now,
		})
	default:
		return fmt.Errorf("credits: unknown money-in timestamp column %q", column)
	}
}
