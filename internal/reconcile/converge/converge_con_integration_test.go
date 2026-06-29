//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511 Phase D (CON plane): an entitlement whose polymorphic source_type/source_id
// pair resolves to no subscription is detected as
// consistency.reference.source_reference — a residual referential-integrity check
// that can't be a Postgres FK (the reference is polymorphic). It is surface-only:
// ADMIN, no auto-repair (a dangling source may be a valid historical record or a
// real corruption — an operator decides), and the entitlement is left untouched.
func TestConverge_ConReferenceSourceReference(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	feature := "feat_" + suffix
	entID := uuid.New()
	danglingSubID := uuid.New() // no such subscription exists
	var customer uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		// An active entitlement pointing at a subscription that does not exist.
		_, err := appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
			 VALUES ($1,$2,$3,$4,$5,$6,'subscription',$7)`,
			entID, customer, feature, time.Now().Add(-24*time.Hour), time.Now().Add(24*time.Hour), danglingSubID, merchantID)
		require.NoError(t, err)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "entitlement:"+entID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE id=$1`, entID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.RequiresReview, "dangling reference is surfaced for review, not auto-fixed")
		require.Equal(t, 0, res.AutoFixed)

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='consistency.reference.source_reference' AND subject_key=$2`,
			merchantID, "entitlement:"+entID.String()).Scan(&status))
		require.Equal(t, "requires_review", status)

		// surface-only: the entitlement itself is untouched.
		var revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT revoked_at FROM openrails.entitlements WHERE id=$1`, entID).Scan(&revokedAt))
		require.Nil(t, revokedAt, "CON reference finding does not mutate the entitlement")
		return nil
	}))

	// Idempotent (re-asserts the open finding, no duplicate, still surfaced).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings, "still dangling → still surfaced")
		require.Equal(t, 1, res.RequiresReview)

		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`,
			merchantID, "entitlement:"+entID.String()).Scan(&n))
		require.Equal(t, 1, n, "finding upserted, not duplicated")
		return nil
	}))
}

// #511 CON plane: two settled, non-refunded charges for the same customer/product
// in the same month are detected as consistency.duplicate.provider_charge — money
// collected twice with no explaining invoice/proration. EXCESS → ADMIN,
// surface-only (a refund is an operator decision; the finding carries the payment
// ids). Folded from the retired audit D-2 check. Idempotent.
func TestConverge_ConDuplicateProviderCharge(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID := uuid.New(), uuid.New()
	pay1, pay2 := uuid.New(), uuid.New()
	var customer uuid.UUID
	when := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second) // both in the same month

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "dup-prod-"+suffix, "dup-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'usd',720,true,$3)`, priceID, productID, merchantID)
		ins := func(id uuid.UUID, txn string) {
			exec(`INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, merchant_id, customer_id)
			      VALUES ($1,$2,'nmi',$3,9990000,9990000,'usd','completed',$4,$5,$6)`, id, priceID, txn, when, merchantID, customer)
		}
		ins(pay1, "dup-txn-1-"+suffix)
		ins(pay2, "dup-txn-2-"+suffix) // second charge, same product/month -> duplicate
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='consistency.duplicate.provider_charge' AND evidence->'local'->>'customer_id'=$2`, merchantID, customer.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=ANY($1)`, []uuid.UUID{pay1, pay2})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.RequiresReview, "duplicate money is surfaced for an operator, not auto-undone")
		require.Equal(t, 0, res.AutoFixed)

		var status string
		var ev map[string]any
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, evidence->'local' FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='consistency.duplicate.provider_charge' AND evidence->'local'->>'customer_id'=$2`,
			merchantID, customer.String()).Scan(&status, &ev))
		require.Equal(t, "requires_review", status)
		require.EqualValues(t, 2, ev["charge_count"], "two charges in the duplicate group")
		require.Len(t, ev["payment_ids"], 2)

		// surface-only: the payments are untouched.
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE id=ANY($1) AND refunded_payment_id IS NULL`, []uuid.UUID{pay1, pay2}).Scan(&n))
		require.Equal(t, 2, n, "no payment auto-refunded")
		return nil
	}))

	// Idempotent: still a duplicate -> still surfaced, one row.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		return nil
	}))
}
