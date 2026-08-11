//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// or#908: business posture is a consequence of onboarding, never a settable
// flag (FC-17). These tests pin the two chokepoints: onboarding requires the
// terms gate, offboarding refuses while the payer owes, and the row's
// presence IS the posture.

func businessCleanup(t *testing.T, ctx context.Context, svc *money.MoneyService, payerID uuid.UUID) {
	t.Helper()
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_business_profiles WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.customer_invoice_profiles WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.ledger_transfers WHERE customer_id = $1", payerID)
	})
}

func TestBusinessProfile_PostureIsConsequenceOfOnboarding(t *testing.T) {
	svc, _, payer, currency, ctx := moneyInEnv(t)
	businessCleanup(t, ctx, svc, payer.UUID())

	// Consumer by default: no record, no posture.
	got, err := svc.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.Nil(t, got)

	acceptedAt := time.Now().UTC().Truncate(time.Second)

	// The terms gate: no terms_version, no business posture.
	_, err = svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsAcceptedAt: acceptedAt, Currency: currency,
	})
	require.Error(t, err)
	_, err = svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-08", Currency: currency,
	})
	require.Error(t, err)
	// Thresholds must be positive amounts.
	_, err = svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-08", TermsAcceptedAt: acceptedAt, Currency: currency,
		BudgetAlertThresholds: []int64{100, 0},
	})
	require.Error(t, err)
	// A refused onboard leaves no posture behind.
	got, err = svc.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.Nil(t, got)

	// A real onboard: profile + invoice profile + credit line, one call.
	p, err := svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion:          "2026-08",
		TermsAcceptedAt:       acceptedAt,
		TermsAcceptedBy:       "cfo@example.com",
		KYCReference:          "kyc-123",
		Currency:              currency,
		BudgetAlertThresholds: []int64{500, 100, 500}, // unsorted + dup, normalized on write
		InvoiceProfile: money.CustomerInvoiceProfile{
			NetTermsDays:     30,
			CollectionMethod: money.CollectionSendInvoice,
			PONumber:         "PO-77",
			BillingContacts:  []models.InvoiceContact{{Name: "AP", Email: "ap@example.com"}},
		},
		CreditLimit: 5_000_000,
	})
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, "2026-08", p.TermsVersion)
	require.Equal(t, []int64{100, 500}, p.BudgetAlertThresholds)

	got, err = svc.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "kyc-123", got.KYCReference)

	ip, err := svc.GetCustomerInvoiceProfile(ctx, payer)
	require.NoError(t, err)
	require.NotNil(t, ip)
	require.Equal(t, 30, ip.NetTermsDays)
	require.Equal(t, money.CollectionSendInvoice, ip.CollectionMethod)

	limit, err := svc.GetCreditLimit(ctx, payer, currency)
	require.NoError(t, err)
	require.Equal(t, int64(5_000_000), limit)

	roster, err := svc.ListBusinessProfiles(ctx, 0)
	require.NoError(t, err)
	found := false
	for _, row := range roster {
		if row.CustomerID == payer {
			found = true
		}
	}
	require.True(t, found, "onboarded payer must appear on the business roster")

	// Onboard again = update, idempotent.
	p2, err := svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-09", TermsAcceptedAt: acceptedAt, Currency: currency,
		CreditLimit: 1_000_000,
	})
	require.NoError(t, err)
	require.Equal(t, "2026-09", p2.TermsVersion)

	// Offboard with nothing owed: posture reverts, credit line zeroed.
	require.NoError(t, svc.OffboardBusinessCustomer(ctx, payer))
	got, err = svc.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.Nil(t, got)
	limit, err = svc.GetCreditLimit(ctx, payer, currency)
	require.NoError(t, err)
	require.Zero(t, limit)

	// Offboarding a consumer is a no-op, not an error.
	require.NoError(t, svc.OffboardBusinessCustomer(ctx, payer))
}

func TestBusinessProfile_OffboardRefusedWhileOwing(t *testing.T) {
	svc, _, payer, currency, ctx := moneyInEnv(t)
	businessCleanup(t, ctx, svc, payer.UUID())

	_, err := svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-08", TermsAcceptedAt: time.Now().UTC(), Currency: currency,
		CreditLimit: 1_000_000,
	})
	require.NoError(t, err)

	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "or908-owed", 400)
	require.NoError(t, err)

	err = svc.OffboardBusinessCustomer(ctx, payer)
	require.ErrorIs(t, err, money.ErrBusinessOutstandingBalance)

	// The refusal preserved everything: still business, credit line intact.
	got, err := svc.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.NotNil(t, got)
	limit, err := svc.GetCreditLimit(ctx, payer, currency)
	require.NoError(t, err)
	require.Equal(t, int64(1_000_000), limit)
}

func TestBusinessProfile_MerchantScoped(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	businessCleanup(t, ctx, svc, payer.UUID())

	_, err := svc.OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion: "2026-08", TermsAcceptedAt: time.Now().UTC(), Currency: currency,
	})
	require.NoError(t, err)

	// A second merchant, its own pinned connection: merchant A's onboarding
	// record must be invisible.
	otherID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active') ON CONFLICT (slug) WHERE deleted_at IS NULL DO NOTHING`,
		otherID, "or908-other-"+otherID.String()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", otherID)
	})
	otherDB := dbtest.OpenMerchantDB(t, otherID)
	otherSvc := money.NewMoneyService(otherDB)
	otherCtx := merchant.WithID(context.Background(), merchant.ID(otherID))

	got, err := otherSvc.GetBusinessProfile(otherCtx, payer)
	require.NoError(t, err)
	require.Nil(t, got, "merchant B must not see merchant A's business profile")

	roster, err := otherSvc.ListBusinessProfiles(otherCtx, 0)
	require.NoError(t, err)
	for _, row := range roster {
		require.NotEqual(t, payer, row.CustomerID)
	}

	// And merchant B offboarding the payer is a no-op that leaves A's record.
	require.NoError(t, otherSvc.OffboardBusinessCustomer(otherCtx, payer))
	got, err = svc.GetBusinessProfile(ctx, payer)
	require.NoError(t, err)
	require.NotNil(t, got)
}
