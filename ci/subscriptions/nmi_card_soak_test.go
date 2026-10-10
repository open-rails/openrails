//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
)

// storedCard is a saved method's local row, read as the engine sees it, and
// the storing transaction of its live card-on-file mandate.
type storedCard struct {
	vault, billing, lastFour, cardType, onFileRef string
}

func (w *world) storedCard(methodID string) storedCard {
	w.t.Helper()
	id := strings.TrimPrefix(methodID, "pm_")
	var c storedCard
	var last, brand, onFile *string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT pm.rail_customer_ref, pm.rail_method_ref, pm.card_last4, pm.card_brand,
		(SELECT m.initial_transaction_id FROM billing.mandates m WHERE m.merchant_id = pm.merchant_id AND m.payment_method_id = pm.id
		   AND m.kind = 'card_on_file' AND m.status = 'active')
		FROM billing.payment_methods pm WHERE pm.id = $1::uuid`), id).Scan(&c.vault, &c.billing, &last, &brand, &onFile))
	if onFile != nil {
		c.onFileRef = *onFile
	}
	if last != nil {
		c.lastFour = *last
	}
	if brand != nil {
		c.cardType = *brand
	}
	return c
}

// A card saved at NMI is verified (type=validate) as the storing transaction
// of its card-on-file agreement; a membership moving onto it verifies its own
// recurring agreement, which the next merchant-initiated renewal references.
func TestNMISavedCardRecurringAgreement(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", tp)
			method := e.c.saveCard("nmi", mastercard)
			saved := w.storedCard(method)
			verifications := w.nmi.Validations(saved.vault)
			require.NotEmpty(t, verifications, "the saved card was verified at NMI")
			verification := verifications[0]
			require.Equal(t, verification.TransactionID, saved.onFileRef, "the verification stores the card for reuse")
			require.Equal(t, "customer", verification.Form.Get("initiated_by"))
			require.Equal(t, "stored", verification.Form.Get("stored_credential_indicator"))
			require.Empty(t, verification.Form.Get("billing_method"), "saving a card declares no recurring agreement")
			require.Empty(t, verification.Form.Get("initial_transaction_id"))
			require.Empty(t, verification.Form.Get("amount"), "a verification moves no funds")
			require.Equal(t, "mastercard", saved.cardType)

			id, err := billing.ParsePaymentMethodID(method)
			require.NoError(t, err)
			_, err = w.client[tp].SetSubscriptionPaymentMethod(t.Context(), e.sub, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &id})
			require.NoError(t, err)
			verifications = w.nmi.Validations(saved.vault)
			require.Len(t, verifications, 2, "the move verifies the subscription's recurring agreement on the card")
			agreement := verifications[1]
			require.True(t, agreement.Approved)
			require.Equal(t, "recurring", agreement.Form.Get("billing_method"))
			require.Equal(t, "stored", agreement.Form.Get("stored_credential_indicator"))
			require.True(t, strings.HasPrefix(agreement.Form.Get("orderid"), "pmr-"))
			mandate := w.subscriptionMandate(tp, e.c.cid(), e.sub)
			require.Equal(t, agreement.TransactionID, str(mandate.InitialTransactionID))
			require.Equal(t, id, *mandate.PaymentMethodID)
			sales := len(w.nmi.ledger(""))
			end := e.periodEnd()
			e.toFreshPeriodEnd()
			w.runRenewals()
			require.True(t, e.periodEnd().After(end), "the membership renews on the new card")
			require.Len(t, w.nmi.ledger(""), sales+1, "exactly one renewal charge")
			renewal := w.nmi.LastSale()
			require.Equal(t, saved.vault, renewal.Vault)
			require.Equal(t, mastercard.Last4, renewal.Card.Last4)
			require.Equal(t, "merchant", renewal.InitiatedBy)
			require.Equal(t, "used", renewal.Indicator)
			require.Equal(t, agreement.TransactionID, renewal.Initial, "the MIT references the subscription's agreement on the new card")
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// A card the issuer refuses to verify is not saved: typed card decline, no
// local method and no vault left at NMI. A funds decline still verifies.
func TestNMISavedCardVerificationRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	vaults := func() int { return len(w.nmi.Vaults()) }
	before := vaults()
	status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"],
		"payment_token": w.nmi.Tokenize(card{Brand: "visa", Last4: "0119", Decline: "200"}), "billing_details": map[string]any{"name": "Refused Payer"}})
	require.Equal(t, http.StatusPaymentRequired, status, "%v", body)
	require.Equal(t, "card_declined", errorCode(body))
	require.Equal(t, before, vaults(), "the refused card's vault is removed")
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_methods WHERE customer_id = $1::uuid`), c.id).Scan(&n))
	require.Zero(t, n)

	funds := c.saveCard("nmi", card{Brand: "visa", Last4: "0002", Decline: "202"})
	require.NotEmpty(t, w.storedCard(funds).onFileRef, "insufficient funds does not fail a verification")
	require.Zero(t, len(w.nmi.Attempts()))
	require.Empty(t, w.nmi.Unexpected())
}

