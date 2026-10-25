//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestCalendarSubscription(t *testing.T) {
	t.Run("OR-J14-S06/calendar-anchor-survives-short-month-restart-and-archive", func(t *testing.T) {
		anchor := time.Date(2025, time.January, 31, 12, 34, 56, 0, time.UTC)
		a, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(anchor.Sub(w.clock.Now())) })
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "calendar", DisplayName: "Monthly membership", Entitlements: []string{"calendar:paid"}})
		require.NoError(t, err)
		for _, invalid := range []billing.CreatePriceParams{
			{ProductID: product.ID, Key: "ambiguous", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(720), BillingIntervalMonths: ptr(1)},
			{ProductID: product.ID, Key: "zero", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalMonths: ptr(0)},
			{ProductID: product.ID, Key: "overflow", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalMonths: ptr(1201)},
			{ProductID: product.ID, Key: "trial", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalMonths: ptr(1), TrialUnitAmount: ptr(int64(0)), TrialDurationHours: ptr(24)},
			{ProductID: product.ID, Key: "solana", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalMonths: ptr(1), SellVia: []string{"solana"}},
		} {
			_, err := a.alpha.CreatePrice(t.Context(), invalid)
			require.ErrorIs(t, err, billing.ErrInvalid)
		}
		require.Empty(t, provider.effects(t), "invalid or unsupported calendar terms cannot move money")
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalMonths: ptr(1)})
		require.NoError(t, err)
		require.Nil(t, price.BillingIntervalHours)
		require.Equal(t, 1, *price.BillingIntervalMonths)
		token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "calendar-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.Equal(t, billing.OrderComplete, order.Status)
		require.Nil(t, order.Lines[0].BillingIntervalHours)
		require.Equal(t, 1, *order.Lines[0].BillingIntervalMonths)
		subID := *order.Lines[0].SubscriptionID
		sub, err := a.alpha.GetSubscription(t.Context(), subID)
		require.NoError(t, err)
		require.Equal(t, time.Date(2025, 2, 28, 12, 34, 56, 0, time.UTC), *sub.CurrentPeriodEndsAt)

		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+subID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Pause renewal"})
		require.Equal(t, http.StatusOK, status, string(raw))
		resumed, err := a.alpha.ResumeSubscription(t.Context(), subID)
		require.NoError(t, err)
		require.Equal(t, *sub.CurrentPeriodEndsAt, *resumed.CurrentPeriodEndsAt)
		require.Len(t, provider.effects(t), 1, "cancel and resume retain the paid calendar period")

		// Replacing the catalog price cannot rewrite this customer's paid agreement.
		_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 20_000_000, Currency: "USD", BillingIntervalMonths: ptr(2)})
		require.NoError(t, err)
		fleet := startFleet(t, a.world)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) - time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Len(t, provider.effects(t), 1, "no early calendar charge")
		a.clock.Advance(time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		march := time.Date(2025, 3, 31, 12, 34, 56, 0, time.UTC)
		require.Eventually(t, func() bool {
			s, err := a.alpha.GetSubscription(t.Context(), subID)
			return err == nil && s.CurrentPeriodEndsAt.Equal(march)
		}, 20*time.Second, 20*time.Millisecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Len(t, provider.effects(t), 2, "duplicate due workers collect one calendar period")

		// A restart after February must recover the 31st, not repeat 28 elapsed days.
		require.NoError(t, fleet.Stop(context.Background()))
		require.NoError(t, a.engine.Close(context.Background()))
		a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
		require.NoError(t, err)
		a.alpha, err = a.engine.MerchantBySlug("alpha")
		require.NoError(t, err)
		fleet = startFleet(t, a.world)
		a.clock.Advance(march.Sub(a.clock.Now()) + time.Hour)
		runRegistered(t, fleet, "openrails.dunning", 2)
		april := time.Date(2025, 4, 30, 12, 34, 56, 0, time.UTC)
		require.Eventually(t, func() bool {
			s, err := a.alpha.GetSubscription(t.Context(), subID)
			return err == nil && s.CurrentPeriodStartsAt.Equal(march) && s.CurrentPeriodEndsAt.Equal(april)
		}, 20*time.Second, 20*time.Millisecond)
		for _, effect := range provider.effects(t) {
			require.EqualValues(t, 1000, effect.Amount)
		}
		require.Len(t, provider.effects(t), 3)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"calendar:paid"}, At: april.Add(-time.Microsecond)})
		require.NoError(t, err)
		require.True(t, access.Entitlements["calendar:paid"], "default access follows each unequal calendar period")
		_, err = a.alpha.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Renamed")})
		require.NoError(t, err)
		require.NoError(t, fleet.Stop(context.Background()))
		archive := archiveBytes(t, a.alpha)
		_, restored := archiveTarget(t, a.world)
		_, err = restored.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		copy, err := restored.GetSubscription(t.Context(), subID)
		require.NoError(t, err)
		require.Equal(t, march, *copy.CurrentPeriodStartsAt)
		require.Equal(t, april, *copy.CurrentPeriodEndsAt)
		copyOrder, err := restored.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, 1, *copyOrder.Lines[0].BillingIntervalMonths)
		require.Nil(t, copyOrder.Lines[0].BillingIntervalHours)
		require.Len(t, provider.effects(t), 3, "archive transfer never collects a payment")
		refund, err := a.alpha.RefundPayment(t.Context(), *order.Payment.PaymentID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "Refund the initial calendar period", IdempotencyKey: "calendar-first-refund"})
		require.NoError(t, err)
		require.EqualValues(t, -10_000_000, refund.Amount)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.refund_access"})
		require.NoError(t, err)
		require.Empty(t, findings.Items, "the calendar line identifies its exact initial access window")
		access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"calendar:paid"}, At: april.Add(-time.Microsecond)})
		require.NoError(t, err)
		require.True(t, access.Entitlements["calendar:paid"], "refunding January leaves March's paid access intact")

	})
}
