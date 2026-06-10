package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// AuthorizeHoldInput is the input to AuthorizeAndHold: the payer (tenant subject), the
// actor (canonical actor for per-actor caps), the credit type, the estimated
// charge, and the idempotency-keyed source coordinates of the hold.
type AuthorizeHoldInput struct {
	Payer          identity.TenantSubjectID
	Actor          string // canonical: 'serviceToken:<key_id>', 'user:<id>', '<issuer>:<sub>'
	CreditType     string
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
	Hold *models.CreditTransaction
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
func (s *CreditsService) AuthorizeAndHold(ctx context.Context, in AuthorizeHoldInput) (*AuthorizeHoldResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	in.CreditType = strings.TrimSpace(in.CreditType)
	if in.CreditType == "" {
		return nil, fmt.Errorf("credit_type required")
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

	var result *AuthorizeHoldResult
	err := s.db.RunInTenantTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		// A tx-scoped service so the policy reads (CheckSpendAllowed via Q(ctx))
		// and the hold placement run on THIS transaction — one atomic unit.
		txSvc := &CreditsService{db: db.NewWithTx(tx), clock: s.clock}

		// Resolve the credit type INSIDE the tenant tx: the GUC is set here, so the
		// lookup is RLS-scoped to the request tenant (under the openrails_app role a
		// lookup outside the tx would be fail-closed -> no rows). #227.
		ct, cterr := txSvc.GetCreditTypeByName(ctx, in.CreditType)
		if cterr != nil {
			return cterr
		}
		if !ct.IsActive {
			return ErrCreditTypeInactive
		}

		tenantID := tenant.FromContextOrDefault(ctx).UUID()
		payerID := in.Payer.UUID()
		now := s.now()

		// Idempotency: an existing hold for these coordinates short-circuits — return
		// it as an allowed decision without re-evaluating (the original authorize
		// already passed). Mirrors Hold's idempotency key.
		existing := new(models.CreditTransaction)
		ierr := tx.NewSelect().Model(existing).
			Where("transaction_type = 'hold'").
			Where("tenant_id = ? AND tenant_subject_id = ?", tenantID, payerID).
			Where("credit_type_id = ?", ct.ID).
			Where("source = ? AND source_id = ?", in.Source, in.SourceID).
			Limit(1).Scan(ctx)
		if ierr == nil {
			snap, serr := txSvc.snapshotTx(ctx, in.Payer, in.CreditType)
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
		if !errors.Is(ierr, sql.ErrNoRows) {
			return ierr
		}

		// Lock the balance row FOR UPDATE up front: every subsequent read/decision in
		// this tx is serialized behind it for the same (tenant, payer, credit_type).
		bal, lerr := txSvc.lockBalance(ctx, tx, in.Payer, in.Payer.UUID().String(), ct.ID)
		if lerr != nil {
			return lerr
		}
		available := bal.Balance - bal.HeldBalance

		settings, serr := txSvc.GetAccountSettings(ctx, in.Payer, in.CreditType)
		if serr != nil {
			return serr
		}

		// Spend policy (per-actor + org caps + outstanding ceiling).
		dec, derr := txSvc.CheckSpendAllowed(ctx, in.Payer, in.CreditType, strings.TrimSpace(in.Actor), in.EstimateMicros)
		if derr != nil {
			return derr
		}

		// Prepaid accounts additionally gate on available balance; arrears accounts
		// are gated by the outstanding ceiling inside CheckSpendAllowed.
		if settings.BillingMode != BillingModeArrears && in.EstimateMicros > available {
			dec.Allowed = false
			if dec.DenyCode == "" {
				dec.DenyCode = DenyInsufficientBalance
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
		if _, err := tx.NewUpdate().Model((*models.CreditBalance)(nil)).
			Set("held_balance = ?", bal.HeldBalance+amount).
			Set("updated_at = ?", now).
			Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payerID, ct.ID).
			Exec(ctx); err != nil {
			return err
		}

		exp := in.ExpiresAt.UTC()
		auth := amount
		srcID := in.SourceID
		hold := &models.CreditTransaction{
			ID:              uuidutil.NewV7(),
			TenantID:        tenantID,
			TenantSubjectID: payerID,
			Actor:           strings.TrimSpace(in.Actor),
			CreditTypeID:    ct.ID,
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
		if _, err := tx.NewInsert().Model(hold).Exec(ctx); err != nil {
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

type accountSnapshot struct {
	billingMode string
	available   int64
	outstanding int64
}

// snapshotTx reads the balance + settings snapshot using the (tx-scoped) service.
func (s *CreditsService) snapshotTx(ctx context.Context, payer identity.TenantSubjectID, creditType string) (accountSnapshot, error) {
	bal, err := s.GetBalanceForTenantSubject(ctx, payer, creditType)
	if err != nil {
		return accountSnapshot{}, err
	}
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return accountSnapshot{}, err
	}
	return accountSnapshot{
		billingMode: settings.BillingMode,
		available:   bal.Balance - bal.HeldBalance,
		outstanding: settings.OutstandingOwedMicros,
	}, nil
}
