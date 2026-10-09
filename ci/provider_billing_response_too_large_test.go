//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// A provider response too large to keep carries no body. A nil RawBody, from
// Go or as JSON null, is stored as the empty body: the qualification refuses
// and the hold stays reserved, and either spelling replays.
func TestProviderBillingResponseTooLargeHasNoBody(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "too-large-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())

	customer := billing.CustomerID(uuid.New())
	_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
	require.NoError(t, err)
	_, err = client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: uuid.NewString()})
	require.NoError(t, err)
	body := []byte(`{"rental":"too-large"}`)
	_, err = client.OpenOperationAuthorization(ctx, billing.OpenOperationAuthorizationParams{
		OperationID: "too-large", CustomerID: customer, RecordOwner: "user:1", Currency: "USD", Amount: 400_000,
		ClaimReference: "claim:too-large", AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
	})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Microsecond)
	start, end := now.Add(-2*time.Hour), now.Add(-time.Hour)
	in := billing.RecordProviderBillingObservationParams{
		OperationID: "too-large", ObservationID: "too-large:1",
		Lifecycle: billing.ProviderBillingLifecycleEvidence{
			Provider: "provider", ProviderResourceID: "pod-too-large",
			ProviderLifetimeStartsAt: start, ProviderLifetimeEndsAt: end, ProviderAbsentAt: end,
			ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
			WindowsClosedAt: end, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
		},
		NormalizedQuery: "pod=too-large", QueryStartsAt: start, QueryEndsAt: end,
		Refusal: &billing.ProviderBillingObservationRefusal{Kind: billing.ProviderBillingRefusalResponseTooLarge},
	}

	qual, err := client.RecordProviderBillingObservation(ctx, in)
	require.NoError(t, err)
	require.Equal(t, billing.ProviderBillingQualificationRefused, qual.State)
	require.Equal(t, billing.ProviderBillingProviderEvidenceRefused, qual.Reason)
	require.False(t, qual.Replayed)
	require.Equal(t, billing.OperationAuthorizationOpen, qual.Authorization.State)
	bal, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 400_000, bal.HeldAmount)

	var available bool
	var length int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT raw_body_available, octet_length(raw_body_bytes) FROM `+pgx.Identifier{f.schema, "cost_observations"}.Sanitize()+
		` WHERE merchant_id = $1 AND operation_id = $2 AND observation_id = $3`, client.MerchantID().UUID(), in.OperationID, in.ObservationID).Scan(&available, &length))
	require.False(t, available)
	require.Zero(t, length)

	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	for _, raw := range [][]byte{nil, {}} {
		in.RawBody = raw
		replay, err := client.RecordProviderBillingObservationTx(ctx, tx, in)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, billing.ProviderBillingQualificationRefused, replay.State)
	}
}
