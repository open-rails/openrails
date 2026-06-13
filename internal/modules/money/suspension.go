package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// suspension.go — account suspension + payment-method-verification STATE
// (issue #299). This slice RECORDS state only; it does NOT gate admission.
//
// Payment-method verification: the intended verification mechanism is a $1
// auth-and-void charge against the customer's card. That requires a
// `Charger.AuthorizeAndVoid` capability which is OUT of this slice — the
// charge itself lives in a later slice. Here, a caller that has already run a
// successful verification flips the verified flag via SetPaymentMethodVerified.
//
// Suspension: Suspend/Resume toggle suspended_at + suspend_reason. Wiring the
// admission deny path to reject suspended accounts is a SEPARATE slice; the
// foreground gates on IsSuspended.

// SetPaymentMethodVerified records whether (payer, credit_type) has a verified
// payment method. When verified is true, verified_at is stamped with now; when
// false, verified_at is cleared. Upserts the settings row (prepaid default) if
// one does not yet exist.
//
// NOTE: this only RECORDS the flag. The actual $1 auth-and-void verification
// charge (needing a Charger.AuthorizeAndVoid capability) is out of this slice;
// a caller flips this after running that flow successfully.
func (s *MoneyService) SetPaymentMethodVerified(ctx context.Context, payer identity.TenantSubjectID, verified bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, DefaultCurrency, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SetMoneyAccountPaymentVerified(ctx, gen.SetMoneyAccountPaymentVerifiedParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: DefaultCurrency,
			Verified: verified, Now: now,
		})
	})
}

// Suspend marks (payer, credit_type) suspended: stamps suspended_at=now and
// records suspend_reason. Upserts the settings row if one does not yet exist.
// Admission-deny-on-suspended wiring is a separate slice.
func (s *MoneyService) Suspend(ctx context.Context, payer identity.TenantSubjectID, reason string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()
	reason = strings.TrimSpace(reason)

	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, DefaultCurrency, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SuspendMoneyAccount(ctx, gen.SuspendMoneyAccountParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: DefaultCurrency,
			Now: now, Reason: nilIfEmpty(reason),
		})
	})
}

// Resume clears the suspension on (payer, credit_type): nulls suspended_at and
// suspend_reason. No-op (other than touching updated_at) if not suspended.
func (s *MoneyService) Resume(ctx context.Context, payer identity.TenantSubjectID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, DefaultCurrency, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.ResumeMoneyAccount(ctx, gen.ResumeMoneyAccountParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: DefaultCurrency, UpdatedAt: now,
		})
	})
}

// IsSuspended reports whether (payer, credit_type) is currently suspended
// (suspended_at set). An payer with no settings row is not suspended.
func (s *MoneyService) IsSuspended(ctx context.Context, payer identity.TenantSubjectID) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer)
	if err != nil {
		return false, err
	}
	return settings.SuspendedAt != nil, nil
}

// IsPaymentMethodVerified reports whether (payer, credit_type) has a verified
// payment method. An payer with no settings row is not verified.
func (s *MoneyService) IsPaymentMethodVerified(ctx context.Context, payer identity.TenantSubjectID) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer)
	if err != nil {
		return false, err
	}
	return settings.VerifiedPaymentMethod, nil
}

// ArrearsRequiresVerification reports whether an account is on a credit line
// (arrears) but has NOT verified a payment method (#299 PM-on-file gate). When
// true, admission should deny credit-line spend until a method is verified.
func (s *MoneyService) ArrearsRequiresVerification(ctx context.Context, payer identity.TenantSubjectID) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer)
	if err != nil {
		return false, err
	}
	return settings.BillingMode == BillingModeArrears && !settings.VerifiedPaymentMethod, nil
}
