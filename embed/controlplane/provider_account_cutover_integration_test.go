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
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(mctx, sql, args...)
		require.NoError(t, err)
	}

	archived, active := uuid.New(), uuid.New()
	exec(`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, archived) VALUES ($1, $2, 'nmi', 'test', $3, true), ($4, $2, 'nmi', 'test', $5, false)`,
		archived, mid, "arch-"+sfx, active, "act-"+sfx)
	customerID := dbtest.EnsureCustomerIDPgx(mctx, t, pool, uuid.NewString())
	strangerID := dbtest.EnsureCustomerIDPgx(mctx, t, pool, uuid.NewString())
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	oldPM, reentered, sameAccount, strangers := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`, productID, "cutover-"+sfx, mid)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, mid)
	exec(`INSERT INTO openrails.payment_methods (id, merchant_id, customer_id, rail, rail_customer_ref, psp_id, custodian, initial_transaction_id, created_at, updated_at)
	      VALUES ($1, $2, $3, 'nmi', $4, $5, 'psp', 'txn-' || $4, $9, $9), ($6, $2, $3, 'nmi', $7, $8, 'psp', 'txn-' || $7, $9, $9), ($10, $2, $3, 'nmi', $11, $5, 'psp', 'txn-' || $11, $9, $9), ($12, $2, $13, 'nmi', $14, $8, 'psp', 'txn-' || $14, $9, $9)`,
		oldPM, mid, customerID, "vault-old-"+sfx, archived, reentered, "vault-new-"+sfx, active, now, sameAccount, "vault-same-"+sfx, strangers, strangerID, "vault-stranger-"+sfx)
	exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at, payment_method_id, customer_id, merchant_id, psp_id)
	      VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $5, $7, $8, $9, $10)`,
		subID, priceID, productID, "psid-"+sfx, now.Add(-time.Hour), now.Add(720*time.Hour), oldPM, customerID, mid, archived)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM openrails.subscriptions WHERE id = $1`, subID)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.prices WHERE id = $1`, priceID)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.products WHERE id = $1`, productID)
		_, _ = pool.Exec(bg, `DELETE FROM openrails.payment_methods WHERE id = ANY($1)`, []uuid.UUID{oldPM, reentered, sameAccount, strangers})
		_, _ = pool.Exec(bg, `DELETE FROM openrails.psps WHERE id = ANY($1)`, []uuid.UUID{archived, active})
	})
	snapshot := func() (string, string) {
		var subs, pms string
		require.NoError(t, pool.QueryRow(mctx, `SELECT psp_id::text || ':' || payment_method_id::text FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subs))
		require.NoError(t, pool.QueryRow(mctx, `SELECT string_agg(id::text || ':' || psp_id::text, ',' ORDER BY id) FROM openrails.payment_methods WHERE id = ANY($1)`, []uuid.UUID{oldPM, reentered, sameAccount}).Scan(&pms))
		return subs, pms
	}
	subsBefore, pmsBefore := snapshot()

	// Card re-entered on the active account: report-only, never executable.
	report, err := cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, ReplacementPaymentMethodID: &reentered})
	require.NoError(t, err)
	require.Equal(t, archived, report.SourcePSPID)
	require.Equal(t, active, report.TargetPSPID)
	require.Equal(t, controlplane.ProviderAccountCutoverRequiresReentry, report.Plan.Disposition)
	require.False(t, report.Plan.Executable)
	require.Equal(t, controlplane.ErrProviderAccountCutoverNotQualified.Error(), report.Plan.Reason)
	require.NotEmpty(t, report.Plan.Steps)

	// No card yet: the plan directs re-entry on the named target.
	report, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, TargetPSPID: &active})
	require.NoError(t, err)
	require.Equal(t, controlplane.ProviderAccountCutoverRequiresReentry, report.Plan.Disposition)
	require.False(t, report.Plan.Executable)
	require.NotEqual(t, controlplane.ErrProviderAccountCutoverNotQualified.Error(), report.Plan.Reason)

	// Same account: the durable payment-source update path.
	report, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, ReplacementPaymentMethodID: &sameAccount})
	require.NoError(t, err)
	require.Equal(t, controlplane.ProviderAccountCutoverSameAccount, report.Plan.Disposition)
	require.True(t, report.Plan.Executable)

	// Target archived: blocked.
	report, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, TargetPSPID: &archived})
	require.NoError(t, err)
	require.Equal(t, controlplane.ProviderAccountCutoverSameAccount, report.Plan.Disposition, "same id is the same account, archived or not")

	// Identity errors: another customer's card, a foreign or unknown PSP, an
	// unknown subscription, an ambiguous query.
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, ReplacementPaymentMethodID: &strangers})
	require.ErrorContains(t, err, "another customer")
	unknown := uuid.New()
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, TargetPSPID: &unknown})
	require.ErrorContains(t, err, "not found")
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: unknown, TargetPSPID: &active})
	require.ErrorContains(t, err, "not found")
	_, err = cp.PlanProviderAccountCutover(ctx, dbtest.TestMerchantID, controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID})
	require.ErrorContains(t, err, "exactly one")
	_, err = cp.PlanProviderAccountCutover(ctx, merchant.ID(uuid.New()), controlplane.ProviderAccountCutoverQuery{SubscriptionID: subID, TargetPSPID: &active})
	require.Error(t, err, "another merchant's scope sees nothing")

	subsAfter, pmsAfter := snapshot()
	require.Equal(t, subsBefore, subsAfter, "a plan never repoints the subscription")
	require.Equal(t, pmsBefore, pmsAfter, "a plan never re-attributes an instrument")
}
