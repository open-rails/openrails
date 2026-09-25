//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
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
			require.Zero(t, w.nmi.saleAttempts())
			require.False(t, c.entitled("content:post"))
		})
	}
}

// Renewals charge the subscription's stored method, never the customer's
// current default: with the stored method gone the due pass refuses the
// renewal with a clear reason and charges nothing.
func TestRenewalNeverFallsBackToDefaultCard(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	other := e.c.saveCard("nmi", mastercard)
	e.c.setDefault(embedded, other)
	subID := strings.TrimPrefix(e.sub.String(), "sub_")
	// The FK's ON DELETE SET NULL outcome of a removed stored method.
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE openrails.subscriptions SET payment_method_id = NULL WHERE id = $1::uuid`), subID)
	require.NoError(t, err)
	charges := len(e.providerLedger())
	e.toPeriodEnd()
	w.runRenewals()
	require.Len(t, e.providerLedger(), charges, "nothing is charged, least of all the default card")
	require.Contains(t, w.openFindings("life.due_pass.refused"), subID)
	var action string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT recommended_action FROM openrails.reconciliation_findings WHERE finding_type = 'life.due_pass.refused' AND subject_key = $1`), subID).Scan(&action))
	require.Contains(t, action, "never fall back to the default card")
}
