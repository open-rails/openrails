package credits

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
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
func (s *CreditsService) SetPaymentMethodVerified(ctx context.Context, payer identity.TenantSubjectID, creditType string, verified bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	payerID := payer.UUID()
	now := s.now()

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.ensureSettingsRowTx(ctx, tx, tenantID, payerID, ct.ID, BillingModePrepaid, now); err != nil {
		return err
	}

	upd := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
		Set("verified_payment_method = ?", verified).
		Set("updated_at = ?", now)
	if verified {
		upd = upd.Set("verified_at = ?", now)
	} else {
		upd = upd.Set("verified_at = NULL")
	}
	if _, err := upd.
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payerID, ct.ID).
		Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// Suspend marks (payer, credit_type) suspended: stamps suspended_at=now and
// records suspend_reason. Upserts the settings row if one does not yet exist.
// Admission-deny-on-suspended wiring is a separate slice.
func (s *CreditsService) Suspend(ctx context.Context, payer identity.TenantSubjectID, creditType, reason string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	payerID := payer.UUID()
	now := s.now()
	reason = strings.TrimSpace(reason)

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.ensureSettingsRowTx(ctx, tx, tenantID, payerID, ct.ID, BillingModePrepaid, now); err != nil {
		return err
	}

	upd := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
		Set("suspended_at = ?", now).
		Set("updated_at = ?", now)
	if reason != "" {
		upd = upd.Set("suspend_reason = ?", reason)
	} else {
		upd = upd.Set("suspend_reason = NULL")
	}
	if _, err := upd.
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payerID, ct.ID).
		Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// Resume clears the suspension on (payer, credit_type): nulls suspended_at and
// suspend_reason. No-op (other than touching updated_at) if not suspended.
func (s *CreditsService) Resume(ctx context.Context, payer identity.TenantSubjectID, creditType string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	payerID := payer.UUID()
	now := s.now()

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.ensureSettingsRowTx(ctx, tx, tenantID, payerID, ct.ID, BillingModePrepaid, now); err != nil {
		return err
	}

	if _, err := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
		Set("suspended_at = NULL").
		Set("suspend_reason = NULL").
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payerID, ct.ID).
		Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// IsSuspended reports whether (payer, credit_type) is currently suspended
// (suspended_at set). An payer with no settings row is not suspended.
func (s *CreditsService) IsSuspended(ctx context.Context, payer identity.TenantSubjectID, creditType string) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return false, err
	}
	return settings.SuspendedAt != nil, nil
}

// IsPaymentMethodVerified reports whether (payer, credit_type) has a verified
// payment method. An payer with no settings row is not verified.
func (s *CreditsService) IsPaymentMethodVerified(ctx context.Context, payer identity.TenantSubjectID, creditType string) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return false, err
	}
	return settings.VerifiedPaymentMethod, nil
}

// ArrearsRequiresVerification reports whether an account is on a credit line
// (arrears) but has NOT verified a payment method (#299 PM-on-file gate). When
// true, admission should deny credit-line spend until a method is verified.
func (s *CreditsService) ArrearsRequiresVerification(ctx context.Context, payer identity.TenantSubjectID, creditType string) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return false, err
	}
	return settings.BillingMode == BillingModeArrears && !settings.VerifiedPaymentMethod, nil
}
