//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/collection"
)

// A charge names its payment method (#1087). A customer with a default card
// who names neither a saved method nor a new card token is refused with
// payment_method_required and nothing is charged.
func TestChargeRequiresExplicitPaymentMethod(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			c.saveCard("nmi", visa)
			require.NotEmpty(t, c.requireOneDefault("a saved card is the default"))
			sale := w.permanent("content:post")
			member := w.membership("content:members", 9_990_000)
			for _, offer := range []struct {
				price, entitlement string
				kind               openrails.OfferKind
			}{{sale.ID, "content:post", openrails.OfferPermanent}, {member.ID, "content:members", openrails.OfferRecurring}} {
				_, err := w.client[tp].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
					OfferKind: offer.kind, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: offer.entitlement, PriceID: offer.price,
					IdempotencyKey: "implicit-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["nmi"], Rail: "nmi"}, Confirm: true,
				})
				require.ErrorIs(t, err, openrails.ErrPaymentMethodRequired)
				require.ErrorIs(t, err, openrails.ErrInvalid)
				var status *openrails.StatusError
				require.ErrorAs(t, err, &status)
				require.Equal(t, http.StatusBadRequest, status.Status)
				require.Equal(t, openrails.CodePaymentMethodRequired, status.Code)
				require.NotNil(t, status.Param)
				require.Equal(t, "payment_method_id", *status.Param)
			}
			require.Empty(t, w.nmi.ledger(""), "no card is charged")
			require.Zero(t, len(w.nmi.Attempts()))
			require.False(t, c.entitled("content:post"))
		})
	}
}

// Renewals charge the subscription's stored method, never the customer's
// current default. With the stored method gone only the member can fix it:
// the membership waits for a new card on the dunning clock, the customer is
// asked for one, access follows the dunning access policy, and the wait ends
// when the dunning window does. It is never held as a system stop.
func TestRenewalNeverFallsBackToDefaultCard(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	e := enroll(t, w, "nmi", embedded)
	other := e.c.saveCard("nmi", mastercard)
	e.c.setDefault(embedded, other)
	subID := strings.TrimPrefix(e.sub.String(), "sub_")
	// The FK's ON DELETE SET NULL outcome of a removed stored method.
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE openrails.subscriptions SET payment_method_id = NULL WHERE id = $1::uuid`), subID)
	require.NoError(t, err)
	charges := len(e.providerLedger())
	end := e.periodEnd()
	e.toPeriodEnd()
	w.runRenewals()
	require.Len(t, e.providerLedger(), charges, "nothing is charged, least of all the default card")
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, "awaiting_method", sub.Status)
	window, err := collection.Window(monthHours)
	require.NoError(t, err)
	require.NotNil(t, sub.GraceEndsAt)
	require.True(t, sub.GraceEndsAt.Equal(end.Add(window)), "the wait ends with the dunning window")
	require.Empty(t, w.openFindings("life.due_pass.refused"), "a member-fixable refusal is not an operator finding")
	require.True(t, e.c.hasNotification("payment_method_update_required"), "the customer is asked for a new card")

	w.advance(end.Add(window).Sub(w.clock.Now()) - time.Hour)
	w.runRenewals()
	w.converge()
	require.True(t, e.c.entitled(e.ent), "access is kept through the dunning window")
	require.Empty(t, w.openFindings("life.renewal.held"), "waiting for the member is not a held renewal")

	w.advance(2 * time.Hour)
	w.runRenewals()
	require.Equal(t, "cancelled", w.subscription(embedded, e.sub).Status, "the wait ends at dunning exhaustion")
	require.False(t, e.c.entitled(e.ent))
	require.Len(t, e.providerLedger(), charges)
}

// hasNotification reports whether the customer has a notification of a kind.
func (c *customer) hasNotification(kind string) bool {
	c.w.t.Helper()
	out := c.must(http.MethodGet, "/notifications?limit=100", "", nil)
	items, _ := out["data"].([]any)
	for _, item := range items {
		if item.(map[string]any)["event_type"] == kind {
			return true
		}
	}
	return false
}
