//go:build integration

package service_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// TestCreditFacade_RLS_Under_OpenRailsApp proves the RAW money facade paths
// (DepositCredits/HoldCredits/CaptureHold — all of which open a transaction via
// db.BeginTenantTx) work under the unprivileged openrails_app role with RLS
// ENFORCING (issue #227). Before the #227 GUC plumbing these used a raw
// GetDB().BeginTx that did not set app.tenant_id, so under openrails_app they
// were fail-closed (no rows). This test would fail-closed without the fix.
func TestCreditFacade_RLS_Under_OpenRailsApp(t *testing.T) {
	superDSN := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	if superDSN == "" {
		t.Skip("set OPENRAILS_TEST_DB_DSN to a super/admin Postgres DSN")
	}
	ctx := context.Background()
	require.NoError(t, migrate.RunPostgres(ctx, &config.Config{DB: &config.DBConfig{URL: superDSN}}))

	tenantID := uuid.NewString()
	payer := identity.CustomerIDFromString(uuid.NewString())
	now := time.Now().UTC().Truncate(time.Second)

	// Seed tenant as super (super bypasses RLS). Money needs no credit-type row (#472).
	super, err := db.NewDB(&config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.tenants (id, slug, name) VALUES ($1, $2, 'CF') ON CONFLICT (id) DO NOTHING`,
		tenantID, "tenant-cf-"+tenantID[:8])
	require.NoError(t, err)
	_, err = super.Pool().Exec(ctx,
		`ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw'`)
	require.NoError(t, err)

	// Facade connected as the unprivileged openrails_app role (RLS ENFORCES).
	u, _ := url.Parse(superDSN)
	u.User = url.UserPassword("openrails_app", "app_pw")
	appDB, err := db.NewDB(&config.DBConfig{URL: u.String()})
	require.NoError(t, err)
	defer appDB.Close()
	posture, err := appDB.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, posture.Enforcing)

	rt := &app.Runtime{
		DB:                 appDB,
		MoneyService:       money.NewMoneyService(appDB),
		EntitlementService: entitlements.NewEntitlementService(appDB),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	tctx := merchant.WithID(ctx, mustTenantID(t, tenantID))

	// Deposit 1000 — exercises BeginTenantTx under RLS.
	depSrc := uuid.New()
	_, err = svc.DepositCredits(tctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Actor: payer.UUID().String(), Amount: 1000, Source: "test", SourceID: &depSrc,
	})
	require.NoError(t, err, "DepositCredits must work under openrails_app (GUC set)")

	acct, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(1000), acct.BalanceMicros)

	// Hold 600.
	hold, err := svc.HoldCredits(tctx, billingservice.HoldCreditsRequest{
		CustomerID: &payer, Actor: payer.UUID().String(), Amount: 600,
		Source: "test", SourceID: uuid.NewString(), ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err, "HoldCredits must work under openrails_app")
	require.NotNil(t, hold)

	held, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), held.HeldMicros)
	require.Equal(t, int64(400), held.AvailableMicros)

	// Capture 400 of the 600 hold.
	_, err = svc.CaptureHold(tctx, billingservice.CaptureHoldRequest{HoldID: hold.ID, Amount: 400})
	require.NoError(t, err, "CaptureHold must work under openrails_app")

	final, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), final.BalanceMicros, "1000 - 400 captured = 600")
	require.Equal(t, int64(0), final.HeldMicros, "hold fully resolved")

	// #242 billing-account admin surface under openrails_app: configure arrears
	// mode + an outstanding cap, read it back, and list the org's usage.
	arrears := "arrears"
	var cap int64 = 5000
	require.NoError(t, svc.SetCreditAccountSettings(tctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: &arrears, MaxOutstandingOwedMicros: &cap,
	}), "SetCreditAccountSettings must work under openrails_app")

	settings, err := svc.GetCreditAccountSettings(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, "arrears", settings.BillingMode, "settings read back the configured mode")

	txns, total, err := svc.GetCustomerCreditTransactions(tctx, payer, money.DefaultCurrency, 50, 0)
	require.NoError(t, err)
	require.Greater(t, total, 0, "org has credit transactions (deposit/hold/capture)")
	require.NotEmpty(t, txns)
}
