package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// suspension.go — legacy account-suspension + payment-method-verification
// state. This slice records account metadata only; service admission does not
// read it.
//
// Payment-method verification: callers may record that a collection method was
// verified after they complete their own rail-specific setup. Borrowing
// capacity is computed elsewhere and should be consumed as credit capacity by
// the hot path.
//
// Suspension: Suspend/Resume toggle suspended_at + suspend_reason for legacy
// account metadata. This is intentionally not a service-admit gate.

// SetPaymentMethodVerified records whether (payer, currency) has a verified
// payment method. When verified is true, verified_at is stamped with now; when
// false, verified_at is cleared. Upserts the settings row (prepaid default) if
// one does not yet exist.
//
// NOTE: this only RECORDS the flag. The actual $1 auth-and-void verification
// charge (needing a Charger.AuthorizeAndVoid capability) is out of this slice;
// a caller flips this after running that flow successfully.
func (s *MoneyService) SetPaymentMethodVerified(ctx context.Context, payer identity.CustomerID, currency string, verified bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SetMoneyAccountPaymentVerified(ctx, gen.SetMoneyAccountPaymentVerifiedParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			Verified: verified, Now: now,
		})
	})
}

// Suspend records legacy account-freeze metadata: stamps suspended_at=now and
// records suspend_reason. Upserts the settings row if one does not yet exist.
// Service admission does not consult this flag.
func (s *MoneyService) Suspend(ctx context.Context, payer identity.CustomerID, currency, reason string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()
	reason = strings.TrimSpace(reason)

	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SuspendMoneyAccount(ctx, gen.SuspendMoneyAccountParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			Now: now, Reason: nilIfEmpty(reason),
		})
	})
}

// Resume clears the suspension on (payer, currency): nulls suspended_at and
// suspend_reason. No-op (other than touching updated_at) if not suspended.
func (s *MoneyService) Resume(ctx context.Context, payer identity.CustomerID, currency string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.ResumeMoneyAccount(ctx, gen.ResumeMoneyAccountParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur, UpdatedAt: now,
		})
	})
}

// IsSuspended reports whether (payer, currency) is currently suspended
// (suspended_at set). An payer with no settings row is not suspended.
func (s *MoneyService) IsSuspended(ctx context.Context, payer identity.CustomerID, currency string) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return false, err
	}
	return settings.SuspendedAt != nil, nil
}

// IsPaymentMethodVerified reports whether (payer, currency) has a verified
// payment method. An payer with no settings row is not verified.
func (s *MoneyService) IsPaymentMethodVerified(ctx context.Context, payer identity.CustomerID, currency string) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return false, err
	}
	return settings.VerifiedPaymentMethod, nil
}

// ArrearsRequiresVerification reports whether an account is on a credit line
// (arrears) but has not recorded a verified payment method. This is a legacy
// metadata query; service admission consumes computed credit capacity instead
// of calling this hot-path check.
func (s *MoneyService) ArrearsRequiresVerification(ctx context.Context, payer identity.CustomerID, currency string) (bool, error) {
	settings, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return false, err
	}
	return settings.BillingMode == BillingModeArrears && !settings.VerifiedPaymentMethod, nil
}
