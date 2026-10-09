//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
)

// A provider that bills a resource reports it, at zero if it charged nothing. A read with no
// record over the whole provider-confirmed lifetime (RunPod never reports a CPU pod) refuses
// the qualification and keeps the hold for an operator; it never settles at zero. A read
// that does not cover the lifetime yet stays pending, and records of zero still settle at
// zero.
func TestProviderBillingEmptyReadNeverSettlesAtZero(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "empty-read-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)

	customer := billing.CustomerID(uuid.New())
	_, err = client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	_, err = createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: uuid.NewString()})
	require.NoError(t, err)
	open := func(operationID string) {
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenOperationAuthorization(ctx, billing.OpenOperationAuthorizationParams{
			OperationID: operationID, CustomerID: customer, RecordOwner: "user:1", Currency: "USD", Amount: 200_000,
			ClaimReference: "claim:" + operationID, AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
		})
		require.NoError(t, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	start, end := now.Add(-2*time.Hour), now.Add(-time.Hour)
	empty := func(operationID, observationID string, queryStart time.Time) billing.RecordProviderBillingObservationParams {
		return billing.RecordProviderBillingObservationParams{
			OperationID: operationID, ObservationID: observationID,
			Lifecycle: billing.ProviderBillingLifecycleEvidence{
				Provider: "runpod", ProviderResourceID: "pod-" + operationID,
				ProviderLifetimeStartsAt: start, ProviderLifetimeEndsAt: end, ProviderAbsentAt: end,
				ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
				WindowsClosedAt: end, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
			},
			NormalizedQuery: "pod=" + operationID, QueryStartsAt: queryStart, QueryEndsAt: end, RawBody: []byte(`[]`),
		}
	}

	open("cpu-pod")
	partial, err := client.RecordProviderBillingObservation(ctx, empty("cpu-pod", "cpu-pod:partial", start.Add(time.Minute)))
	require.NoError(t, err)
	require.Equal(t, billing.ProviderBillingQualificationPending, partial.State)
	require.Equal(t, billing.ProviderBillingCoverageIncomplete, partial.Reason, "a read short of the lifetime is not yet evidence")

	refused, err := client.RecordProviderBillingObservation(ctx, empty("cpu-pod", "cpu-pod:whole", start))
	require.NoError(t, err)
	require.Equal(t, billing.ProviderBillingQualificationRefused, refused.State)
	require.Equal(t, billing.ProviderBillingProviderEvidenceRefused, refused.Reason)
	require.Equal(t, billing.OperationAuthorizationOpen, refused.Authorization.State)
	require.Nil(t, refused.Authorization.SettlementAmount)
	bal, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 200_000, bal.HeldAmount, "the hold stays reserved")
	require.EqualValues(t, 1_000_000, bal.BalanceAmount)

	stuck, err := client.ListProviderBillingQualifications(ctx, billing.ProviderBillingQualificationListParams{
		State:              []billing.ProviderBillingQualificationState{billing.ProviderBillingQualificationRefused},
		AuthorizationState: []billing.OperationAuthorizationState{billing.OperationAuthorizationOpen},
	})
	require.NoError(t, err)
	require.Len(t, stuck.Items, 1)
	invoiced := int64(46_000)
	closed, err := client.ResolveProviderBillingQualification(ctx, billing.ResolveProviderBillingQualificationParams{
		OperationID: "cpu-pod", Kind: billing.ProviderBillingResolutionSettled, CostAmount: &invoiced,
		AttestedBy: "operator:paul", Reference: "runpod-invoice:cpu-pod",
	})
	require.NoError(t, err)
	require.EqualValues(t, invoiced, *closed.Authorization.SettlementAmount)

	open("zero-records")
	qual := settleProviderCost(t, ctx, database, client, "zero-records", 0)
	require.Equal(t, billing.OperationAuthorizationSettled, billing.OperationAuthorizationState(qual.Authorization.State))
	require.EqualValues(t, 0, *qual.Authorization.SettlementAmount, "records of zero are zero-cost evidence")
}
