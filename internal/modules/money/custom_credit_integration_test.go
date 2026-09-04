//go:build integration

package money_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestCustomCreditConsumeAndInvariant exercises the #475 happy path (define a
// custom credit unit, deposit + spend it, assert balance) plus the invariant
// (a billing op on the qualified unit is rejected) and presentation.
func TestCustomCreditConsumeAndInvariant(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)

	// (1) define gold @ decimals=2, owned by the test merchant ("test" slug).
	// Registry writes live in the catalog sidecar push (#706); seed the row the
	// same way it does.
	_, err := pool.Exec(ctx, `
INSERT INTO openrails.custom_credit_types (id, merchant_id, name, decimals, active)
VALUES (uuidv7(), $1, 'gold', 2, true)
ON CONFLICT (merchant_id, name) DO UPDATE SET decimals = 2, active = true`, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	unit, err := svc.CustomUnitCode(ctx, "gold")
	require.NoError(t, err)

	// ResolveUnit returns the custom decimals, not a built-in.
	dec, builtin, err := svc.ResolveUnit(ctx, unit)
	require.NoError(t, err)
	require.False(t, builtin)
	require.Equal(t, 2, dec)

	// (2) deposit 500 gold, spend 150 -> balance 350.
	_, err = svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(),
		Currency: unit, Amount: 500, Source: "grant",
	})
	require.NoError(t, err)
	withdrawKey := uuid.New() // or#891: the idempotency key is required, not optional
	_, err = svc.Withdraw(ctx, money.WithdrawParams{
		CustomerID: &payer, Invoker: payer.UUID().String(),
		Currency: unit, Amount: 150, Source: "usage", SourceID: &withdrawKey,
	})
	require.NoError(t, err)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, unit)
	require.NoError(t, err)
	require.Equal(t, int64(350), bal.Balance)

	// (3) invariant: a billing op (owed accrual) on the qualified unit is rejected.
	_, err = svc.AccrueOwed(ctx, payer, unit, "billing", uuid.NewString(), 100)
	require.Error(t, err)
	require.True(t, errors.Is(err, money.ErrBillingUnitRequired), "got %v", err)
}
