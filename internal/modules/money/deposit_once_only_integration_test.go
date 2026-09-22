//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
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

	q := dbtest.Queries(pool)
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
		SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM billing.grants
		 WHERE merchant_id = $1 AND customer_id = $2 AND source_id = $3
		   AND kind = 'credit' AND event = 'grant'
	`, merchantID, customer, sourceID).Scan(&rows, &total))
	require.Equal(t, 1, rows, "one key, one credit lot — enforced by the index alone")
	require.Equal(t, int64(5_000), total)
}

func TestDepositReplayPreservesFinancialTermsAndReceipt(t *testing.T) {
	svc, _, _, payer, currency, ctx := moneyInEnvWithDB(t)
	clock := clockwork.NewFakeClockAt(time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC))
	svc.SetClock(clock)
	key, description := uuid.NewString(), "original credit"
	expiry := clock.Now().Add(24 * time.Hour)
	in := money.DepositParams{CustomerID: &payer, Invoker: "original-invoker", Currency: currency,
		Amount: 1000000, Source: "original-source", SourceID: &key, ExpiresAt: &expiry, Description: &description}
	first, err := svc.Deposit(ctx, in)
	require.NoError(t, err)
	require.False(t, first.Replayed)

	for _, field := range []string{"amount", "currency", "expires_at", "permanent"} {
		t.Run(field, func(t *testing.T) {
			changed := in
			expectedField := field
			switch field {
			case "amount":
				changed.Amount++
			case "currency":
				changed.Currency = "EUR"
			case "expires_at":
				later := expiry.Add(time.Hour)
				changed.ExpiresAt = &later
			case "permanent":
				changed.ExpiresAt = nil
				expectedField = "expires_at"
			}
			_, err := svc.Deposit(ctx, changed)
			require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
			var conflict *money.IdempotencyConflict
			require.ErrorAs(t, err, &conflict)
			require.Equal(t, expectedField, conflict.Field)
		})
	}
	clock.Advance(time.Hour)
	retry := in
	retry.Currency = " usd "
	retry.Invoker, retry.Source = "replacement-invoker", "replacement-source"
	replacementDescription := "replacement credit"
	retry.Description = &replacementDescription
	replayed, err := svc.Deposit(ctx, retry)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	first.Replayed = true
	require.Equal(t, first, replayed, "diagnostic retry inputs cannot rewrite the original receipt")
	read, err := svc.GetDepositBySourceID(ctx, payer, key)
	require.NoError(t, err)
	require.Equal(t, first, read, "key read and POST replay return one persisted receipt")
	bal, err := svc.GetBalanceForCustomer(ctx, payer, currency)
	require.NoError(t, err)
	require.Equal(t, in.Amount, bal.Balance)
	eur, err := svc.GetBalanceForCustomer(ctx, payer, "EUR")
	require.NoError(t, err)
	require.Zero(t, eur.Balance)
}
