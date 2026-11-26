//go:build integration

package critical_test

import (
	"testing"

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
	t.Run("OR-J22-S02/a-revocation-posts-settled-usage-first-and-conserves-funds", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer, grant := fundedCustomer(t, w)
		g := newGate(t, w.alpha, openrails.GateDeps{})
		ticket := gateAdmit(t, g, computeSpend(customer, "held-credit", 450))
		// An open ticket holds nothing on the server.
		balance(t, w.alpha, customer, 1000, 0, 1000)
		require.NoError(t, ticket.Capture(t.Context(), billing.CaptureUsage{Dimensions: map[string]int64{"units": 1}}))
		gateFlush(t, g)
		balance(t, w.alpha, customer, 550, 450, 1000)
		// The revocation posts the settled usage first: the lot funds it, and
		// only what is left is revoked.
		revoked, err := w.alpha.RevokeCreditGrant(t.Context(), "", grant.ID, billing.RevokeCreditGrantParams{Reason: "Revoke only unused credit"})
		require.NoError(t, err)
		require.EqualValues(t, 550, revoked.RevokedAmount)
		final, err := w.alpha.GetCreditGrant(t.Context(), grant.ID)
		require.NoError(t, err)
		require.EqualValues(t, 450, final.SpentAmount)
		require.Zero(t, final.RemainingAmount)
		require.EqualValues(t, 1000, final.SpentAmount+final.RemainingAmount+final.RevokedAmount+final.ExpiredAmount)
		balance(t, w.alpha, customer, 0, 0, 0)
	})
}
