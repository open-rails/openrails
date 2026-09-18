//go:build integration

package controlplane_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PlanProviderAccountCutover is the host-reachable, read-only #657 report: it
// resolves durable identities and classifies the move; it never writes and
// never executes a cross-account cutover.
func TestPlanProviderAccountCutoverIsReportOnly(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{
		Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI,
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
		exec(`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, archived) VALUES ($1, $2, $3, 'test', $4, $5)`,
			id, mid, rail, rail+"-"+id.String()[:8], archived)
		psps = append(psps, id)
		return id
	}
	customer := func() uuid.UUID { return dbtest.EnsureCustomerIDPgx(mctx, t, pool, uuid.NewString()) }
	card := func(customerID uuid.UUID, rail string, pspID uuid.UUID) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO openrails.payment_methods (id, merchant_id, customer_id, rail, rail_customer_ref, psp_id, custodian, initial_transaction_id, created_at, updated_at)
		      VALUES ($1, $2, $3, $4, $5, $6, 'psp', $7, $8, $8)`, id, mid, customerID, rail, "vault-"+id.String()[:8], pspID, "txn-"+id.String()[:8], now)
		methods = append(methods, id)
		return id
	}
	productID, priceID := uuid.New(), uuid.New()
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`, productID, "cutover-"+sfx, mid)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, mid)
	sub := func(customerID uuid.UUID, rail, status string, pspID uuid.UUID, method *uuid.UUID) uuid.UUID {
		id := uuid.New()
		var cancelledAt *time.Time
		var cancelType *string
		if status == "cancelled" {
			cancelledAt, cancelType = &now, strPtr("user")
		}
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at,
		        payment_method_id, customer_id, merchant_id, psp_id, cancelled_at, cancel_type)
		      VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $9, $10, $11, $12, $13, $14)`,
			id, priceID, productID, status, rail, "psid-"+id.String()[:8], now.Add(-time.Hour), now.Add(720*time.Hour), method, customerID, mid, pspID, cancelledAt, cancelType)
		subs = append(subs, id)
		return id
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM openrails.subscriptions WHERE id = ANY($1)`, subs)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.prices WHERE id = $1`, priceID)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.products WHERE id = $1`, productID)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.payment_methods WHERE id = ANY($1)`, methods)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.psps WHERE id = ANY($1)`, psps)
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
			SELECT (SELECT string_agg(id::text || ':' || psp_id::text || ':' || coalesce(payment_method_id::text, '-') || ':' || status::text, ',' ORDER BY id) FROM openrails.subscriptions WHERE id = ANY($1))
			    || '|' || (SELECT string_agg(id::text || ':' || psp_id::text, ',' ORDER BY id) FROM openrails.payment_methods WHERE id = ANY($2))
			    || '|' || (SELECT count(*)::text FROM openrails.rail_intents WHERE subscription_id = ANY($1))`, subs, methods).Scan(&out))
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
		{"same account, card ready", controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, ReplacementPaymentMethodID: &homeNew},
			want{controlplane.ProviderAccountCutoverSameAccount, controlplane.ProviderAccountCutoverReady}},
		{"same account, no replacement card", controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, TargetPSPID: &active},
			want{controlplane.ProviderAccountCutoverSameAccount, controlplane.ProviderAccountCutoverReplacementCardRequired}},
		{"same account, card on another PSP", controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, TargetPSPID: &active, ReplacementPaymentMethodID: &homeOnOther},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardPSP}},
		{"stripe subscription on its own account", controlplane.ProviderAccountCutoverQuery{SubscriptionID: stripeSub, TargetPSPID: &stripePSP},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverRailUnsupported}},
		{"ccbill subscription on its own account", controlplane.ProviderAccountCutoverQuery{SubscriptionID: ccbillSub, TargetPSPID: &ccbillPSP},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverRailUnsupported}},
		{"cancelled subscription", controlplane.ProviderAccountCutoverQuery{SubscriptionID: cancelled, ReplacementPaymentMethodID: &quitterNew},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverSubscriptionNotRebilling}},
		{"archived target", controlplane.ProviderAccountCutoverQuery{SubscriptionID: drain, TargetPSPID: &archived},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverTargetArchived}},
		{"another customer's card", controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, ReplacementPaymentMethodID: &stranger},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardNotOwned}},
		{"card that does not exist", controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, ReplacementPaymentMethodID: &missing},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardNotFound}},
		{"drain, no card yet", controlplane.ProviderAccountCutoverQuery{SubscriptionID: drain, TargetPSPID: &active},
			want{controlplane.ProviderAccountCutoverRequiresReentry, controlplane.ProviderAccountCutoverReplacementCardRequired}},
		{"drain, card re-entered on the target", controlplane.ProviderAccountCutoverQuery{SubscriptionID: drain, ReplacementPaymentMethodID: &reentered},
			want{controlplane.ProviderAccountCutoverRequiresReentry, controlplane.ProviderAccountCutoverCrossAccountNotQualified}},
		{"drain, card on a different PSP than the target", controlplane.ProviderAccountCutoverQuery{SubscriptionID: drain, TargetPSPID: &active, ReplacementPaymentMethodID: &drainerOnOther},
			want{controlplane.ProviderAccountCutoverBlocked, controlplane.ProviderAccountCutoverReplacementCardPSP}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, tc.q)
			require.NoError(t, err)
			require.Equal(t, tc.q.SubscriptionID, report.SubscriptionID)
			require.Equal(t, tc.want.disposition, report.Plan.Disposition, report.Plan.Reason)
			require.Equal(t, tc.want.code, report.Plan.Code, report.Plan.Reason)
			require.Equal(t, tc.want.code == controlplane.ProviderAccountCutoverReady, report.Plan.Executable)
			require.NotEmpty(t, report.Plan.Reason)
			if report.Plan.Disposition == controlplane.ProviderAccountCutoverBlocked {
				require.Empty(t, report.Plan.Steps)
			}
		})
	}
	report, err := cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: drain, ReplacementPaymentMethodID: &reentered})
	require.NoError(t, err)
	require.Equal(t, archived, report.SourcePSPID)
	require.Equal(t, active, report.TargetPSPID)
	require.Equal(t, controlplane.ErrProviderAccountCutoverNotQualified.Error(), report.Plan.Reason)

	// Identity errors: an unknown PSP or subscription, no target at all, and
	// another merchant's scope.
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, TargetPSPID: &missing})
	require.ErrorContains(t, err, "not found")
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: missing, TargetPSPID: &active})
	require.ErrorContains(t, err, "not found")
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: home})
	require.ErrorContains(t, err, "required")
	_, err = cp.PlanProviderAccountCutover(ctx, merchant.ID(uuid.New()), controlplane.ProviderAccountCutoverQuery{SubscriptionID: home, TargetPSPID: &active})
	require.Error(t, err, "another merchant's scope sees nothing")

	require.Equal(t, before, snapshot(), "a plan never writes: subscriptions, instruments and intents are untouched")
}

func strPtr(s string) *string { return &s }
