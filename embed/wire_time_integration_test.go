//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// microInstant is a UTC instant with a non-zero microsecond part, so a
// whole-second wire encoding would visibly lose it.
func microInstant(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Microsecond)
	if t.Nanosecond() == 0 {
		t = t.Add(time.Microsecond)
	}
	return t
}

// TestWireTimesKeepSubSecondPrecisionAcrossDeployments proves the one time
// rule on every Client parameter that used to travel as whole seconds: hold
// deadlines (Admit, ExtendHold), usage occurred_at, rollup and revenue windows,
// and the created_at/expires_at instants on returned DTOs, through the
// embedded in-process and standalone HTTP Clients.
func TestWireTimesKeepSubSecondPrecisionAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	local, err := host.Runtime().Client(openrails.WithCurrency("USD"))
	require.NoError(t, err)
	mid := dbtest.TestMerchantID.UUID()

	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": standalone.Client()} {
		t.Run(name, func(t *testing.T) {
			customer := uuid.New()
			_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
			require.NoError(t, err)
			payer := openrails.CustomerID(customer)
			_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "wire-time", Currency: "USD", Amount: 1_000_000, Source: "wire-time", SourceID: uuid.NewString()})
			require.NoError(t, err)

			// A hold deadline keeps its microseconds and is echoed back exactly.
			deadline := microInstant(time.Now().Add(time.Hour))
			requestID := "wire-time-" + uuid.NewString()
			admitted, err := client.Admit(ctx, openrails.AdmitRequest{
				CustomerID: payer.String(), Invoker: "wire-time", InvokerType: openrails.InvokerTypePayer, Currency: "USD",
				EstimatedAmount: 10_000, ExpiresAt: &deadline, RequestID: requestID, Source: "wire-time",
			})
			require.NoError(t, err)
			require.True(t, admitted.Allowed)
			require.NotNil(t, admitted.HoldExpiresAt)
			require.True(t, admitted.HoldExpiresAt.Equal(deadline), "hold_expires_at %s != declared %s", admitted.HoldExpiresAt, deadline)

			// A replay with the same sub-second deadline is the same operation;
			// a whole-second reading of it would be a changed term.
			replayed, err := client.Admit(ctx, openrails.AdmitRequest{
				CustomerID: payer.String(), Invoker: "wire-time", InvokerType: openrails.InvokerTypePayer, Currency: "USD",
				EstimatedAmount: 10_000, ExpiresAt: &deadline, RequestID: requestID, Source: "wire-time",
			})
			require.NoError(t, err)
			require.True(t, replayed.Replayed)

			extended := microInstant(deadline.Add(30 * time.Minute))
			require.NoError(t, client.ExtendHold(ctx, requestID, extended))
			passed := microInstant(time.Now().Add(-time.Hour))
			require.ErrorIs(t, client.ExtendHold(ctx, requestID, passed), openrails.ErrInvalid, "a past deadline is refused")
			_, err = client.Capture(ctx, requestID, 8_000, &openrails.CaptureUsage{EventType: "invoke", Resource: "wire-time"})
			require.NoError(t, err)

			// A usage event placed at a microsecond instant is found by a window
			// that opens at that instant and missed by one that opens a
			// microsecond later.
			occurred := microInstant(time.Now())
			require.NoError(t, client.RecordUsage(ctx, openrails.UsageReport{
				CustomerID: payer.String(), Invoker: "wire-time", Currency: "USD", EventType: "wire-time-event",
				Dimensions: map[string]int64{"units": 7}, Amount: 0, Resource: "wire-time", Source: "wire-time", SourceID: uuid.NewString(),
				OccurredAt: &occurred,
			}))
			rows, err := client.UsageRollup(ctx, payer.String(), "USD", occurred, occurred.Add(time.Microsecond), "resource")
			require.NoError(t, err)
			require.Len(t, rows, 1, "the event occurred exactly at the window start")
			rows, err = client.UsageRollup(ctx, payer.String(), "USD", occurred.Add(time.Microsecond), occurred.Add(time.Second), "resource")
			require.NoError(t, err)
			require.Empty(t, rows, "one microsecond later the event is outside the window")
			revenue, err := client.ResourceRevenueDaily(ctx, "wire-time", "USD", occurred.Add(-time.Hour), occurred.Add(time.Hour))
			require.NoError(t, err)
			require.Equal(t, "USD", revenue.Currency)

			// Returned instants decode with their microseconds intact.
			pmID, created := uuid.New(), microInstant(time.Now().Add(-time.Minute))
			_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,last_four,card_type,created_at,updated_at) VALUES($1,$2,$3,$4,'nmi',$5::text,$5::text,$5::text,'4242','visa',$6,$6)`,
				pmID, mid, customer, dbtest.EnsureTestPSP(ctx, t, h.Pool(), mid, "nmi"), "wire-time-"+pmID.String(), created)
			require.NoError(t, err)
			methods, err := client.ListPaymentMethods(ctx, customer.String(), openrails.PageOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, methods.Data, 1)
			require.True(t, methods.Data[0].CreatedAt.Equal(created), "created_at %s != %s", methods.Data[0].CreatedAt, created)
		})
	}
}
