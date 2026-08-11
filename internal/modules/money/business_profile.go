package money

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#908: the B2B business-profile record and its two chokepoints.
//
// POSTURE DOCTRINE (upstreamed from tensorhub th#1462): business posture is a
// CONSEQUENCE of onboarding, never a settable flag. The
// customer_business_profiles row IS the posture — there is no boolean to flip,
// so a posture with no terms acceptance behind it is unrepresentable. The only
// way in is OnboardBusinessCustomer (terms acceptance required); the only way
// out is OffboardBusinessCustomer, which refuses while the payer still owes —
// offboarding a debtor would hide the receivable from the arrears cycle, which
// iterates business profiles.

// BusinessProfile is the stored onboarding record. Invoice-document fields
// live on CustomerInvoiceProfile; the credit line lives on money settings —
// this carries what onboarding itself asserts.
type BusinessProfile struct {
	CustomerID            identity.CustomerID `json:"customer_id"`
	TermsVersion          string              `json:"terms_version"`
	TermsAcceptedAt       time.Time           `json:"terms_accepted_at"`
	TermsAcceptedBy       string              `json:"terms_accepted_by,omitempty"`
	KYCReference          string              `json:"kyc_reference,omitempty"`
	Currency              string              `json:"currency"`
	BudgetAlertThresholds []int64             `json:"budget_alert_thresholds"`
	// SuspensionRecommendedAt / SuspensionReason are the or#910 dunning
	// cycle's open RECOMMENDATION episode (a signal — hosts enforce; nil =
	// none open). Written only by the cycle's CAS edges, never by Onboard.
	SuspensionRecommendedAt *time.Time `json:"suspension_recommended_at,omitempty"`
	SuspensionReason        string     `json:"suspension_reason,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

// BusinessOnboarding is the operator onboarding/update payload. TermsVersion +
// TermsAcceptedAt are the terms-acceptance gate business posture hangs on;
// InvoiceProfile and CreditLimit are written through to their own records in
// the same operation so one call leaves the payer fully business-configured.
type BusinessOnboarding struct {
	TermsVersion          string
	TermsAcceptedAt       time.Time
	TermsAcceptedBy       string
	KYCReference          string
	Currency              string
	BudgetAlertThresholds []int64
	InvoiceProfile        CustomerInvoiceProfile
	CreditLimit           int64
}

// ErrBusinessOutstandingBalance refuses an offboard that would hide a
// receivable from the arrears cycle.
var ErrBusinessOutstandingBalance = errors.New("payer has an outstanding arrears balance: settle or write off the open receivables before offboarding")

// OnboardBusinessCustomer grants (or updates) business standing: invoice
// profile + credit line + the business-profile row, one call. Idempotent.
// Operator surface — a payer must not onboard itself.
func (s *MoneyService) OnboardBusinessCustomer(ctx context.Context, payer identity.CustomerID, in BusinessOnboarding) (*BusinessProfile, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	in.TermsVersion = strings.TrimSpace(in.TermsVersion)
	if in.TermsVersion == "" || in.TermsAcceptedAt.IsZero() {
		return nil, fmt.Errorf("terms_version and terms_accepted_at are required (terms acceptance gates business posture)")
	}
	cur := normalizeCurrency(in.Currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	thresholds, err := normalizeThresholds(in.BudgetAlertThresholds)
	if err != nil {
		return nil, err
	}
	if in.CreditLimit < 0 {
		return nil, fmt.Errorf("credit_limit must be >= 0")
	}
	for _, c := range in.InvoiceProfile.BillingContacts {
		if !strings.Contains(c.Email, "@") {
			return nil, fmt.Errorf("billing contact email %q invalid", c.Email)
		}
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	// Sibling records first (both idempotent, each its own chokepoint with its
	// own validation), then the profile row that IS the posture — so a refused
	// invoice profile or credit line never leaves a business customer without
	// them.
	if err := s.SetCustomerInvoiceProfile(ctx, payer, in.InvoiceProfile); err != nil {
		return nil, err
	}
	if err := s.SetCreditLimit(ctx, payer, cur, in.CreditLimit); err != nil {
		return nil, err
	}

	now := s.now()
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := ensureCustomer(ctx, q, tid.UUID(), payer.UUID()); err != nil {
			return err
		}
		return q.UpsertCustomerBusinessProfile(ctx, gen.UpsertCustomerBusinessProfileParams{
			MerchantID:            tid.UUID(),
			CustomerID:            payer.UUID(),
			TermsVersion:          in.TermsVersion,
			TermsAcceptedAt:       in.TermsAcceptedAt.UTC(),
			TermsAcceptedBy:       strings.TrimSpace(in.TermsAcceptedBy),
			KycReference:          strings.TrimSpace(in.KYCReference),
			Currency:              cur,
			BudgetAlertThresholds: thresholds,
			Now:                   now,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetBusinessProfile(ctx, payer)
}

// GetBusinessProfile returns a payer's business-profile record; nil when the
// payer has none (consumer posture).
func (s *MoneyService) GetBusinessProfile(ctx context.Context, payer identity.CustomerID) (*BusinessProfile, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var row gen.OpenrailsCustomerBusinessProfile
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		row, e = s.db.Gen(ctx).GetCustomerBusinessProfile(ctx, gen.GetCustomerBusinessProfileParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(),
		})
		return e
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	p := businessProfileFromRow(row)
	return &p, nil
}

// ListBusinessProfiles returns the merchant's business roster, oldest
// onboarding first.
func (s *MoneyService) ListBusinessProfiles(ctx context.Context, limit int) ([]BusinessProfile, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var rows []gen.OpenrailsCustomerBusinessProfile
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		rows, e = s.db.Gen(ctx).ListCustomerBusinessProfiles(ctx, gen.ListCustomerBusinessProfilesParams{
			MerchantID: tid.UUID(), RowLimit: int64(limit),
		})
		return e
	})
	if err != nil {
		return nil, err
	}
	out := make([]BusinessProfile, 0, len(rows))
	for _, row := range rows {
		out = append(out, businessProfileFromRow(row))
	}
	return out, nil
}

// OffboardBusinessCustomer withdraws business standing: clears the credit
// line and deletes the profile row, which IS the posture downgrade back to
// consumer. Refused while the payer still owes (open/past-due receivables or
// accrued-but-uninvoiced pending items) in the profile's currency.
func (s *MoneyService) OffboardBusinessCustomer(ctx context.Context, payer identity.CustomerID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	profile, err := s.GetBusinessProfile(ctx, payer)
	if err != nil {
		return err
	}
	if profile == nil {
		return nil // already consumer: the posture and the profile cannot disagree
	}
	outstanding, err := s.GetOutstandingOwed(ctx, payer, profile.Currency)
	if err != nil {
		return fmt.Errorf("outstanding balance read: %w", err)
	}
	if outstanding > 0 {
		return fmt.Errorf("%w (%d outstanding in %s)", ErrBusinessOutstandingBalance, outstanding, profile.Currency)
	}
	if err := s.SetCreditLimit(ctx, payer, profile.Currency, 0); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, e := s.db.Gen(ctx).DeleteCustomerBusinessProfile(ctx, gen.DeleteCustomerBusinessProfileParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(),
		})
		return e
	})
}

func businessProfileFromRow(row gen.OpenrailsCustomerBusinessProfile) BusinessProfile {
	thresholds := row.BudgetAlertThresholds
	if thresholds == nil {
		thresholds = []int64{}
	}
	return BusinessProfile{
		CustomerID:              identity.CustomerID(row.CustomerID),
		TermsVersion:            row.TermsVersion,
		TermsAcceptedAt:         row.TermsAcceptedAt,
		TermsAcceptedBy:         row.TermsAcceptedBy,
		KYCReference:            row.KycReference,
		Currency:                row.Currency,
		BudgetAlertThresholds:   thresholds,
		SuspensionRecommendedAt: row.SuspensionRecommendedAt,
		SuspensionReason:        row.SuspensionReason,
		CreatedAt:               row.CreatedAt,
		UpdatedAt:               row.UpdatedAt,
	}
}

// normalizeThresholds validates budget-alert thresholds (positive amounts) and
// returns them ascending, deduplicated — the order the alert ladder fires in.
func normalizeThresholds(in []int64) ([]int64, error) {
	out := make([]int64, 0, len(in))
	seen := map[int64]bool{}
	for _, t := range in {
		if t <= 0 {
			return nil, fmt.Errorf("budget_alert_thresholds must be positive amounts, got %d", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
