//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
)

// or#906: deposit once-only is a DATABASE fact. This bypasses EVERY Go-side
// guard — lockBalance, the GetCreditGrantBySourceID pre-check — and drives the
// generated InsertGrant twice at one deposit key. The second insert must be
// refused by uq_grants_credit_deposit_once, which is the only evidence that a
// future deposit path forgetting the lock still cannot double-credit. (LED-14
// cannot catch this: the deposit's ledger leg is keyed on the GRANT id, so two
// grant rows are two "distinct" ledger coordinates.)
func TestOr906_TheDatabaseRefusesADuplicateDepositGrant(t *testing.T) {
	_, _, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	merchantID := dbtest.TestMerchantID.UUID()
	customer := payer.UUID()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer.String())

	q := gen.New(pool)
	amount := int64(5_000)
	sourceID := "or906-raw-" + uuid.NewString()
	params := gen.InsertGrantParams{
		MerchantID: merchantID, CustomerID: customer,
		Kind: "credit", SourceType: "admin", SourceID: sourceID,
		Event: "grant", StartsAt: time.Now().UTC(),
		Amount: &amount, Currency: &cur,
	}

	_, err := q.InsertGrant(ctx, params)
	require.NoError(t, err, "the first grant at a deposit key lands")

	_, err = q.InsertGrant(ctx, params)
	require.ErrorContains(t, err, "uq_grants_credit_deposit_once",
		"the database alone must refuse the duplicate deposit grant")

	// A relabeled retry (different source_type, same source_id) is the SAME
	// deposit — doctrine (client.go): source is not part of the key.
	relabeled := params
	relabeled.SourceType = "purchase"
	_, err = q.InsertGrant(ctx, relabeled)
	require.ErrorContains(t, err, "uq_grants_credit_deposit_once",
		"a relabeled source must not mint a second credit at the same key")

	var rows int
	var total int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM openrails.grants
		 WHERE merchant_id = $1 AND customer_id = $2 AND source_id = $3
		   AND kind = 'credit' AND event = 'grant'
	`, merchantID, customer, sourceID).Scan(&rows, &total))
	require.Equal(t, 1, rows, "one key, one credit lot — enforced by the index alone")
	require.Equal(t, int64(5_000), total)
}

// A deposit replay carrying a different amount is refused with the typed
// conflict, and the identical replay still answers the original grant.
func TestOr906_MoneyDepositRefusesAChangedAmount(t *testing.T) {
	ms, _, _, payer, cur, ctx := moneyInEnvWithDB(t)
	sourceID := "or906-" + uuid.NewString()
	src := "admin"

	first, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 5_000, Source: src, SourceID: &sourceID,
	})
	require.NoError(t, err)
	require.False(t, first.Replayed)

	_, err = ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: src, SourceID: &sourceID,
	})
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused,
		"a changed-amount deposit retry must be refused, not answered with the original")

	replay, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 5_000, Source: src, SourceID: &sourceID,
	})
	require.NoError(t, err, "the identical retry is still an idempotent replay")
	require.True(t, replay.Replayed)
	require.Equal(t, first.ID, replay.ID)

	bal, err := ms.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(5_000), bal.Balance, "exactly one deposit moved money")
}
