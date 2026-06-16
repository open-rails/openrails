package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AuthorizeHoldInput is the input to AuthorizeAndHold: the payer (merchant subject), the
// invoker (canonical invoker for per-invoker caps), the estimated charge, and the
// idempotency-keyed source coordinates of the hold.
type AuthorizeHoldInput struct {
	Payer           identity.CustomerID
	Invoker         string // canonical: 'serviceToken:<key_id>', 'user:<id>', '<issuer>:<sub>'
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

// AuthorizeHoldResult is the money-policy decision plus the available capacity
// snapshot. The actual in-flight hold lives in Redis, keyed by merchant +
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
	// CapacityAmount is the maximum money exposure this account can start now,
	// before subtracting active Redis holds.
	CapacityAmount int64
}

// AuthorizeAndHold evaluates the spend policy + prepaid/arrears capacity gate
// under the per-customer spend lock. It does not insert a money_transactions
// hold row. In-flight reservations are Redis state owned by the service layer.
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
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}

	var result *AuthorizeHoldResult
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// A tx-scoped service so the policy reads (CheckSpendAllowed via Qx(ctx))
		// and the hold placement run on THIS transaction — one atomic unit.
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

		// Spend policy (per-invoker + org caps + outstanding ceiling).
		dec, derr := txSvc.CheckSpendAllowed(ctx, in.Payer, cur, strings.TrimSpace(in.Invoker), in.EstimatedAmount)
		if derr != nil {
			return derr
		}

		// Admit-time balance/credit gate. Prepaid accounts gate on available balance.
		// Arrears accounts gate on prepaid availability plus remaining credit line:
		// credit_limit_amount - existing owed exposure. A zero credit line means no
		// arrears capacity; prepaid balance can still admit work. During the #506
		// transition, outstanding_owed_amount remains the exposure cache, and open
		// invoice amount_due is used if it is higher so paid/open invoice state can
		// drive admission as we migrate away from the account-level counter.
		capacity := available
		switch {
		case settings.BillingMode != BillingModeArrears:
			if in.EstimatedAmount > available {
				dec.Allowed = false
				if dec.DenyCode == "" {
					dec.DenyCode = DenyInsufficientBalance
				}
			}
		default:
			openDue, oerr := q.SumOpenInvoiceAmountDue(ctx, gen.SumOpenInvoiceAmountDueParams{
				MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			})
			if oerr != nil {
				return oerr
			}
			pendingDue, perr := q.SumPendingInvoiceItemAmount(ctx, gen.SumPendingInvoiceItemAmountParams{
				MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			})
			if perr != nil {
				return perr
			}
			exposure := settings.OutstandingOwedAmount
			invoiceExposure := openDue + pendingDue
			if invoiceExposure > exposure {
				exposure = invoiceExposure
			}
			remainingCredit := settings.CreditLimitAmount - exposure
			if remainingCredit < 0 {
				remainingCredit = 0
			}
			capacity = available + remainingCredit
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
			OutstandingOwedAmount: settings.OutstandingOwedAmount,
			CapacityAmount:        capacity,
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

type accountSnapshot struct {
	billingMode string
	available   int64
	outstanding int64
}

// snapshotTx reads the balance + settings snapshot using the (tx-scoped) service.
func (s *MoneyService) snapshotTx(ctx context.Context, payer identity.CustomerID, currency string) (accountSnapshot, error) {
	cur := normalizeCurrency(currency)
	bal, err := s.GetBalanceForCustomer(ctx, payer, cur)
	if err != nil {
		return accountSnapshot{}, err
	}
	settings, err := s.getAccountSettings(ctx, payer, cur)
	if err != nil {
		return accountSnapshot{}, err
	}
	return accountSnapshot{
		billingMode: settings.BillingMode,
		available:   bal.Balance - bal.HeldBalance,
		outstanding: settings.OutstandingOwedAmount,
	}, nil
}
