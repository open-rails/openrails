//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
)

// A prepaid customer runs into debt down to the host's floor: overdraft_amount
// widens one call's capacity, a hold may exceed the balance, settlement spends
// the balance and posts the rest as owed, the floor counts that debt, and the
// next funding repays it. A customer never funded gets a balance account when
// an overdraft covers its first hold.
func TestOperationAuthorizationOverdraft(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "overdraft-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)
	const floor = 2_000_000

	customer := func() billing.CustomerID {
		c := billing.CustomerID(uuid.New())
		_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: c}})
		require.NoError(t, err)
		return c
	}
	fund := func(c billing.CustomerID, amount int64) {
		_, err := client.CreateCreditGrant(ctx, c, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "support", SourceID: uuid.NewString()})
		require.NoError(t, err)
	}
	open := func(c billing.CustomerID, operationID string, amount, overdraft int64) error {
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenOperationAuthorization(ctx, billing.OpenOperationAuthorizationParams{
			OperationID: operationID, CustomerID: c, RecordOwner: "user:1", Currency: "USD", Amount: amount,
			ClaimReference: "claim:" + operationID, AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
			OverdraftAmount: overdraft,
		})
		return err
	}
	extend := func(operationID string, ordinal, amount, minimum, overdraft int64) (*billing.OperationAuthorizationExtension, error) {
		return client.ExtendOperationAuthorization(ctx, billing.ExtendOperationAuthorizationParams{
			OperationID: operationID, Ordinal: ordinal, Amount: amount, MinimumAmount: minimum, OverdraftAmount: overdraft,
		})
	}
	balance := func(c billing.CustomerID) billing.Balance {
		bal, err := client.GetBalance(ctx, c, "USD")
		require.NoError(t, err)
		return *bal
	}
	invalid := func(err error) {
		t.Helper()
		var se *billing.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, 400, se.Status, se.Error())
	}

	payer := customer()
	fund(payer, 1_000_000)
	require.ErrorIs(t, open(payer, "od-a", 2_500_000, 0), billing.ErrInsufficientCredits, "no overdraft, no debt")
	invalid(open(payer, "od-a", 2_500_000, -1))
	require.NoError(t, open(payer, "od-a", 2_500_000, floor))
	bal := balance(payer)
	require.EqualValues(t, 1_000_000, bal.BalanceAmount)
	require.EqualValues(t, 2_500_000, bal.HeldAmount)
	require.EqualValues(t, -1_500_000, bal.AvailableAmount)

	// 0.5 USD is left above the floor: a minimum past it is refused and writes
	// nothing; a smaller minimum takes exactly what is left.
	_, err = extend("od-a", 1, 1_000_000, 600_000, floor)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits)
	_, err = extend("od-a", 1, 1_000_000, 400_000, 0)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits, "without the overdraft the customer is already past zero")
	invalid(func() error { _, err := extend("od-a", 1, 1_000_000, 400_000, -1); return err }())
	grown, err := extend("od-a", 1, 1_000_000, 400_000, floor)
	require.NoError(t, err)
	require.EqualValues(t, 500_000, grown.GrantedAmount)
	require.EqualValues(t, 3_000_000, grown.AuthorizedAmount)
	require.EqualValues(t, -floor, balance(payer).AvailableAmount)
	_, err = extend("od-a", 2, 1, 1, floor)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits, "at the floor")
	replayed, err := extend("od-a", 1, 1_000_000, 400_000, 0)
	require.NoError(t, err, "the overdraft is not part of a grant's identity")
	require.True(t, replayed.Replayed)

	// Settled under the hold: the balance is spent and the rest is owed.
	settleProviderCost(t, ctx, database, client, "od-a", 2_800_000)
	bal = balance(payer)
	require.EqualValues(t, 0, bal.BalanceAmount)
	require.EqualValues(t, 0, bal.HeldAmount)
	require.EqualValues(t, 1_800_000, bal.OwedAmount)

	// The floor counts the debt: 0.2 USD of room is left.
	require.ErrorIs(t, open(payer, "od-b", 300_000, floor), billing.ErrInsufficientCredits)
	require.NoError(t, open(payer, "od-b", 200_000, floor))
	_, err = client.ReleaseOperationAuthorization(ctx, billing.ReleaseOperationAuthorizationParams{OperationID: "od-b", ReleaseReference: "never-created:b"})
	require.NoError(t, err)

	// The next funding repays the debt first.
	fund(payer, 5_000_000)
	bal = balance(payer)
	require.EqualValues(t, 3_200_000, bal.BalanceAmount)
	require.EqualValues(t, 0, bal.OwedAmount)
	require.EqualValues(t, 3_200_000, bal.AvailableAmount)

	t.Run("a customer never funded holds within the overdraft", func(t *testing.T) {
		fresh := customer()
		require.ErrorIs(t, open(fresh, "fresh-a", 1_500_000, 0), billing.ErrInsufficientCredits, "typed, with no balance account")
		require.ErrorIs(t, open(fresh, "fresh-a", 2_500_000, floor), billing.ErrInsufficientCredits)
		require.NoError(t, open(fresh, "fresh-a", 1_500_000, floor))
		require.EqualValues(t, -1_500_000, balance(fresh).AvailableAmount)
		settleProviderCost(t, ctx, database, client, "fresh-a", 1_200_000)
		bal := balance(fresh)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 1_200_000, bal.OwedAmount)
	})

	t.Run("an arrears account keeps its credit line", func(t *testing.T) {
		arrears := customer()
		_, err := client.UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{CustomerID: arrears, CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 500_000}}}})
		require.NoError(t, err)
		require.ErrorIs(t, open(arrears, "arrears-a", 1_000_000, floor), billing.ErrInsufficientCredits)
		require.NoError(t, open(arrears, "arrears-a", 500_000, floor))
	})
}
