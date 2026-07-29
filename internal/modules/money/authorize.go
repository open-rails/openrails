package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AuthorizeHoldInput is the input to AuthorizeAndHold: the payer, invoker
// provenance, estimated charge, and idempotency-keyed source coordinates of the
// hold.
type AuthorizeHoldInput struct {
	Payer           identity.CustomerID
	Invoker         string // canonical: 'apiKey:<key_id>', 'user:<id>', '<issuer>:<sub>'
	Currency        string
	EstimatedAmount int64
	// Source + SourceID are the durable provenance/idempotency coordinates used
	// later when the admitted request is captured. SourceID is the caller's
	// request_id.
	Source   string
	SourceID string
	// ExpiresAt bounds the Redis hold's lifetime; this package only validates it.
	ExpiresAt time.Time
}

// AuthorizeHoldResult is the account-capacity decision plus its snapshot. The
// actual in-flight hold lives in Redis, keyed by merchant +
// request_id, not in money_transactions.
type AuthorizeHoldResult struct {
	Decision    SpendDecision
	BillingMode string
	Currency    string
	// AvailableAmount/OutstandingOwedAmount are the snapshot AS EVALUATED inside the
	// transaction (post-lock, pre-hold), so they reflect the balance the decision
	// was made against.
	AvailableAmount       int64
	OutstandingOwedAmount int64
	// AccountCapacityAmount is prepaid availability plus remaining credit
	// capacity before subtracting active Redis holds.
	AccountCapacityAmount int64
}

// AuthorizeAndHold evaluates prepaid/arrears account capacity under the
// per-customer spend lock. It does not insert a money_transactions hold row.
// In-flight reservations are Redis state owned by the service layer.
func (s *MoneyService) AuthorizeAndHold(ctx context.Context, in AuthorizeHoldInput) (*AuthorizeHoldResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if in.Payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if in.EstimatedAmount < 0 {
		return nil, fmt.Errorf("estimate must be >= 0")
	}
	in.Source = strings.TrimSpace(in.Source)
	in.SourceID = strings.TrimSpace(in.SourceID)
	if in.Source == "" || in.SourceID == "" {
		return nil, fmt.Errorf("source and source_id (request_id) required")
	}
	if in.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("expires_at required")
	}
	cur := normalizeCurrency(in.Currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}

	var result *AuthorizeHoldResult
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// A tx-scoped service keeps balance/settings/capacity reads on this
		// transaction.
		q := gen.New(tx)
		txSvc := &MoneyService{db: s.db.NewWithPgxTx(tx), clock: s.clock}

		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		payerID := in.Payer.UUID()

		// Lock the balance row FOR UPDATE up front: every subsequent read/decision in
		// this tx is serialized behind it for the same (merchant, payer).
		bal, lerr := txSvc.lockBalance(ctx, q, in.Payer, in.Payer.UUID().String(), cur)
		if lerr != nil {
			return lerr
		}
		available := bal.Balance - bal.HeldBalance

		settings, serr := txSvc.getAccountSettings(ctx, in.Payer, cur)
		if serr != nil {
			return serr
		}

		dec := SpendDecision{Allowed: true, HardStop: true}

		// Account-capacity gate. Prepaid accounts gate on available balance.
		// Arrears accounts gate on prepaid availability plus remaining credit line:
		// credit_limit_amount - invoice-derived exposure. A zero credit line means
		// no arrears capacity; prepaid balance can still admit work.
		accountCapacity := available
		exposure := int64(0)
		switch {
		case settings.BillingMode != BillingModeArrears:
			if in.EstimatedAmount > available {
				dec.Allowed = false
				if dec.DenyCode == "" {
					dec.DenyCode = DenyInsufficientBalance
				}
			}
		default:
			var eerr error
			exposure, eerr = txSvc.arrearsExposureTx(ctx, q, tenantID, payerID, cur)
			if eerr != nil {
				return eerr
			}
			remainingCredit := settings.CreditLimitAmount - exposure
			if remainingCredit < 0 {
				remainingCredit = 0
			}
			accountCapacity = available + remainingCredit
			if in.EstimatedAmount > available+remainingCredit {
				dec.Allowed = false
				if dec.DenyCode == "" {
					dec.DenyCode = DenyInsufficientCredit
				}
			}
		}

		res := &AuthorizeHoldResult{
			Decision:              dec,
			BillingMode:           settings.BillingMode,
			Currency:              cur,
			AvailableAmount:       available,
			OutstandingOwedAmount: exposure,
			AccountCapacityAmount: accountCapacity,
		}
		result = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DenyInsufficientBalance is the deny code when a prepaid account lacks the
// available balance to cover the estimate. Mirrored by the pkg/service facade.
const DenyInsufficientBalance = "insufficient_balance"

// DenyInsufficientCredit is the deny code (#489) when an arrears account with an
// admin-set credit line would exceed credit_limit_amount (the negative-balance
// ceiling) by placing this hold.
const DenyInsufficientCredit = "insufficient_credit"
