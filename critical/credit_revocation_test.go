//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func fundedCustomer(t *testing.T, w *world) (billing.CustomerID, billing.CreditGrant) {
	t.Helper()
	customer := billing.CustomerID(uuid.New())
	grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "funded"}})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	return customer, grants[0]
}

func TestCreditRevocation(t *testing.T) {
	t.Run("OR-J22-S02/capture-and-credit-revocation-conserve-funds", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer, grant := fundedCustomer(t, w)
		g := newGate(t, w.alpha, openrails.GateDeps{})
		ticket := gateAdmit(t, g, computeSpend(customer, "held-credit", 450))
		// The gate reports the open ticket in flight: the credit it may spend
		// is held.
		inFlightReported()
		reason := billing.RevokeCreditGrantParams{Reason: "Revoke only unused credit"}
		_, err := w.alpha.RevokeCreditGrant(t.Context(), "", grant.ID, reason)
		var held *billing.StatusError
		require.ErrorAs(t, err, &held)
		require.Equal(t, billing.CodeCreditGrantHeld, held.Code)
		w.clock.Advance(time.Second)
		start := make(chan struct{})
		cch := make(chan error, 1)
		type revokeResult struct {
			grant *billing.CreditGrant
			err   error
		}
		rch := make(chan revokeResult, 1)
		go func() {
			<-start
			err := ticket.Capture(t.Context(), billing.CaptureUsage{Dimensions: map[string]int64{"units": 1}})
			if err == nil {
				err = g.Flush(t.Context())
			}
			cch <- err
		}()
		go func() {
			<-start
			result, err := w.alpha.RevokeCreditGrant(t.Context(), "", grant.ID, reason)
			rch <- revokeResult{result, err}
		}()
		close(start)
		require.NoError(t, <-cch)
		r := <-rch
		if r.err != nil {
			var refusal *billing.StatusError
			require.ErrorAs(t, r.err, &refusal)
			require.Equal(t, billing.CodeCreditGrantHeld, refusal.Code)
		}
		// Settled usage stays held until its day posts: the revocation never
		// takes what the capture spent.
		fleet := startFleet(t, w)
		nextMonth(w)
		closeMonths(t, fleet)
		final, err := w.alpha.GetCreditGrant(t.Context(), grant.ID)
		require.NoError(t, err)
		require.GreaterOrEqual(t, final.RemainingAmount, int64(0))
		require.GreaterOrEqual(t, final.RevokedAmount, int64(0))
		require.EqualValues(t, 1000, final.SpentAmount+final.RemainingAmount+final.RevokedAmount+final.ExpiredAmount)
		if r.err == nil {
			require.EqualValues(t, 550, final.RevokedAmount)
			require.EqualValues(t, 450, final.SpentAmount)
			balance(t, w.alpha, customer, 0, 0, 0)
		} else {
			require.EqualValues(t, 450, final.SpentAmount)
			require.EqualValues(t, 550, final.RemainingAmount)
			balance(t, w.alpha, customer, 550, 0, 550)
		}
	})
}
