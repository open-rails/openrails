//go:build integration

package converge

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511 Phase D (DERIVE plane): a live grant whose derived effect is missing is
// detected as `derive.grant_effect.missing` and auto-repaired by running
// derive-2 (grants.MaterializeGrant). Idempotent: a second pass finds nothing.
func TestConverge_DeriveGrantEffectMissing(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	cur := "TC" + strings.ToUpper(uuid.NewString()[:6])
	e := NewConvergeEngine(appDB)

	var customer, grantID uuid.UUID

	// Seed a credit grant WITHOUT materializing it → its #512 deposit is missing.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		amt := int64(5000)
		g, err := grants.New(appDB.Gen(ctx), merchantID).Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Credit, Source: grants.Purchase,
			SourceID: "pay_" + uuid.NewString()[:8], Amount: &amt, Currency: &cur,
		})
		require.NoError(t, err)
		grantID = g.ID
		dep, err := appDB.Gen(ctx).GrantCreditDeposited(ctx, gen.GrantCreditDepositedParams{MerchantID: merchantID, GrantID: g.ID})
		require.NoError(t, err)
		require.False(t, dep, "precondition: effect not yet materialized")
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "grant_effect:"+grantID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			return nil
		})
	})

	// Converge → detect + auto-repair (run derive-2).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)

		dep, err := appDB.Gen(ctx).GrantCreditDeposited(ctx, gen.GrantCreditDepositedParams{MerchantID: merchantID, GrantID: grantID})
		require.NoError(t, err)
		require.True(t, dep, "repair materialized the credit deposit")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant_effect.missing' AND subject_key=$2`,
			merchantID, "grant_effect:"+grantID.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)
		return nil
	}))

	// Idempotent: the effect now exists → no DERIVE finding on a second pass.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "already materialized → converged, no finding")
		return nil
	}))
}

// #511 Phase D (DERIVE plane): a TERMINATED entitlement grant whose entitlement
// window was never retracted (a bare revoke event, no derive-2) is detected as
// `derive.grant_effect.excess` and auto-retracted (RevokeEntitlementsByGrant).
// AUTO + not gated: the grant + its revoke are both present (a recorded decision,
// not a confirmed-absence case). Idempotent.
func TestConverge_DeriveGrantEffectExcess_TerminatedNotRetracted(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	feature := "feat_" + uuid.NewString()[:8]

	var customer, grantID uuid.UUID

	// Seed a materialized entitlement grant, then append a BARE revoke event (no
	// MaterializeGrant) → terminated grant, entitlement window still live.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		gl := grants.New(appDB.Gen(ctx), merchantID)
		// Grace-sourced so the materialized entitlement (which now preserves its
		// source via grant_id, #511) isn't also flagged by the CON orphan-source
		// checks — this test is about grant_effect.excess retraction, not CON.
		g, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Entitlement, Source: grants.Grace,
			SourceID: uuid.NewString(), Spec: &grants.Spec{Entitlements: []string{feature}},
		})
		require.NoError(t, err)
		require.NoError(t, gl.MaterializeGrant(ctx, g)) // entitlement window exists
		_, err = gl.Revoke(ctx, g.ID, "admin removed (retraction not yet run)")
		require.NoError(t, err)
		grantID = g.ID
		var live bool
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM openrails.entitlements
				WHERE merchant_id=$1 AND grant_id=$2
				  AND revoked_at IS NULL AND deleted_at IS NULL
			)
		`, merchantID, g.ID).Scan(&live))
		require.True(t, live, "precondition: entitlement still live despite the revoke")
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key = ANY($2)`,
				merchantID, []string{"grant_effect:" + grantID.String(), "customer:" + customer.String()})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notification_queue WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND event<>'grant'`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			return nil
		})
	})

	// Converge → detect excess → retract the entitlement window.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		// 2 findings: the excess retraction + the #789 post-repair NOTIFY
		// access-ended row for the window this run just revoked.
		require.Equal(t, 2, res.Findings)
		require.Equal(t, 2, res.AutoFixed, "terminated grant retraction is AUTO (recorded revoke, not absence)")

		var live bool
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM openrails.entitlements
				WHERE merchant_id=$1 AND grant_id=$2
				  AND revoked_at IS NULL AND deleted_at IS NULL
			)
		`, merchantID, grantID).Scan(&live))
		require.False(t, live, "entitlement window retracted by the repair")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant_effect.excess' AND subject_key=$2`,
			merchantID, "grant_effect:"+grantID.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)
		return nil
	}))

	// Idempotent: retracted → no finding on a second pass.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "retracted → converged, no finding")
		return nil
	}))
}

