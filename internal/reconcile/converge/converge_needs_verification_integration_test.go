//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #632 detector: an active, period-elapsed, provider-auto-billed subscription
// (CCBill, or vault-less NMI) with no confirming payment is flipped to `unknown`
// by the LIFE pass — NOT to past_due. An our-rebill NMI sub (with a vault) still
// takes the period_overdue -> past_due path. A current sub is untouched.
func TestConverge_NeedsVerification_FlipsAutoBilledToUnknown(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]

	pm := uuid.New()
	subCCBill, subNMIVaultless, subNMIVaulted, subCurrent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var cust uuid.UUID
	elapsed := time.Now().UTC().Add(-100 * 24 * time.Hour) // long past grace
	current := time.Now().UTC().Add(20 * 24 * time.Hour)
	start := time.Now().UTC().Add(-130 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.payment_methods (id,merchant_id,customer_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id) VALUES ($1,$2,$3,'nmi','cust-x','vault-x','tx-x')`, pm, merchantID, cust)
		// Distinct product per sub: uq_subscriptions_customer_product_lifecycle
		// forbids two active subs for one (customer, product).
		ins := func(id uuid.UUID, key, rail string, pmID *uuid.UUID, end time.Time) {
			prod, price := uuid.New(), uuid.New()
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, key+"-"+sfx, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,payment_method_id,started_at,current_period_starts_at,current_period_ends_at)
			      VALUES ($1,$2,$3,$4,$5,'active',$6,$7,$8,$8,$9)`, id, merchantID, cust, prod, price, rail, pmID, start, end)
		}
		ins(subCCBill, "nvcc", "ccbill", nil, elapsed)    // auto-billed -> unknown
		ins(subNMIVaultless, "nvnv", "nmi", nil, elapsed) // vault-less -> unknown
		ins(subNMIVaulted, "nvnp", "nmi", &pm, elapsed)   // our-rebill -> past_due
		ins(subCurrent, "nvcu", "ccbill", nil, current)   // current -> untouched
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE customer_id=$1`, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payment_methods WHERE id=$1`, pm)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE merchant_id=$1 AND product_id IN (SELECT id FROM openrails.products WHERE key LIKE 'nv%'||$2)`, merchantID, "-"+sfx)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE merchant_id=$1 AND key LIKE 'nv%'||$2`, merchantID, "-"+sfx)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key LIKE 'subscription:%'`, merchantID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		status := func(id uuid.UUID) string {
			var s string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, id).Scan(&s))
			return s
		}
		require.Equal(t, "unknown", status(subCCBill), "ccbill auto-billed lapsed -> unknown")
		require.Equal(t, "unknown", status(subNMIVaultless), "vault-less nmi lapsed -> unknown")
		require.Equal(t, "past_due", status(subNMIVaulted), "our-rebill nmi lapsed -> past_due (NOT unknown)")
		require.Equal(t, "active", status(subCurrent), "current sub untouched")
		return nil
	}))
}

// #632 detector exclusion: a confirming renewal payment (purchased at/after the
// period end) means the provider DID bill — the sub is NOT flagged for
// verification (it stays active; the renewal/advance path owns it).
func TestConverge_NeedsVerification_SkipsWhenRenewalPaymentPresent(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]
	prod, price, sub, pay := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var cust uuid.UUID
	periodEnd := time.Now().UTC().Add(-100 * 24 * time.Hour)
	start := time.Now().UTC().Add(-130 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "nvr-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,started_at,current_period_starts_at,current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','ccbill',$6,$6,$7)`, sub, merchantID, cust, prod, price, start, periodEnd)
		// A renewal charge landed AFTER the period end → provider billed.
		exec(`INSERT INTO openrails.payments (id,merchant_id,customer_id,price_id,subscription_id,rail,transaction_id,amount,list_amount,currency,status,purchased_at)
		      VALUES ($1,$2,$3,$4,$5,'ccbill',$6,5000000,5000000,'usd','completed',$7)`, pay, merchantID, cust, price, sub, "r-"+sfx, periodEnd.Add(time.Hour))
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=$1`, pay)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		var s string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, sub).Scan(&s))
		require.Equal(t, "active", s, "renewal payment present → not flipped to unknown")
		return nil
	}))
}

// #632 resolver: each provider-confirmed outcome moves an `unknown` sub to the
// right state; ResolveUnreachable leaves it unknown for backoff retry.
func TestResolveUnknownSubscription_Branches(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	sfx := uuid.NewString()[:8]
	prod, price := uuid.New(), uuid.New()
	start := time.Now().UTC().Add(-130 * 24 * time.Hour)
	periodEnd := time.Now().UTC().Add(-100 * 24 * time.Hour)

	// Distinct customer per unknown sub: resolving several into active/past_due for
	// one (customer, product) would trip uq_subscriptions_customer_product_lifecycle.
	mkUnknown := func(ctx context.Context) uuid.UUID {
		id := uuid.New()
		c := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		_, err := appDB.Qx(ctx).Exec(ctx, `INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,started_at,current_period_starts_at,current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'unknown','ccbill',$6,$6,$7)`, id, merchantID, c, prod, price, start, periodEnd)
		require.NoError(t, err)
		return id
	}
	get := func(ctx context.Context, id uuid.UUID) *models.Subscription {
		s, err := repo.NewSubscriptionRepo(appDB).GetByID(ctx, id)
		require.NoError(t, err)
		return s
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx, `INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "nvres-"+sfx, merchantID)
		require.NoError(t, err)
		_, err = appDB.Qx(ctx).Exec(ctx, `INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
		require.NoError(t, err)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE price_id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		// Unreachable → stays unknown.
		id := mkUnknown(ctx)
		require.NoError(t, lc.ResolveUnknownSubscription(ctx, appDB, get(ctx, id), subscriptions.ResolveUnreachable, nil, time.Now()))
		require.Equal(t, models.StatusUnknown, get(ctx, id).Status)

		// Renewed → active, period advanced to the provider's new end.
		id = mkUnknown(ctx)
		newEnd := time.Now().UTC().Add(30 * 24 * time.Hour)
		require.NoError(t, lc.ResolveUnknownSubscription(ctx, appDB, get(ctx, id), subscriptions.ResolveRenewed, &newEnd, time.Now()))
		got := get(ctx, id)
		require.Equal(t, models.StatusActive, got.Status)
		require.NotNil(t, got.CurrentPeriodEndsAt)
		require.WithinDuration(t, newEnd, *got.CurrentPeriodEndsAt, time.Second)

		// PastDue → past_due with a grace window.
		id = mkUnknown(ctx)
		require.NoError(t, lc.ResolveUnknownSubscription(ctx, appDB, get(ctx, id), subscriptions.ResolvePastDue, nil, periodEnd.Add(48*time.Hour)))
		require.Equal(t, models.StatusPastDue, get(ctx, id).Status)

		// Cancelled → cancelled.
		id = mkUnknown(ctx)
		require.NoError(t, lc.ResolveUnknownSubscription(ctx, appDB, get(ctx, id), subscriptions.ResolveCancelled, nil, time.Now()))
		require.Equal(t, models.StatusCancelled, get(ctx, id).Status)
		return nil
	}))
}
