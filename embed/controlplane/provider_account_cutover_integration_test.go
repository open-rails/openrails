//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PlanProviderAccountCutover is the host-reachable, read-only #657 report: it
// resolves durable identities and classifies the move; it never writes and
// never executes a cross-account cutover.
func TestPlanProviderAccountCutoverIsReportOnly(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{
		TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI,
		SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn},
		Auth: &config.AuthConfig{Issuer: "https://cutover.openrails.test", KeysPath: t.TempDir()},
	}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{})
	require.NoError(t, err)

	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	mctx := merchant.WithID(ctx, dbtest.TestMerchantID)
	mid := dbtest.TestMerchantID.UUID()
	sfx := uuid.NewString()[:8]
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(mctx, sql, args...)
		require.NoError(t, err)
	}
	var psps, methods, subs []uuid.UUID
	psp := func(rail string, archived bool) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO billing.psps (id, merchant_id, rail, environment, account_id, archived) VALUES ($1, $2, $3, 'test', $4, $5)`,
			id, mid, rail, rail+"-"+id.String()[:8], archived)
		psps = append(psps, id)
		return id
	}
	customer := func() uuid.UUID { return dbtest.EnsureCustomerIDPgx(mctx, t, pool, uuid.NewString()) }
	card := func(customerID uuid.UUID, rail string, pspID uuid.UUID) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO billing.payment_methods (id, merchant_id, customer_id, rail, rail_customer_ref, psp_id, custodian, initial_transaction_id, created_at, updated_at)
		      VALUES ($1, $2, $3, $4, $5, $6, 'psp', $7, $8, $8)`, id, mid, customerID, rail, "vault-"+id.String()[:8], pspID, "txn-"+id.String()[:8], now)
		methods = append(methods, id)
		return id
	}
	productID, priceID := uuid.New(), uuid.New()
	exec(`INSERT INTO billing.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`, productID, "cutover-"+sfx, mid)
	exec(`INSERT INTO billing.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, mid)
	sub := func(customerID uuid.UUID, rail, status string, pspID uuid.UUID, method *uuid.UUID) uuid.UUID {
		id := uuid.New()
		var cancelledAt *time.Time
		var cancelType *string
		if status == "cancelled" {
			cancelledAt, cancelType = &now, strPtr("user")
		}
		exec(`INSERT INTO billing.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at,
		        payment_method_id, customer_id, merchant_id, psp_id, cancelled_at, cancel_type)
		      VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $9, $10, $11, $12, $13, $14)`,
			id, priceID, productID, status, rail, "psid-"+id.String()[:8], now.Add(-time.Hour), now.Add(720*time.Hour), method, customerID, mid, pspID, cancelledAt, cancelType)
		subs = append(subs, id)
		return id
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM billing.subscriptions WHERE id = ANY($1)`, subs)
		_, _ = pool.Exec(bg, `DELETE FROM billing.prices WHERE id = $1`, priceID)
		_, _ = pool.Exec(bg, `DELETE FROM billing.products WHERE id = $1`, productID)
		_, _ = pool.Exec(bg, `DELETE FROM billing.payment_methods WHERE id = ANY($1)`, methods)
		_, _ = pool.Exec(bg, `DELETE FROM billing.psps WHERE id = ANY($1)`, psps)
	})

	archived, active, other := psp("nmi", true), psp("nmi", false), psp("nmi", false)
	stripePSP, ccbillPSP := psp("stripe", false), psp("ccbill", false)

	// A subscriber draining off the archived account.
	drainer := customer()
	drainOld := card(drainer, "nmi", archived)
	reentered := card(drainer, "nmi", active)
	drainerOnOther := card(drainer, "nmi", other)
	drain := sub(drainer, "nmi", "active", archived, &drainOld)
	// A subscriber on an active account swapping cards there.
	homer := customer()
	homeOld := card(homer, "nmi", active)
	homeNew := card(homer, "nmi", active)
	homeOnOther := card(homer, "nmi", other)
	home := sub(homer, "nmi", "active", active, &homeOld)
	// Rails and lifecycles the durable update does not serve.
	stripeSub := sub(customer(), "stripe", "active", stripePSP, nil)
	ccbillSub := sub(customer(), "ccbill", "active", ccbillPSP, nil)
	quitter := customer()
	quitterNew := card(quitter, "nmi", active)
	cancelled := sub(quitter, "nmi", "cancelled", active, nil)
	stranger := card(customer(), "nmi", active)
	missing := uuid.New()

	snapshot := func() string {
		var out string
		require.NoError(t, pool.QueryRow(mctx, `
			SELECT (SELECT string_agg(id::text || ':' || psp_id::text || ':' || coalesce(payment_method_id::text, '-') || ':' || status::text, ',' ORDER BY id) FROM billing.subscriptions WHERE id = ANY($1))
			    || '|' || (SELECT string_agg(id::text || ':' || psp_id::text, ',' ORDER BY id) FROM billing.payment_methods WHERE id = ANY($2))
			    || '|' || (SELECT count(*)::text FROM billing.rail_intents WHERE subscription_id = ANY($1))`, subs, methods).Scan(&out))
		return out
	}
	before := snapshot()

	type want struct {
		disposition controlplane.ProviderAccountCutoverDisposition
		code        controlplane.ProviderAccountCutoverCode
	}
	cases := []struct {
		name string
		q    controlplane.ProviderAccountCutoverQuery
		want want
	}{
		{"same account, card ready", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&homeNew)},
			want{controlplane.ProviderAccountCutoverSameAccount, controlplane.ProviderAccountCutoverReady}},
		{"same account, no replacement card", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &active},
			want{controlplane.ProviderAccountCutoverSameAccount, controlplane.ProviderAccountCutoverReplacementCardRequired}},
		{"same account, card on another PSP", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &active, ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&homeOnOther)},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardPSP}},
		{"stripe subscription on its own account", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(stripeSub), TargetPSPID: &stripePSP},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverRailUnsupported}},
		{"ccbill subscription on its own account", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(ccbillSub), TargetPSPID: &ccbillPSP},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverRailUnsupported}},
		{"cancelled subscription", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(cancelled), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&quitterNew)},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverSubscriptionNotRebilling}},
		{"archived target", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(drain), TargetPSPID: &archived},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverTargetArchived}},
		{"another customer's card", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&stranger)},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardNotOwned}},
		{"card that does not exist", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&missing)},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardNotFound}},
		{"drain, no card yet", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(drain), TargetPSPID: &active},
			want{controlplane.ProviderAccountCutoverRequiresReentry, controlplane.ProviderAccountCutoverReplacementCardRequired}},
		{"drain, card re-entered on the target", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(drain), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&reentered)},
			want{controlplane.ProviderAccountCutoverRequiresReentry, controlplane.ProviderAccountCutoverCrossAccountNotQualified}},
		{"drain, card on a different PSP than the target", controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(drain), TargetPSPID: &active, ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&drainerOnOther)},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardPSP}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, tc.q)
			require.NoError(t, err)
			require.Equal(t, tc.q.SubscriptionID, report.SubscriptionID)
			wire, err := json.Marshal(report)
			require.NoError(t, err)
			require.Contains(t, string(wire), `"subscription_id":"`+tc.q.SubscriptionID.String()+`"`)
			require.Equal(t, tc.want.disposition, report.Plan.Disposition, report.Plan.Reason)
			require.Equal(t, tc.want.code, report.Plan.Code, report.Plan.Reason)
			require.Equal(t, tc.want.code == controlplane.ProviderAccountCutoverReady, report.Plan.Executable)
			require.NotEmpty(t, report.Plan.Reason)
			if report.Plan.Disposition == controlplane.ProviderAccountCutoverBlocked {
				require.Empty(t, report.Plan.Steps)
			}
		})
	}
	report, err := cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(drain), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&reentered)})
	require.NoError(t, err)
	require.Equal(t, archived, report.SourcePSPID)
	require.Equal(t, active, report.TargetPSPID)
	require.Equal(t, controlplane.ErrProviderAccountCutoverNotQualified.Error(), report.Plan.Reason)

	// Explicit zero identifiers are refused with the coded invalid-parameter
	// envelope BEFORE any lookup: never silently "not supplied" (which would
	// re-target the subscription's own account) and never a "not found" probe.
	zero := uuid.Nil
	for _, tc := range []struct {
		name  string
		id    merchant.ID
		q     controlplane.ProviderAccountCutoverQuery
		param string
	}{
		{"zero merchant", merchant.ID{}, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &active}, "merchant_id"},
		{"zero subscription", dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{TargetPSPID: &active}, "subscription_id"},
		{"zero target psp", dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &zero}, "target_psp_id"},
		{"zero replacement method", dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), ReplacementPaymentMethodID: (*openrails.PaymentMethodID)(&zero)}, "replacement_payment_method_id"},
		{"neither target nor card", dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home)}, "target_psp_id"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, err := cp.PlanProviderAccountCutover(ctx, tc.id, tc.q)
			require.ErrorIs(t, err, openrails.ErrInvalid)
			var se *apperr.Error
			require.ErrorAs(t, err, &se)
			require.Equal(t, 400, se.Status)
			require.Equal(t, "invalid_param", se.Code)
			require.NotEmpty(t, se.Param)
			require.Equal(t, tc.param, se.Param)
			require.NotContains(t, se.Message, "not found", "a zero id is invalid, never a missing row")
		})
	}

	// Identity errors: an unknown PSP or subscription, no target at all, and
	// another merchant's scope.
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &missing})
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(missing), TargetPSPID: &active})
	require.ErrorIs(t, err, openrails.ErrNotFound)

	foreignMerchant, foreignPSP := uuid.New(), uuid.New()
	super := dbtest.SharedSuperuserPGXPool(t)
	_, err = super.Exec(ctx, `INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, foreignMerchant, "foreign-cutover-"+foreignMerchant.String())
	require.NoError(t, err)
	_, err = super.Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id) VALUES($1,$2,'nmi','test','hidden-foreign-provider')`, foreignPSP, foreignMerchant)
	require.NoError(t, err)
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &foreignPSP})
	require.ErrorIs(t, err, openrails.ErrNotFound, "a foreign PSP is indistinguishable from a missing one")
	require.NotContains(t, err.Error(), "hidden-foreign-provider")
	_, err = cp.PlanProviderAccountCutover(ctx, merchant.ID(foreignMerchant), controlplane.ProviderAccountCutoverQuery{SubscriptionID: openrails.SubscriptionID(home), TargetPSPID: &foreignPSP})
	require.ErrorIs(t, err, openrails.ErrNotFound, "another merchant's scope sees no subscription")

	require.Equal(t, before, snapshot(), "a plan never writes: subscriptions, instruments and intents are untouched")
}

func strPtr(s string) *string { return &s }