// derive.grant.excess (grant tier): a LIVE grant whose backing payment was
// REFUNDED is surfaced for review — surface-only, never
// auto-retracted (a goodwill refund may intentionally keep access). Idempotent.
func TestConverge_DeriveGrantExcess_RefundedPayment(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, paymentID := uuid.New(), uuid.New(), uuid.New()
	var customer, grantID uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`, productID, "ge-prod-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id)
		      VALUES ($1,$2,9990000,'USD',$3)`, priceID, productID, merchantID)
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "mobius")
		// A REFUNDED payment backing a still-live ownership grant.
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'mobius',$5,9990000,9990000,'USD','refunded',now(),$6)`,
			paymentID, merchantID, customer, priceID, "txn_"+suffix, pspID)
		gl := grants.New(appDB.Gen(ctx), merchantID)
		g, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Ownership, Product: &productID,
			Source: grants.Purchase, SourceID: paymentID.String(), Payment: &paymentID,
		})
		require.NoError(t, err)
		grantID = g.ID
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "grant:"+grantID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=$1`, paymentID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.GreaterOrEqual(t, res.Findings, 1)

		var status, sev string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, severity FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant.excess' AND subject_key=$2`,
			merchantID, "grant:"+grantID.String()).Scan(&status, &sev))
		require.Equal(t, "requires_review", status, "refunded-payment grant surfaced for operator triage")

		// Surface-only: the grant is NOT auto-terminated.
		terminated, err := appDB.Gen(ctx).IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: merchantID, GrantID: grantID})
		require.NoError(t, err)
		require.False(t, terminated, "derive.grant.excess never auto-revokes the grant")
		return nil
	}))

	// Idempotent: re-running surfaces the same finding, still no auto-revoke.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		return nil
	}))
}

// derive.grant.missing (grant tier, spec-aware): a completed one-off payment for
// a product whose spec PROMISES entitlements but produced no grant is surfaced
// ADMIN. A payment for an empty-spec product, and a payment that DID produce a
// grant, are both ignored — the product's own grant spec is the positive signal.
func TestConverge_DeriveGrantMissing_GrantablePayment(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]
	// grantable product (promises "premium"); empty-spec product (a pure fee)
	prodGrant, prodEmpty := uuid.New(), uuid.New()
	priceGrant, priceEmpty := uuid.New(), uuid.New()
	payMissing, payEmpty, payOK := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`, prodGrant, "gm-g-"+sfx, merchantID)
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prodEmpty, "gm-e-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,5000000,'USD',$3)`, priceGrant, prodGrant, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,5000000,'USD',$3)`, priceEmpty, prodEmpty, merchantID)
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "mobius")
		ins := func(id, price uuid.UUID, txn string) {
			exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
			      VALUES ($1,$2,$3,$4,'mobius',$5,5000000,5000000,'USD','completed',now(),$6)`, id, merchantID, customer, price, txn, pspID)
		}
		ins(payMissing, priceGrant, "txn-m-"+sfx) // grantable product, NO grant → flagged
		ins(payEmpty, priceEmpty, "txn-e-"+sfx)   // empty-spec product → ignored
		ins(payOK, priceGrant, "txn-o-"+sfx)      // grantable product, HAS a grant → ignored
		gl := grants.New(appDB.Gen(ctx), merchantID)
		_, err := gl.Grant(ctx, grants.GrantInput{Customer: customer, Kind: grants.Entitlement, Source: grants.Purchase, SourceID: payOK.String(), Spec: &grants.Spec{Entitlements: []string{"premium"}}})
		require.NoError(t, err)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key LIKE 'payment:%'`, merchantID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=ANY($1)`, []uuid.UUID{payMissing, payEmpty, payOK})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=ANY($1)`, []uuid.UUID{priceGrant, priceEmpty})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=ANY($1)`, []uuid.UUID{prodGrant, prodEmpty})
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		count := func(pid uuid.UUID) int {
			var n int
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT count(*) FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant.missing' AND subject_key=$2`,
				merchantID, "payment:"+pid.String()).Scan(&n))
			return n
		}
		require.Equal(t, 1, count(payMissing), "grantable product, no grant → flagged")
		require.Equal(t, 0, count(payEmpty), "empty-spec product → not flagged")
		require.Equal(t, 0, count(payOK), "payment with a grant → not flagged")
		var severity, status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT severity, status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant.missing' AND subject_key=$2`,
			merchantID, "payment:"+payMissing.String()).Scan(&severity, &status))
		require.Equal(t, "critical", severity, "#690: orphaned category (paying, no access) outranks freeloader")
		require.Equal(t, "requires_review", status, "ADMIN surface-only")
		return nil
	}))
}

