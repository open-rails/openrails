package money

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
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
	// CapturedStoredCredentialRef is the rail-scoped stored-credential replay
	// reference this charge established for the instrument's UNSCHEDULED
	// agreement sequence (#297) — set when the instrument had none (first use
	// or legacy). ScopedCharger persists it write-once; "" = nothing captured.
	CapturedStoredCredentialRef string
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
			TopupAmount:     r.AutoTopupAmount,
			PaymentMethodID: r.AutoTopupPaymentMethodID,
			LastTopupAt:     r.LastTopupAt,
		})
	}
	return out, nil
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
	// PspID is the account that vaulted the instrument, and therefore the one
	// that will take the charge (or#893).
	PspID         uuid.UUID
	EpisodeAnchor string
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
			PspID:           method.PspID,
			EpisodeAnchor:   anchor,
		})
	}
	return out, nil
}
