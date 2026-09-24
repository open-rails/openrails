//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// storedCard is a saved method's local row, read as the engine sees it.
type storedCard struct {
	vault, billing, lastFour, cardType, recurringRef string
}

func (w *world) storedCard(methodID string) storedCard {
	w.t.Helper()
	id := strings.TrimPrefix(methodID, "pm_")
	var c storedCard
	var last, brand *string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT rail_customer_ref, rail_method_ref, last_four, card_type, stored_credential_recurring_ref
		FROM openrails.payment_methods WHERE id = $1::uuid`), id).Scan(&c.vault, &c.billing, &last, &brand, &c.recurringRef))
	if last != nil {
		c.lastFour = *last
	}
	if brand != nil {
		c.cardType = *brand
	}
	return c
}

// A card saved at NMI is verified as the initial customer-initiated
// transaction of a recurring agreement (type=validate, no funds), so an
// engine membership can move onto it: the next renewal is a merchant-
// initiated charge of the new card referencing that verification.
func TestNMISavedCardRecurringAgreement(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", tp)
			method := e.c.saveCard("nmi", mastercard)
			saved := w.storedCard(method)
			verification := w.nmi.validationOf(saved.vault)
			require.NotNil(t, verification, "the saved card was verified at NMI")
			require.Equal(t, verification.TransactionID, saved.recurringRef, "the verification is the card's recurring agreement")
			require.Equal(t, "customer", verification.Form.Get("initiated_by"))
			require.Equal(t, "stored", verification.Form.Get("stored_credential_indicator"))
			require.Equal(t, "recurring", verification.Form.Get("billing_method"))
			require.Empty(t, verification.Form.Get("initial_transaction_id"))
			require.Empty(t, verification.Form.Get("amount"), "a verification moves no funds")
			require.Equal(t, "mastercard", saved.cardType)

			id, err := openrails.ParsePaymentMethodID(method)
			require.NoError(t, err)
			require.NoError(t, w.client[tp].UpdateSubscriptionPaymentMethod(t.Context(), e.sub, openrails.UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: id}))
			sales := len(w.nmi.ledger(""))
			end := e.periodEnd()
			e.toPeriodEnd()
			w.runRenewals()
			require.True(t, e.periodEnd().After(end), "the membership renews on the new card")
			require.Len(t, w.nmi.ledger(""), sales+1, "exactly one renewal charge")
			renewal := w.nmi.lastSale()
			require.Equal(t, saved.vault, renewal.Vault)
			require.Equal(t, mastercard.Last4, renewal.Card.Last4)
			require.Equal(t, "merchant", renewal.InitiatedBy)
			require.Equal(t, "used", renewal.Indicator)
			require.Equal(t, verification.TransactionID, renewal.Initial, "the MIT references the new card's agreement")
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// A card the issuer refuses to verify is not saved: typed card decline, no
// local method and no vault left at NMI. A funds decline still verifies.
func TestNMISavedCardVerificationRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	vaults := func() int {
		w.nmi.mu.Lock()
		defer w.nmi.mu.Unlock()
		return len(w.nmi.vaults)
	}
	before := vaults()
	status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"provider": "nmi", "psp_id": w.psp["nmi"],
		"payment_token": w.nmi.tokenize(card{Brand: "visa", Last4: "0119", Decline: "200"}), "name_on_card": "Refused Payer"})
	require.Equal(t, http.StatusPaymentRequired, status, "%v", body)
	require.Equal(t, "card_declined", errorCode(body))
	require.Equal(t, before, vaults(), "the refused card's vault is removed")
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM openrails.payment_methods WHERE customer_id = $1::uuid`), c.id).Scan(&n))
	require.Zero(t, n)

	funds := c.saveCard("nmi", card{Brand: "visa", Last4: "0002", Decline: "202"})
	require.NotEmpty(t, w.storedCard(funds).recurringRef, "insufficient funds does not fail a verification")
	require.Zero(t, w.nmi.saleAttempts())
	require.Empty(t, w.nmi.unexpected())
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
	w.nmi.mu.Lock()
	w.nmi.vaults[vault].NoBrand = true
	w.nmi.mu.Unlock()
	replacement := card{Brand: "visa", Last4: "1111"}
	c.must(http.MethodPut, "/payment-methods/"+method, "", map[string]any{"provider": "nmi", "payment_token": w.nmi.tokenize(replacement), "last_four": replacement.Last4, "expiry_date": "12/35"})
	w.settle()
	got := w.storedCard(method)
	require.Equal(t, "1111", got.lastFour, "the replacement completes")
	require.Equal(t, "visa", got.cardType, "the brand comes from the masked number")

	gap := c.saveCard("nmi", visa)
	gapVault := w.storedCard(gap).vault
	w.nmi.mu.Lock()
	w.nmi.vaults[gapVault].Card.Last4 = ""
	w.nmi.mu.Unlock()
	status, body := c.call(http.MethodPut, "/payment-methods/"+gap, "", map[string]any{"provider": "nmi", "payment_token": w.nmi.tokenize(mastercard), "last_four": mastercard.Last4, "card_type": mastercard.Brand, "expiry_date": "12/35"})
	t.Logf("data gap replacement: %d %v", status, body)
	w.settle()
	require.Contains(t, w.openFindings("life.payment_method_update.provider_data_gap"), strings.TrimPrefix(gap, "pm_"))
	var open int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM openrails.rail_intents WHERE intent_type = 'nmi_payment_method_update'
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
		updated, err := w.client[tp].Products.Update(t.Context(), product, &openrails.ProductUpdateParams{TierGroup: &group, SetTierGroup: true, TierRank: &rank})
		require.NoError(t, err, "a live product takes a tier group")
		require.NotNil(t, updated.TierGroup)
		require.Equal(t, group, *updated.TierGroup)
		var denormalized string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT tier_group FROM openrails.subscriptions WHERE id = $1::uuid`), e.sub.UUID()).Scan(&denormalized))
		require.Equal(t, group, denormalized, "the membership follows its product's group")
	}

	other := w.membership("content:other", 4_990_000)
	e.c.subscribeAgain(embedded, "nmi", other.ID, "content:other", e.method)
	group := "joined-" + uuid.NewString()[:6]
	_, err := w.client[remote].Products.Update(t.Context(), product, &openrails.ProductUpdateParams{TierGroup: &group, SetTierGroup: true})
	require.NoError(t, err)
	_, err = w.client[remote].Products.Update(t.Context(), other.ProductID, &openrails.ProductUpdateParams{TierGroup: &group, SetTierGroup: true})
	requireCode(t, err, http.StatusConflict, "product_tier_group_conflict")
}
