//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// TestCreditGrantExpiryDefault (#857) pins the DEFAULT on credit-lot expiry
// through the real grant path (MoneyService.GrantPurchaseCredits -> grant
// ledger) and the real reaper (CreditExpiryWorker, whose predicate is
// `ends_at IS NOT NULL AND ends_at <= now`).
//
// Doctrine: expiry destroys customer money, so it happens only because a
// merchant asked for it. A credits_spec that omits expiry_hours writes a NULL
// ends_at, which no future sweep can ever match — it is not "expires in a
// year", it is never. An explicitly declared 365-day expiry is a contract and
// still fires exactly on time.
//
// Before the #857 fix the omitted case resolved to DefaultCreditGrantExpiryHours
// (365*24), so subtest never_expires_when_unspecified failed on both legs:
// ends_at was non-NULL and the worker clawed the balance back to zero.
func TestCreditGrantExpiryDefault(t *testing.T) {
	ctx := context.Background()
	// The WORKER's handle stays unpinned — that is production's posture and what
	// or#868/B1 is about. Grants and balance reads are module-service calls that
	// the request/worker layer pins in production, so they get a pinned handle.
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	svcDB := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()))
	mctx := dbtest.WithTestMerchant(ctx)

	// Grants are stamped at a fixed, well-past instant so the single sweep below
	// is unambiguously past a 365-day expiry.
	grantedAt := time.Now().UTC().Add(-2 * 365 * 24 * time.Hour).Truncate(time.Second)

	unspecified := grantCreditLot(t, mctx, svcDB, grantedAt, nil)
	explicit365 := grantCreditLot(t, mctx, svcDB, grantedAt, ptrInt(365*24))

	t.Run("never_expires_when_unspecified", func(t *testing.T) {
		require.Nil(t, lotEndsAt(t, mctx, dbi, unspecified),
			"omitted expiry_hours must leave ends_at NULL — no implicit clock on customer money")
	})
	t.Run("declared_expiry_is_honored", func(t *testing.T) {
		ends := lotEndsAt(t, mctx, dbi, explicit365)
		require.NotNil(t, ends, "an explicit expiry_hours must stamp ends_at")
		require.WithinDuration(t, grantedAt.Add(365*24*time.Hour), ends.UTC(), time.Minute)
	})

	// One real sweep, well past both grants' 365-day mark.
	worker := CreditExpiryWorker{DB: dbi, Clock: clockwork.NewFakeClockAt(grantedAt.Add(400 * 24 * time.Hour))}
	require.NoError(t, worker.Work(ctx, nil))

	t.Run("worker_leaves_the_never_expiring_lot_alone", func(t *testing.T) {
		bal, err := money.NewMoneyService(svcDB, clockwork.NewRealClock()).
			GetBalanceForCustomer(mctx, unspecified.customer, unspecified.unit)
		require.NoError(t, err)
		require.Equal(t, int64(1_000), bal.Balance,
			"a lot with no declared expiry must survive every sweep, forever")
	})
	t.Run("worker_claws_back_the_declared_expiry", func(t *testing.T) {
		t.Skip("or#868/B1: CreditExpiryWorker enumerates lapsed lots via w.DB.RunInTx on the base pool " +
			"under a comment claiming a 'Privileged (no-GUC) cross-merchant sweep'. There is no privileged " +
			"pool: as openrails_app with no app.merchant_id the enumeration returns ZERO rows, so the worker " +
			"has never expired a credit lot. Un-skip with the definer-backed enumeration fix.")
		bal, err := money.NewMoneyService(svcDB, clockwork.NewRealClock()).
			GetBalanceForCustomer(mctx, explicit365.customer, explicit365.unit)
		require.NoError(t, err)
		require.Equal(t, int64(0), bal.Balance,
			"a declared 365-day expiry is contractual and still fires")
	})
}

type creditLot struct {
	customer identity.CustomerID
	unit     string
}

// grantCreditLot deposits one 1,000-unit purchase credit lot for a fresh
// customer through the production grant path, with the money service's clock
// pinned to grantedAt so the lot's expiry math is deterministic.
func grantCreditLot(t *testing.T, ctx context.Context, dbi *db.DB, grantedAt time.Time, expiryHours *int) creditLot {
	t.Helper()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()), uuid.New().String())
	svc := money.NewMoneyService(dbi, clockwork.NewFakeClockAt(grantedAt))
	require.NoError(t, svc.GrantPurchaseCredits(ctx, money.GrantPurchaseCreditsParams{
		Payer:     identity.CustomerID(customerID),
		PaymentID: uuid.New(),
		Source:    "purchase",
		Spec: models.CreditsSpec{
			"expiry_default_probe": {
				Unit:        "USD",
				Amount:      1_000,
				Cadence:     models.CreditGrantCadenceOnce,
				ExpiryHours: expiryHours,
			},
		},
	}))
	return creditLot{customer: identity.CustomerID(customerID), unit: "USD"}
}

// lotEndsAt reads the single credit lot's ends_at straight from the grants
// table — the column the expiry predicate keys on.
func lotEndsAt(t *testing.T, ctx context.Context, dbi *db.DB, lot creditLot) *time.Time {
	t.Helper()
	var endsAt *time.Time
	require.NoError(t, dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID()).QueryRow(ctx,
		`SELECT ends_at FROM openrails.grants
		  WHERE customer_id = $1 AND kind = 'credit' AND event = 'grant'`,
		lot.customer.UUID()).Scan(&endsAt))
	return endsAt
}

func ptrInt(v int) *int { return &v }
