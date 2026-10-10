//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/nmimock"
)

// editCard changes a payment method's card at Stripe.
func (f *stripeFake) editCard(pm string, edit func(card obj)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.methods[pm]["card"].(obj))
}

// detach detaches a payment method from its customer at Stripe.
func (f *stripeFake) detach(pm string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods[pm]["customer"] = nil
}

// A card changed through Stripe's API (an expiry edit in Stripe's portal) is
// a version of the same method, read from Stripe. A card Stripe detached is
// removed and its agreements end.
func TestStripePaymentMethodUpdatedAndDetached(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	pm := w.methodRow(e.method, "rail_method_ref")
	customer := w.stripe.customerOf(pm)

	w.stripe.editCard(pm, func(card obj) { card["exp_month"] = 7 })
	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("payment_method.updated", obj{"object": "payment_method", "id": pm, "customer": customer})))
	require.Equal(t, []string{"customer_save/saved", "provider_read/updated"}, w.methodVersions(e.method))
	require.Equal(t, "7", w.methodRow(e.method, "card_exp_month::text"))
	require.Equal(t, "active", w.methodRow(e.method, "status"))

	w.stripe.detach(pm)
	require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("payment_method.detached", obj{"object": "payment_method", "id": pm, "customer": nil})))
	require.Equal(t, "removed", w.methodRow(e.method, "status"))
	for _, state := range w.mandateStates(e.method) {
		require.True(t, strings.HasSuffix(state, "/ended/payment_method_removed"), "every agreement on a removed card ends, got %v", w.mandateStates(e.method))
	}
	view := e.c.must(http.MethodGet, "/payment-methods", "", nil)["data"].([]any)[0].(map[string]any)
	require.Equal(t, "removed", view["status"])
	require.Equal(t, false, view["health"].(map[string]any)["active"])
}

// The customer edits a saved card in place: the expiry reaches the card's
// holder and is a version; billing details change; reuse is withdrawn and
// given again. The card number cannot change, and a month already past is
// refused.
func TestPatchPaymentMethod(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			path := "/payment-methods/" + method

			out := c.must(http.MethodPatch, path, "", map[string]any{"exp_month": 9, "exp_year": 2031,
				"billing_details": map[string]any{"name": "Ada Lovelace", "address": map[string]any{"postal_code": "94110"}}})
			card := out["card"].(map[string]any)
			require.Equal(t, []any{float64(9), float64(2031)}, []any{card["exp_month"], card["exp_year"]})
			details := out["billing_details"].(map[string]any)
			require.Equal(t, "Ada Lovelace", details["name"])
			require.Equal(t, "94110", details["address"].(map[string]any)["postal_code"])
			require.Equal(t, []string{"customer_save/saved", "customer_edit/updated"}, w.methodVersions(method))
			if rail == "nmi" {
				require.Equal(t, "0931", w.nmi.Vault(w.vaultOf(method)).Card.Exp, "the vault takes the expiry")
			} else {
				w.stripe.mu.Lock()
				exp := w.stripe.methods[w.methodRow(method, "rail_method_ref")]["card"].(obj)["exp_month"]
				w.stripe.mu.Unlock()
				require.Equal(t, 9, exp, "Stripe takes the expiry")
			}

			out = c.must(http.MethodPatch, path, "", map[string]any{"reusable": false})
			require.Equal(t, false, out["reusable"])
			out = c.must(http.MethodPatch, path, "", map[string]any{"reusable": true})
			require.Equal(t, true, out["reusable"])

			status, body := c.call(http.MethodPatch, path, "", map[string]any{"exp_month": 1, "exp_year": 2020})
			require.Equal(t, http.StatusBadRequest, status, "%v", body)
			status, body = c.call(http.MethodPatch, path, "", map[string]any{"payment_token": "tok_other"})
			require.Equal(t, http.StatusBadRequest, status, "a card number never changes in place: %v", body)
			require.Equal(t, []string{"customer_save/saved", "customer_edit/updated"}, w.methodVersions(method))
		})
	}
}

// interceptVaultSale runs after, once the gateway answered a sale on vault.
func (w *world) interceptVaultSale(vault string, after func()) {
	w.nmi.Intercept(func(r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/transact.php") {
			return false
		}
		form, _ := url.ParseQuery(readBody(r))
		return form.Get("type") == "sale" && form.Get("customer_vault_id") == vault
	}, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
		res := serve()
		after()
		return res, nil
	})
	w.t.Cleanup(w.nmi.ClearIntercepts)
}

// A renewal declined as an expired card reads the vault once before the
// member is asked: NMI's updater already holds the reissued card, which the
// same method takes, and the renewal is collected without asking anyone.
func TestDeclinedCardIsReadFromItsVault(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	vault := w.vaultOf(e.method)
	w.nmi.SetDecline(visa.Last4, "223")
	w.interceptVaultSale(vault, func() {
		w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Decline, v.Card.Exp = "", "0331" })
	})
	e.toFreshPeriodEnd()
	w.runRenewals()
	w.settle()
	w.nmi.ClearIntercepts()
	require.Equal(t, []string{"customer_save/saved", "provider_read/updated"}, w.methodVersions(e.method))
	require.Equal(t, "3", w.methodRow(e.method, "card_exp_month::text"))
	require.Zero(t, e.c.notificationCount("payment_method_update_required"), "the member is not asked")
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status, "the renewal is collected on the reissued card")
}

