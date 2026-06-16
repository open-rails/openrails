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
	"github.com/open-rails/openrails/internal/modules/holds"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
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

	// openrails_app login + a merchant directory row for this test merchant.
	for _, stmt := range []string{
		`ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw'`,
		`INSERT INTO openrails.merchants (id, slug, name) VALUES
		   ('` + tenantID + `','merchant-azh-` + tenantID[:8] + `','AZH')
		 ON CONFLICT (id) DO NOTHING`,
	} {
		_, e := super.Pool().Exec(ctx, stmt)
		require.NoError(t, e, stmt)
	}

	now := time.Now().UTC().Truncate(time.Second)
	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()

	t.Cleanup(func() {
		for _, q := range []struct {
			tbl   string
			where string
			arg   any
		}{
			{"openrails.money_transactions", "customer_id = $1", payerID},
			{"openrails.money_blocks", "customer_id = $1", payerID},
			{"openrails.merchants", "id = $1", tenantID},
		} {
			_, _ = super.Pool().Exec(ctx, "DELETE FROM "+q.tbl+" WHERE "+q.where, q.arg)
		}
	})

	// Seed a funded USD balance (1000 cents) as a spendable block (#491: balance
	// is derived from money_blocks). Inserted as super (bypasses RLS), test-scoped.
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.money_blocks
		   (id, merchant_id, customer_id, currency, original_amount, remaining_amount, created_at)
		 VALUES ($1,$2,$3,'USD',1000,1000,$4)`,
		uuid.New(), tenantID, payerID, now)
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
	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(ctx) })
	conn, err := rc.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rt.RedisClient = redis.NewClient(opt)
	t.Cleanup(func() { _ = rt.RedisClient.Close() })
	require.NoError(t, rt.RedisClient.Ping(ctx).Err())
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	tctx := merchant.WithID(ctx, mustTenantID(t, tenantID))
	exp := now.Add(time.Hour)

	// (1) Authorize WITHIN balance => allowed + hold placed + available reduced.
	res, err := svc.AuthorizeAndHold(tctx, billingservice.AuthorizeAndHoldRequest{
		CustomerID: payer, Invoker: "serviceToken:k1", EstimatedAmount: 600,
		InvokerType: string(identity.InvokerTypeDelegated), RequestID: "req-1", ExpiresAt: exp,
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)
	require.NotNil(t, res.HoldExpiresAt)
	require.Equal(t, int64(1000), res.AvailableAmount, "snapshot is pre-hold available")

	snap, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(0), snap.HeldAmount, "request holds are Redis state, not ledger held balance")
	require.Equal(t, int64(1000), snap.AvailableAmount)
	active, err := holds.NewStore(rt.RedisClient).ActiveAmount(tctx, tenantID, payerID.String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), active)
	var txCount int
	require.NoError(t, super.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.money_transactions WHERE merchant_id=$1 AND customer_id=$2`,
		tenantID, payerID,
	).Scan(&txCount))
	require.Equal(t, 0, txCount, "admit/authorize must not create ledger rows")

	// (2) Authorize OVER remaining balance => denied, NO new hold.
	deny, err := svc.AuthorizeAndHold(tctx, billingservice.AuthorizeAndHoldRequest{
		CustomerID: payer, Invoker: "serviceToken:k1", EstimatedAmount: 700,
		InvokerType: string(identity.InvokerTypeDelegated), RequestID: "req-2", ExpiresAt: exp,
	})
	require.NoError(t, err)
	require.False(t, deny.Allowed)
	require.Equal(t, billingservice.DenyInsufficientBalance, deny.DenyCode)
	require.Nil(t, deny.HoldExpiresAt)

	snap2, err := svc.GetCreditAccount(tctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(0), snap2.HeldAmount, "denied authorize must not change ledger held")
	active, err = holds.NewStore(rt.RedisClient).ActiveAmount(tctx, tenantID, payerID.String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(600), active, "denied authorize must not create a Redis hold")

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
				CustomerID: payer, Invoker: "serviceToken:k1", EstimatedAmount: 300,
				InvokerType: string(identity.InvokerTypeDelegated),
				RequestID:   "req-conc-" + string(rune('a'+i)), ExpiresAt: exp,
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
	require.Equal(t, int64(0), final.HeldAmount)
	require.Equal(t, int64(1000), final.AvailableAmount)
	active, err = holds.NewStore(rt.RedisClient).ActiveAmount(tctx, tenantID, payerID.String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(900), active)

	require.NoError(t, svc.ReleaseHold(tctx, "req-1"))
	active, err = holds.NewStore(rt.RedisClient).ActiveAmount(tctx, tenantID, payerID.String(), money.DefaultCurrency)
	require.NoError(t, err)
	require.Equal(t, int64(300), active)
	require.NoError(t, super.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.money_transactions WHERE merchant_id=$1 AND customer_id=$2`,
		tenantID, payerID,
	).Scan(&txCount))
	require.Equal(t, 0, txCount, "admit/release without completion must not create ledger rows")
}

func mustTenantID(t *testing.T, s string) merchant.ID {
	t.Helper()
	id, err := merchant.ParseID(s)
	require.NoError(t, err)
	return id
}
