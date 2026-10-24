//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func publicGrant(t *testing.T, a *authorityWorld, key string, hours int) billing.ProductAccessGrant {
	t.Helper()
	product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"library:read"}})
	require.NoError(t, err)
	grants, err := a.alphaRemote.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: key, Items: []billing.CreateProductAccessParams{{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), ProductID: product.ID, Hours: ptr(hours), Reason: billing.GrantReasonComp}}})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, product.ID, grants[0].ProductID)
	require.Equal(t, billing.ProductAccessSourceGrant, grants[0].SourceType)
	return grants[0]
}

func publicPastClock(w *world, ago time.Duration) {
	// PostgreSQL's public timestamps have microsecond precision. Pin the
	// controlled input to that precision before creating any domain facts.
	w.clock.Advance(w.clock.Now().Truncate(time.Microsecond).Add(-ago).Sub(w.clock.Now()))
}

func publicAccess(t *testing.T, a *authorityWorld, at time.Time, expected bool) {
	t.Helper()
	check, err := a.alphaRemote.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Entitlements: []string{"library:read", "never:granted"}, At: at})
	require.NoError(t, err)
	require.Equal(t, expected, check.Entitlements["library:read"], at.String())
	require.False(t, check.Entitlements["never:granted"])
}

func TestPublicEntitlements(t *testing.T) {
	t.Run("OR-J27-S01/purchase-and-grant-overlap-counts-once", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { publicPastClock(w, 72*time.Hour) })
		_, price := publicSale(t, a, "purchased-day", 9_990_000, ptr(24))
		order := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "overlap-purchase")
		start := a.clock.Now()
		a.clock.Advance(12 * time.Hour)
		grant := publicGrant(t, a, "overlapping-comp", 24)
		require.Equal(t, start.Add(12*time.Hour), grant.StartsAt)
		require.Equal(t, start.Add(36*time.Hour), *grant.EndsAt)
		for _, hours := range []int{0, 12, 24, 35} {
			publicAccess(t, a, start.Add(time.Duration(hours)*time.Hour), true)
		}
		publicAccess(t, a, start.Add(36*time.Hour), false)
		a.clock.Advance(24 * time.Hour)
		tenure, err := a.alphaRemote.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: order.CustomerID, Entitlements: []string{"library:read"}})
		require.NoError(t, err)
		require.EqualValues(t, 129600, tenure.Seconds["library:read"], "36 hours, not the sum of two overlapping 24-hour sources")
	})
	t.Run("OR-J27-S02/revoking-one-source-preserves-the-other", func(t *testing.T) {
		a, _ := newPaymentWorld(t, func(w *world) { publicPastClock(w, 72*time.Hour) })
		first := publicGrant(t, a, "short-source", 24)
		second := publicGrant(t, a, "surviving-source", 48)
		a.clock.Advance(time.Hour)
		require.NoError(t, a.alphaRemote.RevokeProductAccess(t.Context(), first.ID, billing.RevokeProductAccessParams{Reason: "One complimentary source ends"}))
		publicAccess(t, a, a.clock.Now(), true)
		publicAccess(t, a, first.StartsAt.Add(24*time.Hour), true)
		publicAccess(t, a, *second.EndsAt, false)
		rows, err := a.alphaRemote.ListProductAccess(t.Context(), billing.ProductAccessListParams{IDs: []billing.ProductAccessID{first.ID, second.ID}})
		require.NoError(t, err)
		require.Len(t, rows.Items, 2)
		for _, row := range rows.Items {
			if row.ID == first.ID {
				require.NotNil(t, row.RevokedAt)
			} else {
				require.Equal(t, second.ID, row.ID)
				require.Nil(t, row.RevokedAt)
			}
		}
	})
	t.Run("OR-J27-S03/exact-window-boundaries-and-historical-tenure", func(t *testing.T) {
		a, _ := newPaymentWorld(t, func(w *world) { publicPastClock(w, 48*time.Hour) })
		grant := publicGrant(t, a, "six-hour-window", 6)
		for _, sample := range []struct {
			at      time.Time
			allowed bool
		}{{grant.StartsAt.Add(-time.Microsecond), false}, {grant.StartsAt, true}, {grant.EndsAt.Add(-time.Microsecond), true}, {*grant.EndsAt, false}} {
			publicAccess(t, a, sample.at, sample.allowed)
		}
		a.clock.Advance(6 * time.Hour)
		for _, sample := range []struct {
			at      time.Time
			seconds int64
		}{{grant.StartsAt, 0}, {grant.StartsAt.Add(3 * time.Hour), 10800}, {*grant.EndsAt, 21600}} {
			tenure, err := a.alphaRemote.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: grant.CustomerID, Entitlements: []string{"library:read", "never:granted"}, At: sample.at})
			require.NoError(t, err)
			require.Equal(t, sample.seconds, tenure.Seconds["library:read"])
			require.Zero(t, tenure.Seconds["never:granted"])
		}
		_, err := a.alphaRemote.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: grant.CustomerID, Entitlements: []string{"library:read"}, At: a.clock.Now().Add(time.Hour)})
		require.ErrorIs(t, err, billing.ErrInvalid)
	})
	t.Run("OR-J27-S04/archive-and-refund-obey-explicit-access-policy", func(t *testing.T) {
		for _, revoke := range []bool{false, true} {
			t.Run(map[bool]string{false: "keep-access", true: "revoke-purchased-access"}[revoke], func(t *testing.T) {
				a, p := newPaymentWorld(t)
				product, price := publicSale(t, a, "refund-policy", 9_990_000, nil)
				order := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "refund-policy-purchase")
				_, err := a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
				require.NoError(t, err)
				publicAccess(t, a, time.Time{}, true)
				refund, err := a.alphaRemote.RefundPayment(t.Context(), *order.Payment.PaymentID, billing.RefundPaymentParams{Full: true, RevokeAccess: revoke, Reason: "Explicit customer refund policy", IdempotencyKey: "policy-refund"})
				require.NoError(t, err)
				require.Equal(t, billing.PaymentSucceeded, refund.Status)
				publicAccess(t, a, time.Time{}, !revoke)
				retained, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
				require.NoError(t, err)
				require.Equal(t, billing.OrderComplete, retained.Status)
				require.Equal(t, order.Payment.PaymentID, retained.Payment.PaymentID)
				payment, err := a.alphaRemote.GetPayment(t.Context(), *order.Payment.PaymentID)
				require.NoError(t, err)
				require.Equal(t, billing.PaymentRefunded, payment.Status)
				require.EqualValues(t, 9_990_000, payment.AmountRefunded)
				require.Len(t, p.effects(t), 2)
			})
		}
	})
}