// When the vault holds nothing newer, the member is asked for a new card, as
// the decline alone would have asked.
func TestDeclinedCardNotReissuedAsksTheMember(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	w.nmi.SetDecline(visa.Last4, "223")
	e.toFreshPeriodEnd()
	w.runRenewals()
	w.settle()
	require.Equal(t, []string{"customer_save/saved"}, w.methodVersions(e.method))
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"))
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status)
}

// Basis Theory's account updater mints a new token for a reissued card: the
// same method adopts it as a version and keeps its agreement, and the
// customer's old number still finds it. A reissue under another brand holds
// the agreement for reconsent, and a contact-cardholder answer on that
// Mastercard card closes it.
func TestBasisTheoryUpdaterKeepsTheMethod(t *testing.T) {
	t.Parallel()
	bt, btURL := newBTFake(t)
	w := prepareWorld(t, 12)
	w.custodians = map[string]openrails.CustodianConfig{"bt": {Kind: "basis_theory", AccountID: "bt-e2e",
		Settings: map[string]any{"public_api_key": "key_public_e2e"}, Secrets: map[string]string{"api_key": "key_private_e2e"}}}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		psps["nmi-bt"] = openrails.NMIPSP{AccountID: "e2e-nmi-bt", SecurityKey: "e2e-nmi-bt-key", WebhookSigningSecret: "nmi_bt_webhook_e2e", Custodian: "bt"}.PSPConfig()
	}
	w.start()
	keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	bt.gateway = func(form url.Values, number string) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
			InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}
	rt := engine.Graph(w.rt).Runtime
	rt.CheckoutService.CustodianSaleService.BTBaseURLOverride = btURL
	ctx := merchant.WithID(t.Context(), rt.ConfiguredMerchant())
	buy := func(c *customer, price billing.PriceID, number string) {
		t.Helper()
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			res, err := rt.CheckoutService.Checkout(ctx, &checkout.CheckoutRequest{PriceID: price.String(), Rail: "nmi-bt", BTTokenIntentID: bt.collect(number), IdempotencyKey: uuid.NewString()}, &checkout.UserIdentity{ID: c.id})
			if err != nil {
				return err
			}
			if res.Status != "success" {
				return fmt.Errorf("checkout answered %+v", res)
			}
			return nil
		}))
		w.settle()
	}
	var custodian uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.custodians`)).Scan(&custodian))
	fold := func(job string, rows ...basistheory.AccountUpdaterResultRow) {
		t.Helper()
		require.NoError(t, rt.DB.RunInMerchantConn(db.WithCustodianID(ctx, custodian), func(ctx context.Context) error {
			_, err := webhooks.FoldAccountUpdaterResults(ctx, rt.DB, rt.SubscriptionLifecycleService, job, rows, w.clock.Now())
			return err
		}))
	}
	const number = "4111111111111111"
	alice := w.newCustomer()
	buy(alice, w.permanent("content:first").ID, number)
	held := w.custodianMethods(alice)
	require.Len(t, held, 1)
	var method string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT 'pm_' || id FROM billing.payment_methods WHERE rail_method_ref = $1`), held[0].Token).Scan(&method))

	fold("job_1", basistheory.AccountUpdaterResultRow{Token: held[0].Token, ResultCode: basistheory.AUUpdatedPAN, NewToken: "tok_reissued",
		NewBrand: "visa", NewLast4: "1881", NewExpirationMonth: "1", NewExpirationYear: "2031", NewFingerprint: "fp_reissued"})
	fold("job_1", basistheory.AccountUpdaterResultRow{Token: held[0].Token, ResultCode: basistheory.AUUpdatedPAN, NewToken: "tok_reissued",
		NewBrand: "visa", NewLast4: "1881", NewExpirationMonth: "1", NewExpirationYear: "2031", NewFingerprint: "fp_reissued"})
	require.Equal(t, []string{"customer_save/saved", "basis_theory_updater/updated"}, w.methodVersions(method), "one version per job and card")
	require.Equal(t, []custodianMethod{{Token: "tok_reissued", Unscheduled: held[0].Unscheduled}}, w.custodianMethods(alice), "the same method takes the new token and keeps its agreement")
	require.Equal(t, "1881", w.methodRow(method, "card_last4"))

	buy(alice, w.permanent("content:second").ID, number)
	require.Equal(t, []custodianMethod{{Token: "tok_reissued", Unscheduled: held[0].Unscheduled}}, w.custodianMethods(alice), "the old number finds the method through its history")

	fold("job_2", basistheory.AccountUpdaterResultRow{Token: "tok_reissued", ResultCode: basistheory.AUUpdatedPAN, NewToken: "tok_mastercard",
		NewBrand: "mastercard", NewLast4: "5100", NewExpirationMonth: "2", NewExpirationYear: "2032", NewFingerprint: "fp_mastercard"})
	require.Equal(t, []string{"card_on_file/requires_reconsent"}, w.mandateStates(method), "another brand waits for the customer")

	fold("job_3", basistheory.AccountUpdaterResultRow{Token: "tok_mastercard", ResultCode: basistheory.AUContactCardholder})
	require.Equal(t, "closed", w.methodRow(method, "status"), "Mastercard's contact answer is a closed account")
	require.Equal(t, []string{"customer_save/saved", "basis_theory_updater/updated", "basis_theory_updater/brand_changed", "basis_theory_updater/closed"}, w.methodVersions(method))
	require.Equal(t, []string{"card_on_file/ended/closed"}, w.mandateStates(method))
}
