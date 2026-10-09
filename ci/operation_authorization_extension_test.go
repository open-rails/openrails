//go:build e2e && integration

package ci_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

// A running rental holds one tranche and grows it while it runs: grants up to
// capacity, refusals that write nothing, ordinal replay and conflicts, one
// winner for the last dollar, host-transaction rollback, and a settlement
// above the grown hold that posts the rest as owed.
func TestOperationAuthorizationExtension(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "holds-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)

	fund := func(amount int64) billing.CustomerID {
		customer := billing.CustomerID(uuid.New())
		_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
		require.NoError(t, err)
		_, err = client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "support", SourceID: uuid.NewString()})
		require.NoError(t, err)
		return customer
	}
	open := func(customer billing.CustomerID, operationID string, amount int64) {
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenOperationAuthorization(ctx, billing.OpenOperationAuthorizationParams{
			OperationID: operationID, CustomerID: customer, RecordOwner: "user:1", Currency: "USD", Amount: amount,
			ClaimReference: "claim:" + operationID, AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
		})
		require.NoError(t, err)
	}
	extend := func(operationID string, ordinal, amount, minimum int64) (*billing.OperationAuthorizationExtension, error) {
		return client.ExtendOperationAuthorization(ctx, billing.ExtendOperationAuthorizationParams{
			OperationID: operationID, Ordinal: ordinal, Amount: amount, MinimumAmount: minimum,
		})
	}
	refused := func(err error, status int, code string, param string) {
		t.Helper()
		var se *billing.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, status, se.Status, se.Error())
		require.Equal(t, code, se.Code)
		if param != "" {
			require.NotNil(t, se.Param)
			require.Equal(t, param, *se.Param)
		}
	}
	balance := func(customer billing.CustomerID) *billing.Balance {
		bal, err := client.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		return bal
	}

	customer := fund(10_000_000)
	open(customer, "rental-a", 2_000_000)

	full, err := extend("rental-a", 1, 3_000_000, 1_000_000)
	require.NoError(t, err)
	require.Equal(t, billing.OperationAuthorizationExtension{OperationID: "rental-a", Ordinal: 1, GrantedAmount: 3_000_000, AuthorizedAmount: 5_000_000}, *full)
	auth, err := client.GetOperationAuthorization(ctx, "rental-a")
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, auth.Amount)
	require.EqualValues(t, 5_000_000, auth.AuthorizedAmount)
	bal := balance(customer)
	require.EqualValues(t, 5_000_000, bal.HeldAmount)
	require.EqualValues(t, 5_000_000, bal.AvailableAmount)

	replay, err := extend("rental-a", 1, 3_000_000, 1_000_000)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.EqualValues(t, 3_000_000, replay.GrantedAmount)
	_, err = extend("rental-a", 1, 4_000_000, 1_000_000)
	refused(err, 409, "operation_authorization_conflict", "amount")
	_, err = extend("rental-a", 1, 3_000_000, 2_000_000)
	refused(err, 409, "operation_authorization_conflict", "minimum_amount")
	_, err = extend("rental-a", 3, 1_000_000, 1_000_000)
	refused(err, 409, "operation_authorization_conflict", "ordinal")
	_, err = extend("rental-a", 0, 1_000_000, 1_000_000)
	refused(err, 400, "invalid_param", "")
	_, err = extend("rental-a", 2, 1_000_000, 2_000_000)
	refused(err, 400, "invalid_param", "")
	_, err = extend("rental-missing", 1, 1_000_000, 1_000_000)
	refused(err, 404, "operation_authorization_not_found", "")

	// A second hold leaves 4 USD; a grant at exactly the minimum takes it all.
	open(customer, "rental-b", 1_000_000)
	partial, err := extend("rental-a", 2, 10_000_000, 4_000_000)
	require.NoError(t, err)
	require.EqualValues(t, 4_000_000, partial.GrantedAmount)
	require.EqualValues(t, 9_000_000, partial.AuthorizedAmount)
	require.EqualValues(t, 0, balance(customer).AvailableAmount)

	// Below the minimum: refused, nothing written, ordinal 3 still free.
	_, err = extend("rental-a", 3, 1_000_000, 1)
	refused(err, 402, "insufficient_credits", "")
	auth, err = client.GetOperationAuthorization(ctx, "rental-a")
	require.NoError(t, err)
	require.EqualValues(t, 9_000_000, auth.AuthorizedAmount)

	_, err = client.ReleaseOperationAuthorization(ctx, billing.ReleaseOperationAuthorizationParams{OperationID: "rental-b", ReleaseReference: "never-created:b"})
	require.NoError(t, err)
	_, err = extend("rental-b", 1, 1_000_000, 1)
	refused(err, 409, "operation_authorization_not_open", "")
	third, err := extend("rental-a", 3, 1_000_000, 1_000_000)
	require.NoError(t, err)
	require.EqualValues(t, 10_000_000, third.AuthorizedAmount)
	_, err = extend("rental-a", 4, math.MaxInt64, 1)
	refused(err, 400, "invalid_param", "")

	t.Run("two holds race for the last dollar", func(t *testing.T) {
		customer := fund(1_000_000)
		open(customer, "race-a", 100_000)
		open(customer, "race-b", 100_000)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, id := range []string{"race-a", "race-b"} {
			wg.Go(func() { _, err := extend(id, 1, 800_000, 800_000); errs <- err })
		}
		wg.Wait()
		close(errs)
		var granted, denied int
		for err := range errs {
			if err == nil {
				granted++
				continue
			}
			require.ErrorIs(t, err, billing.ErrInsufficientCredits)
			denied++
		}
		require.Equal(t, 1, granted)
		require.Equal(t, 1, denied)
		bal := balance(customer)
		require.EqualValues(t, 1_000_000, bal.HeldAmount)
		require.EqualValues(t, 0, bal.AvailableAmount)
	})

	t.Run("a rolled back host transaction leaves nothing", func(t *testing.T) {
		customer := fund(1_000_000)
		open(customer, "host-tx", 100_000)
		tx, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		grown, err := client.ExtendOperationAuthorizationTx(ctx, tx, billing.ExtendOperationAuthorizationParams{OperationID: "host-tx", Ordinal: 1, Amount: 500_000, MinimumAmount: 500_000})
		require.NoError(t, err)
		require.EqualValues(t, 600_000, grown.AuthorizedAmount)
		seen, err := client.GetOperationAuthorizationTx(ctx, tx, "host-tx")
		require.NoError(t, err)
		require.EqualValues(t, 600_000, seen.AuthorizedAmount)
		_, err = client.ExtendOperationAuthorizationTx(ctx, tx, billing.ExtendOperationAuthorizationParams{OperationID: "host-tx", Ordinal: 2, Amount: 900_000, MinimumAmount: 900_000})
		require.ErrorIs(t, err, billing.ErrInsufficientCredits)
		require.NoError(t, tx.Rollback(ctx))

		after, err := client.GetOperationAuthorization(ctx, "host-tx")
		require.NoError(t, err)
		require.EqualValues(t, 100_000, after.AuthorizedAmount)
		require.EqualValues(t, 100_000, balance(customer).HeldAmount)
		again, err := extend("host-tx", 1, 200_000, 200_000)
		require.NoError(t, err, "ordinal 1 was never committed")
		require.False(t, again.Replayed)
	})

	t.Run("settlement above the grown hold posts owed", func(t *testing.T) {
		customer := fund(3_000_000)
		open(customer, "settle", 1_000_000)
		_, err := extend("settle", 1, 2_000_000, 2_000_000)
		require.NoError(t, err)

		qual := settleProviderCost(t, ctx, database, client, "settle", 5_000_000)
		require.Equal(t, billing.OperationAuthorizationSettled, billing.OperationAuthorizationState(qual.Authorization.State))
		require.EqualValues(t, 3_000_000, qual.Authorization.AuthorizedAmount)
		var manifest struct {
			Authorization struct {
				OpeningAmount    string `json:"opening_amount"`
				AuthorizedAmount string `json:"authorized_amount"`
				Extensions       []struct {
					Ordinal       int64  `json:"ordinal"`
					GrantedAmount string `json:"granted_amount"`
				} `json:"extensions"`
			} `json:"authorization"`
		}
		require.NoError(t, json.Unmarshal(qual.Authorization.SettlementBody, &manifest))
		require.Equal(t, "1000000", manifest.Authorization.OpeningAmount)
		require.Equal(t, "3000000", manifest.Authorization.AuthorizedAmount)
		require.Len(t, manifest.Authorization.Extensions, 1)
		require.EqualValues(t, 1, manifest.Authorization.Extensions[0].Ordinal)
		require.Equal(t, "2000000", manifest.Authorization.Extensions[0].GrantedAmount)

		bal := balance(customer)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)
		require.EqualValues(t, 2_000_000, bal.OwedAmount)
		_, err = extend("settle", 2, 1, 1)
		refused(err, 409, "operation_authorization_not_open", "")
		replayed, err := extend("settle", 1, 2_000_000, 2_000_000)
		require.NoError(t, err)
		require.True(t, replayed.Replayed)
	})
}

