//go:build integration

package service_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
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
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TestAuthorizeAndHold_Atomic proves the #235/#247 atomic authorize+hold under
// REAL RLS: it runs the real migration chain (incl. 050: RLS + openrails_app),
// connects the billing service as the unprivileged openrails_app role, seeds a
// funded payer (as super, bypassing RLS), and asserts:
//
//   - authorize WITHIN balance => allowed, a hold is placed, available is reduced;
//   - authorize OVER balance => denied, NO hold placed;
//   - two CONCURRENT authorizes that together exceed the balance => exactly one
//     passes (the FOR UPDATE row lock serializes them — atomicity).
//
// Env hatch: set OPENRAILS_TEST_DB_DSN to a SUPER/admin DSN (mirrors
// internal/db/repo/rls_realtable_integration_test.go). Skipped when unset.
func TestAuthorizeAndHold_Atomic(t *testing.T) {
	superDSN := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	if superDSN == "" {
		t.Skip("set OPENRAILS_TEST_DB_DSN to a super/admin Postgres DSN to run this test")
	}
	ctx := context.Background()

	// Real migrations (idempotent; 050 = RLS + FORCE + openrails_app).
	require.NoError(t, migrate.RunPostgres(ctx, &config.Config{DB: &config.DBConfig{URL: superDSN}}))

	tenantID := uuid.NewString()
	super, err := db.NewDB(&config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()

	// openrails_app login + a tenant directory row for this test tenant.
	for _, stmt := range []string{
		`ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw'`,
		`INSERT INTO openrails.tenants (id, slug, name) VALUES
		   ('` + tenantID + `','tenant-azh-` + tenantID[:8] + `','AZH')
		 ON CONFLICT (id) DO NOTHING`,
	} {
		_, e := super.Pool().Exec(ctx, stmt)
		require.NoError(t, e, stmt)
	}

	now := time.Now().UTC().Truncate(time.Second)
	payer := identity.TenantSubjectIDFromString(uuid.NewString())
	payerID := payer.UUID()

	t.Cleanup(func() {
		for _, q := range []struct {
			tbl   string
			where string
			arg   any
		}{
			{"openrails.money_transactions", "tenant_subject_id = $1", payerID},
			{"openrails.money_balances", "tenant_subject_id = $1", payerID},
			{"openrails.tenants", "id = $1", tenantID},
		} {
			_, _ = super.Pool().Exec(ctx, "DELETE FROM "+q.tbl+" WHERE "+q.where, q.arg)
		}
	})

	// Seed a funded USD balance (1000 cents) as super (bypasses RLS), scoped to the
	// test tenant. Money is unit-less — one balance per currency (#472).
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.money_balances
		   (id, tenant_id, tenant_subject_id, currency, balance, held_balance, created_at, updated_at)
		 VALUES ($1,$2,$3,'USD',1000,0,$4,$5)`,
		uuid.New(), tenantID, payerID, now, now)
	require.NoError(t, err)

	// Billing service as the unprivileged openrails_app role (RLS ENFORCES).
	u, _ := url.Parse(superDSN)
	u.User = url.UserPassword("openrails_app", "app_pw")
	appDB, err := db.NewDB(&config.DBConfig{URL: u.String()})
	require.NoError(t, err)
	defer appDB.Close()
	posture, err := appDB.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, posture.Enforcing, "service must run as an RLS-enforcing role")

	rt := &app.Runtime{
		DB:                 appDB,
		MoneyService:       money.NewMoneyService(appDB),
		EntitlementService: entitlements.NewEntitlementService(appDB),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	tctx := tenant.WithID(ctx, mustTenantID(t, tenantID))
	exp := now.Add(time.Hour)

	// (1) Authorize WITHIN balance => allowed + hold placed + available reduced.
	res, err := svc.AuthorizeAndHold(tctx, billingservice.AuthorizeAndHoldRequest{
		TenantSubjectID: payer, Actor: "serviceToken:k1", EstimateMicros: 600,
		RequestID: "req-1", ExpiresAt: exp,
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)
	require.NotNil(t, res.ReservationID)
	require.Equal(t, int64(1000), res.AvailableMicros, "snapshot is pre-hold available")

	snap, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), snap.HeldMicros)
	require.Equal(t, int64(400), snap.AvailableMicros)

	// (2) Authorize OVER remaining balance => denied, NO new hold.
	deny, err := svc.AuthorizeAndHold(tctx, billingservice.AuthorizeAndHoldRequest{
		TenantSubjectID: payer, Actor: "serviceToken:k1", EstimateMicros: 700,
		RequestID: "req-2", ExpiresAt: exp,
	})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, billingservice.DenyInsufficientBalance, deny.DenyCode)
	require.Nil(t, deny.ReservationID)

	snap2, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), snap2.HeldMicros, "denied authorize must not change held")

	// (3) Concurrent double-authorize on the remaining 400: two requests for 300
	// each. Only ONE may pass (300+300 > 400). The FOR UPDATE lock serializes them.
	var wg sync.WaitGroup
	results := make([]*billingservice.AuthorizeAndHoldResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.AuthorizeAndHold(tctx, billingservice.AuthorizeAndHoldRequest{
				TenantSubjectID: payer, Actor: "serviceToken:k1", EstimateMicros: 300,
				RequestID: "req-conc-" + string(rune('a'+i)), ExpiresAt: exp,
			})
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	allowed := 0
	for _, r := range results {
		if r.Allowed {
			allowed++
		}
	}
	require.Equal(t, 1, allowed, "exactly one concurrent authorize may pass on the same balance (atomicity)")

	// Final held = 600 + 300 (the one winner) = 900; available = 100.
	final, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(900), final.HeldMicros)
	require.Equal(t, int64(100), final.AvailableMicros)
}

func mustTenantID(t *testing.T, s string) tenant.ID {
	t.Helper()
	id, err := tenant.ParseID(s)
	require.NoError(t, err)
	return id
}
