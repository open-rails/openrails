//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A merchant-initiated charge names the agreement its customer gave for the
// card it charges (#1166). A replaced card or one reissued under another brand
// carries none until a customer-initiated charge anchors one.

const agreementRequired = "stored_credential_required"

// agreements is a stored card's recurring and unscheduled agreement.
func (w *world) agreements(method string) (recurring, unscheduled string) {
	return w.methodRow(method, "COALESCE(stored_credential_recurring_ref, '')"), w.methodRow(method, "COALESCE(stored_credential_unscheduled_ref, '')")
}

// methodUpdates is a stored card's recorded updates, as source/kind.
func (w *world) methodUpdates(method string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT source || '/' || kind FROM billing.payment_method_updates
		WHERE payment_method_id = $1 ORDER BY occurred_at, id`), strings.TrimPrefix(method, "pm_"))
	require.NoError(w.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(w.t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// notificationCodes is the failure code of each of the customer's
// notifications of a kind.
func (c *customer) notificationCodes(kind string) []string {
	c.w.t.Helper()
	items, _ := c.must(http.MethodGet, "/notifications?limit=100", "", nil)["data"].([]any)
	var out []string
	for _, item := range items {
		n := item.(map[string]any)
		if n["event_type"] == kind {
			data, _ := n["data"].(map[string]any)
			code, _ := data["failure_code"].(string)
			out = append(out, code)
		}
	}
	return out
}

// arrearsInvoice runs up 50 USD of usage for c and invoices it.
func (w *world) arrearsInvoice(c *customer) billing.InvoiceID {
	w.t.Helper()
	ctx, client := w.t.Context(), w.client[embedded]
	const owed = 50_000_000 // the default invoice threshold
	before, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: c.cid()})
	require.NoError(w.t, err)
	_, err = client.UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{CustomerID: c.cid(), CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: owed}}}})
	require.NoError(w.t, err)
	request, expires := uuid.NewString(), w.clock.Now().Add(time.Hour)
	admitted, err := client.Admit(ctx, []billing.AdmitParams{{CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: owed, RequestID: request, ExpiresAt: &expires}})
	require.NoError(w.t, err)
	require.True(w.t, admitted[0].Allowed(), "%+v", admitted)
	_, err = client.CaptureAdmission(ctx, request, billing.CaptureAdmissionParams{Amount: owed})
	require.NoError(w.t, err)
	w.advance(time.Minute)
	res, err := w.jobs.Insert(ctx, invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
	after, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: c.cid()})
	require.NoError(w.t, err)
	for _, invoice := range after.Items {
		if !slices.ContainsFunc(before.Items, func(old billing.Invoice) bool { return old.ID == invoice.ID }) {
			require.EqualValues(w.t, owed, invoice.AmountDue)
			return invoice.ID
		}
	}
	w.t.Fatal("no invoice for the usage")
	return billing.InvoiceID{}
}

// collectInvoices runs the scheduled invoice collection once.
func (w *world) collectInvoices() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), collectInvoicePass{Collect: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
	w.settle()
}

func (w *world) invoice(id billing.InvoiceID) *billing.Invoice {
	w.t.Helper()
	invoice, err := w.client[embedded].GetInvoice(w.t.Context(), id)
	require.NoError(w.t, err)
	return invoice
}

func credentialFields(s *nmimock.Sale) []string {
	return []string{s.InitiatedBy, s.Indicator, s.Initial}
}

// A card replaced in place keeps only the recurring agreement its
// verification anchored. Invoice collection stops for the customer rather
// than name the old card's unscheduled agreement; the customer's own payment
// anchors the new card's, and later collections name it.
func TestReplacedCardDropsUnscheduledAgreement(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	first := w.arrearsInvoice(c)
	w.refreshProviders()
	w.settleCollectionScans()
	c.must(http.MethodPost, "/invoices/"+first.String()+"/pay-now", "pay-"+uuid.NewString(), map[string]any{"payment_method_id": method})
	w.settle()
	_, old := w.agreements(method)
	require.Equal(t, w.nmi.LastSale().TransactionID, old)
	c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": method, "currency": "USD"})

	c.must(http.MethodPut, "/payment-methods/"+method, "", map[string]any{"payment_token": w.nmi.Tokenize(mastercard)})
	w.settle()
	recurring, unscheduled := w.agreements(method)
	require.Empty(t, unscheduled, "the replaced card's unscheduled agreement goes with it")
	verified := w.nmi.Validations(w.vaultOf(method))
	require.Equal(t, verified[len(verified)-1].TransactionID, recurring, "the replacement's verification anchors its recurring agreement")

	second := w.arrearsInvoice(c)
	sent := len(w.nmi.Attempts())
	w.collectInvoices()
	require.Len(t, w.nmi.Attempts(), sent, "no merchant-initiated charge without the new card's agreement")
	stopped := w.invoice(second)
	require.Equal(t, agreementRequired, *stopped.LastCollectionFailureCode)
	require.Nil(t, stopped.NextCollectionAttemptAt, "collection waits for the customer")
	require.Contains(t, c.notificationCodes("payment_method_update_required"), agreementRequired)

	w.refreshProviders()
	c.must(http.MethodPost, "/invoices/"+second.String()+"/pay-now", "pay-"+uuid.NewString(), map[string]any{"payment_method_id": method})
	w.settle()
	sale := w.nmi.LastSale()
	require.Equal(t, mastercard.Last4, sale.Card.Last4)
	require.Equal(t, []string{"customer", "stored", ""}, credentialFields(sale), "the customer's payment starts the new card's agreement")
	_, unscheduled = w.agreements(method)
	require.Equal(t, sale.TransactionID, unscheduled)

	third := w.arrearsInvoice(c)
	w.refreshProviders()
	w.collectInvoices()
	sale = w.nmi.LastSale()
	require.Equal(t, []string{"merchant", "used", unscheduled}, credentialFields(sale), "collection names the new card's agreement")
	require.Equal(t, billing.InvoicePaid, w.invoice(third).Status)
	require.NotEqual(t, old, sale.Initial)
}

// NMI's Account Updater reissues a member's card. A same-brand reissue keeps
// billing on the same card. A reissue under another brand voids the card's
// agreements: the member is asked to act, the renewal is not sent, and the
// customer's verification of the card anchors the agreement renewals name.
func TestNMIAccountUpdaterBrandChangeNeedsTheCustomer(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	vault := w.vaultOf(e.method)
	recurring, _ := w.agreements(e.method)
	require.NotEmpty(t, recurring)

	w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Last4 = "1881" })
	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("automaticallyupdated", vault)))
	kept, _ := w.agreements(e.method)
	require.Equal(t, recurring, kept, "a same-brand reissue keeps its agreement")
	require.Zero(t, e.c.notificationCount("payment_method_update_required"))

	w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Brand, v.Card.Last4 = "mastercard", "5100" })
	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("automaticallyupdated", vault)))
	require.Equal(t, []string{"nmi_acu/updated", "nmi_acu/brand_changed"}, w.cardUpdates(vault))
	require.Equal(t, "mastercard", w.methodRow(e.method, "card_brand"))
	r, u := w.agreements(e.method)
	require.Equal(t, []string{"", ""}, []string{r, u}, "another brand voids the card's agreements")
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"), "the member is asked to act")

	sent := len(w.nmi.Attempts())
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), sent, "no renewal without the customer's agreement")
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status)

	e.replaceCard(mastercard)
	recurring, _ = w.agreements(e.method)
	require.NotEmpty(t, recurring, "the customer's verification anchors the agreement")
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
	require.Equal(t, []string{"merchant", "used", recurring}, credentialFields(w.nmi.LastSale()))
}

// reissue is Stripe's card updater changing a payment method's card.
func (f *stripeFake) reissue(pm, brand, last4 string, month int, fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods[pm]["card"] = obj{"brand": brand, "last4": last4, "exp_month": month, "exp_year": 2036, "fingerprint": fingerprint}
}

// Stripe's card updater reissues a member's card. The same payment method
// takes the new card. Under another brand OpenRails charges it off-session no
// more: the renewal and invoice collection wait for the customer, who is
// asked to act, and the card cannot collect invoices until they agree again.
func TestStripeCardUpdaterBrandChangeNeedsTheCustomer(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	// The customer is billed in arrears, on this card.
	_, err := w.client[embedded].UpdateCustomerSettings(t.Context(), []billing.UpdateCustomerSettingsParams{{CustomerID: e.c.cid(), CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}}})
	require.NoError(t, err)
	e.c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": e.method, "currency": "USD"})
	pm := w.methodRow(e.method, "rail_method_ref")
	recurring, _ := w.agreements(e.method)
	require.NotEmpty(t, recurring)
	reissued := func(brand, last4, fingerprint string) {
		t.Helper()
		w.stripe.reissue(pm, brand, last4, 3, fingerprint)
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("payment_method.automatically_updated", obj{"object": "payment_method", "id": pm, "customer": w.stripe.customerOf(pm)})))
		w.settle()
	}

	reissued("visa", "1881", "fp_reissued")
	require.Equal(t, []string{"visa", "1881", "3", "2036", "fp_reissued"}, []string{w.methodRow(e.method, "card_brand"), w.methodRow(e.method, "card_last4"),
		w.methodRow(e.method, "card_exp_month::text"), w.methodRow(e.method, "card_exp_year::text"), w.methodRow(e.method, "fingerprint")}, "the same method takes Stripe's card")
	kept, _ := w.agreements(e.method)
	require.Equal(t, recurring, kept, "a same-brand reissue keeps its agreement")
	require.Equal(t, []string{"stripe_card_updater/updated"}, w.methodUpdates(e.method))

	reissued("mastercard", "5100", "fp_mastercard")
	require.Equal(t, "mastercard", w.methodRow(e.method, "card_brand"))
	r, u := w.agreements(e.method)
	require.Equal(t, []string{"", ""}, []string{r, u}, "another brand voids the card's agreements")
	require.Equal(t, []string{"stripe_card_updater/updated", "stripe_card_updater/brand_changed"}, w.methodUpdates(e.method))
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"), "the member is asked to act")

	sent := e.providerAttempts()
	invoice := w.arrearsInvoice(e.c)
	w.collectInvoices()
	require.Equal(t, sent, e.providerAttempts())
	require.Equal(t, agreementRequired, *w.invoice(invoice).LastCollectionFailureCode, "invoice collection waits for the customer")
	require.Empty(t, w.stripe.unexpected())
	status, body := e.c.call(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": e.method, "currency": "USD"})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)

	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Equal(t, sent, e.providerAttempts(), "no off-session renewal without the customer's agreement")
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status)
}
