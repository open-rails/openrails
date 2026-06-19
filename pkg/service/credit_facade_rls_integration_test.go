//go:build integration

package service_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

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
// db.BeginMerchantTx) work under the unprivileged openrails_app role with RLS
// ENFORCING (issue #227). Before the #227 GUC plumbing these used a raw
// GetDB().BeginTx that did not set app.merchant_id, so under openrails_app they
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

	// Seed merchant as super (super bypasses RLS). Money needs no credit-type row (#472).
	super, err := db.NewDB(&config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		tenantID, "merchant-cf-"+tenantID[:8])
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

	// Deposit 1000 — exercises BeginMerchantTx under RLS.
	depSrc := uuid.New()
	_, err = svc.DepositCredits(tctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 1000, Source: "test", SourceID: &depSrc,
	})
	require.NoError(t, err, "DepositCredits must work under openrails_app (GUC set)")

	acct, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(1000), acct.BalanceAmount)
	require.Equal(t, money.DefaultCurrency, acct.Currency)

	eurSrc := uuid.New()
	_, err = svc.DepositCredits(tctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: "EUR", Amount: 2500, Source: "test", SourceID: &eurSrc,
	})
	require.NoError(t, err, "DepositCredits must keep non-USD funds in their requested currency")
	eurAcct, err := svc.GetCreditAccount(tctx, payer, "EUR")
	require.NoError(t, err)
	require.Equal(t, "EUR", eurAcct.Currency)
	require.Equal(t, int64(2500), eurAcct.BalanceAmount, "EUR account must not read the USD balance")

	acct, err = svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(1000), acct.BalanceAmount, "USD account must stay separate from EUR")

	// Withdraw 400 — exercises a money debit under openrails_app (RLS GUC). Request
	// holds moved to Redis (#513), so this direct debit is the merchant-scoped write
	// path that proves writes work under the openrails_app role.
	wSrc := uuid.New()
	_, err = svc.WithdrawCredits(tctx, billingservice.WithdrawCreditsRequest{
		CustomerID: &payer, Invoker: payer.UUID().String(), Amount: 400, Source: "test", SourceID: &wSrc,
	})
	require.NoError(t, err, "WithdrawCredits must work under openrails_app")

	final, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), final.BalanceAmount, "1000 - 400 withdrawn = 600")

	// #242 billing-account admin surface under openrails_app: configure arrears
	// mode + an outstanding cap, read it back, and list the org's usage.
	arrears := "arrears"
	var cap int64 = 5000
	require.NoError(t, svc.SetCreditAccountSettings(tctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: &arrears, MaxOutstandingOwedAmount: &cap,
	}), "SetCreditAccountSettings must work under openrails_app")

	settings, err := svc.GetCreditAccountSettings(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, "arrears", settings.BillingMode, "settings read back the configured mode")

	txns, total, err := svc.GetCustomerCreditTransactions(tctx, payer, money.DefaultCurrency, 50, 0)
	require.NoError(t, err)
	require.Greater(t, total, 0, "org has credit transactions (deposit/withdraw)")
	require.NotEmpty(t, txns)
}

// mustTenantID parses a merchant id for tests (re-homed from the removed
// authorize_and_hold test).
func mustTenantID(t *testing.T, s string) merchant.ID {
	t.Helper()
	id, err := merchant.ParseID(s)
	require.NoError(t, err)
	return id
}