// settleProviderCost drives two equal provider observations one quiescence
// apart, which settles the authorization at cost in the second commit.
func settleProviderCost(t *testing.T, ctx context.Context, database *db.DB, client *openrails.Client, operationID string, cost int64) *money.ProviderBillingQualification {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Microsecond))
	svc := money.NewMoneyService(database, clock)
	start, end := clock.Now().Add(-2*time.Hour), clock.Now().Add(-time.Hour)
	in := billing.RecordProviderBillingObservationParams{
		OperationID: operationID, ObservationID: operationID + ":1",
		Lifecycle: billing.ProviderBillingLifecycleEvidence{
			Provider: "provider", ProviderResourceID: "pod-" + operationID,
			ProviderLifetimeStartsAt: start, ProviderLifetimeEndsAt: end, ProviderAbsentAt: end,
			ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
			WindowsClosedAt: end, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
		},
		NormalizedQuery: "pod=" + operationID, QueryStartsAt: start, QueryEndsAt: end, RawBody: []byte(`[{"cost":1}]`),
		Records: []billing.ProviderBillingRecord{{ProviderResourceID: "pod-" + operationID, BucketStart: start, Amount: cost, TimeBilledMS: 3_600_000}},
	}
	record := func() *money.ProviderBillingQualification {
		tx, err := database.Pool().Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		txCtx, txDB, err := database.BindMerchantTx(ctx, tx, client.MerchantID())
		require.NoError(t, err)
		qual, err := svc.RecordProviderBillingObservationInTx(txCtx, txDB, in, time.Second)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		return qual
	}
	first := record()
	require.Equal(t, money.ProviderBillingQualificationPending, first.State)
	clock.Advance(2 * time.Second)
	in.ObservationID = operationID + ":2"
	qual := record()
	require.Equal(t, money.ProviderBillingQualificationEligible, qual.State)
	require.NotNil(t, qual.Authorization)
	return qual
}
