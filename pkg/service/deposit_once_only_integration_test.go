//go:build integration

package service_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#906 at the SDK boundary (the seam a host retries against): a deposit
// replay carrying a corrected amount is refused with ErrIdempotencyKeyReused —
// never answered as a "success" carrying the ORIGINAL amount, which is how a
// corrected admin grant silently kept the wrong number.
func TestOr906_DepositRefusesAChangedAmount(t *testing.T) {
	svc, ms, _, payer, ctx := idemEnv(t, 0)
	pid := payer.UUID()
	key, err := billingservice.NewDepositIdempotencyKey("admin", "or906-"+uuid.NewString())
	require.NoError(t, err)

	first, err := svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
		Amount: 5_000, Key: key,
	})
	require.NoError(t, err)
	require.False(t, first.Replayed)

	_, err = svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
		Amount: 10_000, Key: key,
	})
	require.ErrorIs(t, err, billingservice.ErrIdempotencyKeyReused)

	replay, err := svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
		Amount: 5_000, Key: key,
	})
	require.NoError(t, err, "the identical retry is still an idempotent replay")
	require.True(t, replay.Replayed)
	require.Equal(t, first.ID, replay.ID)
	require.Equal(t, int64(5_000), idemBalance(t, ms, ctx, payer))
}

// GetDeposit answers "what did this key do" — the committed grant with
// Replayed=true, matching what a replay POST would answer — and (nil, nil)
// for a key that never committed.
func TestOr906_GetDepositAnswersTheKey(t *testing.T) {
	svc, _, _, payer, ctx := idemEnv(t, 0)
	pid := payer.UUID()
	sourceID := "or906-lookup-" + uuid.NewString()
	key, err := billingservice.NewDepositIdempotencyKey("admin", sourceID)
	require.NoError(t, err)
	desc := "or906 lookup fixture"

	first, err := svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &payer, Invoker: pid.String(), Currency: money.DefaultCurrency,
		Amount: 7_500, Key: key, Description: &desc,
	})
	require.NoError(t, err)

	got, err := svc.GetDeposit(ctx, payer, sourceID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, first.ID, got.ID, "the lookup answers the grant the key committed")
	require.Equal(t, int64(7_500), got.Amount)
	require.True(t, got.Replayed, "the movement landed EARLIER — LED-15 vocabulary")
	require.False(t, got.CreatedAt.IsZero())
	require.NotNil(t, got.Description)
	require.Equal(t, desc, *got.Description, "the description is durable on the grant (or#906)")

	missing, err := svc.GetDeposit(ctx, payer, "or906-never-"+uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, missing, "a key that never committed is a nil answer, not an error")
}