// #575: the merchant-wide sweep (Scope.Customer == nil) runs DERIVE as ONE
// set query per check across ALL grant-holders — not one query per customer.
// Seed the same drift (an unmaterialized credit grant) on TWO customers, run a
// single merchant-scoped Converge, and assert it detects + auto-repairs BOTH —
// proving the customer=nil enumeration matches the per-customer path across the
// whole merchant. Guards the new set-based branch.
func TestConverge_DeriveSweepRemediatesAllCustomers(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	cur := "TC" + strings.ToUpper(uuid.NewString()[:6])
	e := NewConvergeEngine(appDB)

	var customers, grantIDs []uuid.UUID

	deposited := func(ctx context.Context, gid uuid.UUID) bool {
		dep, err := appDB.Gen(ctx).GrantCreditDeposited(ctx, gen.GrantCreditDepositedParams{MerchantID: merchantID, GrantID: gid})
		require.NoError(t, err)
		return dep
	}

	// Seed two distinct customers, each with an un-materialized credit grant.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		for i := 0; i < 2; i++ {
			c := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			amt := int64(5000)
			g, err := grants.New(appDB.Gen(ctx), merchantID).Grant(ctx, grants.GrantInput{
				Customer: c, Kind: grants.Credit, Source: grants.Purchase,
				SourceID: "pay_" + uuid.NewString()[:8], Amount: &amt, Currency: &cur,
			})
			require.NoError(t, err)
			require.False(t, deposited(ctx, g.ID), "precondition: customer %d effect not yet materialized", i)
			customers = append(customers, c)
			grantIDs = append(grantIDs, g.ID)
		}
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for i := range customers {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "grant_effect:"+grantIDs[i].String())
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.ledger_transfers WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customers[i])
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customers[i])
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.ledger_accounts WHERE merchant_id=$1 AND currency=$2`, merchantID, cur)
			return nil
		})
	})

	// Merchant-scoped sweep (Customer nil): one DERIVE pass over the whole merchant.
	// Assertions are pollution-robust (the shared merchant may carry other tests'
	// rows): prove MY two grants went unmaterialized → materialized via the sweep,
	// and that the sweep auto-fixed at least them.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID})
		require.NoError(t, err)
		require.GreaterOrEqual(t, res.AutoFixed, 2, "sweep auto-repairs both customers' missing effects")
		for i := range grantIDs {
			require.True(t, deposited(ctx, grantIDs[i]), "sweep materialized customer %d's credit deposit", i)
		}
		return nil
	}))

	// Idempotent: a second sweep leaves both materialized (no re-fire / no clawback).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID})
		require.NoError(t, err)
		for i := range grantIDs {
			require.True(t, deposited(ctx, grantIDs[i]), "customer %d stays materialized after a second sweep", i)
		}
		return nil
	}))
}
