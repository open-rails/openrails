package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AuthorizeHoldInput is the input to AuthorizeAndHold: the payer (tenant subject), the
// actor (canonical actor for per-actor caps), the estimated charge, and the
// idempotency-keyed source coordinates of the hold.
type AuthorizeHoldInput struct {
	Payer          identity.CustomerID
	Actor          string // canonical: 'serviceToken:<key_id>', 'user:<id>', '<issuer>:<sub>'
	Currency       string // "" => DefaultCurrency (#472)
	EstimateMicros int64
	// Source + SourceID form the idempotency key for the placed hold (typically
	// the request_id). A retry with the same coordinates returns the same hold.
	Source   string
	SourceID string
	// ExpiresAt bounds the hold's lifetime.
	ExpiresAt time.Time
}

// AuthorizeHoldResult is the combined decision + (when allowed) placed hold,
// returned atomically by AuthorizeAndHold.
type AuthorizeHoldResult struct {
	Decision    SpendDecision
	BillingMode string
	// AvailableMicros/OutstandingOwedMicros are the snapshot AS EVALUATED inside the
	// transaction (post-lock, pre-hold), so they reflect the balance the decision
	// was made against.
	AvailableMicros       int64
	OutstandingOwedMicros int64
	// Hold is the placed reservation when Decision.Allowed; nil when denied.
	Hold *models.MoneyTransaction
}

// AuthorizeAndHold evaluates the spend policy + prepaid available-balance gate
// AND, when allowed, places the hold — ALL IN ONE TRANSACTION (issue #235/#247).
//
// Atomicity is what makes this distinct from calling CheckSpendAllowed then Hold
// separately: the balance row is locked FOR UPDATE at the top of the tx, and the
// policy evaluation (which counts active holds + windowed spend) and the hold
// insert both run while that lock is held. Two concurrent authorizes on the same
// (tenant, payer, credit_type) therefore serialize on the row lock — the second
// sees the first's held_balance and active-hold exposure, so they cannot both
// pass on the same available balance.
func (s *MoneyService) AuthorizeAndHold(ctx context.Context, in AuthorizeHoldInput) (*AuthorizeHoldResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if in.Payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if in.EstimateMicros < 0 {
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
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
		now := s.now()

		// Idempotency: an existing hold for these coordinates short-circuits — return
		// it as an allowed decision without re-evaluating (the original authorize
		// already passed). Mirrors Hold's idempotency key.
		existingRow, ierr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			TransactionType: "hold", Source: in.Source, SourceID: &in.SourceID,
		})
		if ierr == nil {
			existing, merr := moneyTransactionFromGen(existingRow)
			if merr != nil {
				return merr
			}
			snap, serr := txSvc.snapshotTx(ctx, in.Payer, cur)
			if serr != nil {
				return serr
			}
			result = &AuthorizeHoldResult{
				Decision:              SpendDecision{Allowed: true},
				BillingMode:           snap.billingMode,
				AvailableMicros:       snap.available,
				OutstandingOwedMicros: snap.outstanding,
				Hold:                  existing,
			}
			return nil
		}
		if !errors.Is(ierr, pgx.ErrNoRows) {
			return ierr
		}

		// Lock the balance row FOR UPDATE up front: every subsequent read/decision in
		// this tx is serialized behind it for the same (tenant, payer).
		bal, lerr := txSvc.lockBalance(ctx, q, in.Payer, in.Payer.UUID().String(), cur)
		if lerr != nil {
			return lerr
		}
		available := bal.Balance - bal.HeldBalance

		settings, serr := txSvc.getAccountSettings(ctx, in.Payer, cur)
		if serr != nil {
			return serr
		}

		// Spend policy (per-actor + org caps + outstanding ceiling).
		dec, derr := txSvc.CheckSpendAllowed(ctx, in.Payer, strings.TrimSpace(in.Actor), in.EstimateMicros)
		if derr != nil {
			return derr
		}

		// Admit-time balance/credit gate. Prepaid accounts gate on available balance
		// (unchanged). Arrears accounts (#489): when an admin has set a credit line
		// (credit_limit_micros > 0) the balance may go NEGATIVE only up to it, so a
		// hold is allowed while estimate <= available + credit_limit and denied
		// insufficient_credit otherwise. credit_limit=0 (the default) is OFF and
		// preserves the EXISTING arrears behavior (#302): no admit-time balance gate
		// — a hold may exceed the balance and spill to owed at capture, bounded only
		// by the outstanding ceiling (MaxOutstandingOwed, inside CheckSpendAllowed).
		//
		// NOTE (design choice): #489's literal "credit_limit=0 ⇒ prepaid behavior"
		// would block #302's arrears-spill-past-balance, a real existing capability.
		// We instead make 0 = OFF (existing behavior unchanged) and a positive limit
		// = the new explicit admit-time credit ceiling. Prepaid is unaffected either
		// way.
		switch {
		case settings.BillingMode != BillingModeArrears:
			if in.EstimateMicros > available {
				dec.Allowed = false
				if dec.DenyCode == "" {
					dec.DenyCode = DenyInsufficientBalance
				}
			}
		case settings.CreditLimitMicros > 0:
			if in.EstimateMicros > available+settings.CreditLimitMicros {
				dec.Allowed = false
				if dec.DenyCode == "" {
					dec.DenyCode = DenyInsufficientCredit
				}
			}
		}

		res := &AuthorizeHoldResult{
			Decision:              dec,
			BillingMode:           settings.BillingMode,
			AvailableMicros:       available,
			OutstandingOwedMicros: settings.OutstandingOwedMicros,
		}

		if !dec.Allowed {
			result = res
			return nil
		}

		// Place the hold within the SAME tx + held lock.
		amount := in.EstimateMicros
		if err := q.SetMoneyHeldBalance(ctx, gen.SetMoneyHeldBalanceParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			HeldBalance: bal.HeldBalance + amount, UpdatedAt: now,
		}); err != nil {
			return err
		}

		exp := in.ExpiresAt.UTC()
		auth := amount
		srcID := in.SourceID
		hold := &models.MoneyTransaction{
			ID:              uuidutil.NewV7(),
			MerchantID:      tenantID,
			CustomerID:      payerID,
			Currency:        cur,
			Actor:           strings.TrimSpace(in.Actor),
			Amount:          0,
			Source:          in.Source,
			SourceID:        &srcID,
			Status:          "active",
			Authorized:      &auth,
			ExpiresAt:       &exp,
			TransactionType: "hold",
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(hold)); err != nil {
			return err
		}
		res.Hold = hold
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
// admin-set credit line would exceed credit_limit_micros (the negative-balance
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
		outstanding: settings.OutstandingOwedMicros,
	}, nil
}