// NMI's customer record may omit card_type. An in-place card replacement
// completes with the brand read from the masked number; a record missing the
// last four or expiry ends the replacement with an operator finding instead
// of retrying forever.
func TestNMIInPlaceCardReplacementWithoutBrand(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", mastercard)
	vault := w.storedCard(method).vault
	w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.NoBrand = true })
	replacement := card{Brand: "visa", Last4: "1111"}
	c.must(http.MethodPut, "/payment-methods/"+method, "", map[string]any{"payment_token": w.nmi.Tokenize(replacement)})
	w.settle()
	got := w.storedCard(method)
	require.Equal(t, "1111", got.lastFour, "the replacement completes")
	require.Equal(t, "visa", got.cardType, "the brand comes from the masked number")

	gap := c.saveCard("nmi", visa)
	gapVault := w.storedCard(gap).vault
	w.nmi.EditVault(gapVault, func(v *nmimock.Vault) { v.Card.Last4 = "" })
	status, body := c.call(http.MethodPut, "/payment-methods/"+gap, "", map[string]any{"payment_token": w.nmi.Tokenize(mastercard)})
	t.Logf("data gap replacement: %d %v", status, body)
	w.settle()
	require.Contains(t, w.openFindings("life.payment_method_update.provider_data_gap"), strings.TrimPrefix(gap, "pm_"))
	var open int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type = 'nmi_payment_method_update'
		AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')`)).Scan(&open))
	require.Zero(t, open, "a deterministic data gap is not retried")
	require.Equal(t, visa.Last4, w.storedCard(gap).lastFour, "the local card is unchanged")
}

// A tier group can be assigned to, and changed on, a product with live
// subscriptions; the change reaches the memberships. Joining two products a
// customer both holds into one group is refused with a typed error.
func TestTierGroupOnLiveProduct(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	product := w.subscription(embedded, e.sub).ProductID
	for i, tp := range []topology{embedded, remote} {
		group, rank := fmt.Sprintf("g%d-%s", i, uuid.NewString()[:6]), i+1
		updated, err := w.client[tp].UpdateProduct(t.Context(), product, billing.UpdateProductParams{TierGroup: catalog.Value(group), TierRank: catalog.Value(rank)})
		require.NoError(t, err, "a live product takes a tier group")
		require.NotNil(t, updated.TierGroup)
		require.Equal(t, group, *updated.TierGroup)
		var denormalized string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT tier_group FROM billing.subscriptions WHERE id = $1::uuid`), e.sub.UUID()).Scan(&denormalized))
		require.Equal(t, group, denormalized, "the membership follows its product's group")
	}

	other := w.membership("content:other", 4_990_000)
	e.c.subscribeAgain(embedded, "nmi", other.ID.String(), "content:other", e.method)
	group := "joined-" + uuid.NewString()[:6]
	_, err := w.client[remote].UpdateProduct(t.Context(), product, billing.UpdateProductParams{TierGroup: catalog.Value(group)})
	require.NoError(t, err)
	_, err = w.client[remote].UpdateProduct(t.Context(), other.ProductID, billing.UpdateProductParams{TierGroup: catalog.Value(group)})
	requireCode(t, err, http.StatusConflict, "product_tier_group_conflict")
}
