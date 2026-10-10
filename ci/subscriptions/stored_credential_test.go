//go:build e2e && integration

package subscriptions_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// agreements is the lineage of a stored card's active recurring mandate and
// of its active unscheduled one (the collection mandate, else its card-on-file
// consent): "" when the card has none that can be charged under.
func (w *world) agreements(method string) (recurring, unscheduled string) {
	w.t.Helper()
	lineage := func(kinds string) string {
		var ref *string
		err := w.pool.QueryRow(w.t.Context(), w.q(`SELECT initial_transaction_id FROM billing.mandates
			WHERE payment_method_id = $1::uuid AND status = 'active' AND kind = ANY (string_to_array($2, ','))
			ORDER BY array_position(string_to_array($2, ','), kind), created_at LIMIT 1`), strings.TrimPrefix(method, "pm_"), kinds).Scan(&ref)
		if errors.Is(err, pgx.ErrNoRows) || ref == nil {
			return ""
		}
		require.NoError(w.t, err)
		return *ref
	}
	return lineage("recurring"), lineage("unscheduled,card_on_file")
}

// methodVersions is a stored card's versions, oldest first, as source/kind.
func (w *world) methodVersions(method string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT source || '/' || kind FROM billing.payment_method_versions
		WHERE payment_method_id = $1 ORDER BY created_at, id`), strings.TrimPrefix(method, "pm_"))
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

// arrearsInvoice runs up 50 USD of usage for c and invoices it. A scheduled
// collection may already have collected it under the customer's mandate.
func (w *world) arrearsInvoice(c *customer) billing.InvoiceID {
	w.t.Helper()
	ctx, client := w.t.Context(), w.client[embedded]
	const owed = 50_000_000 // the default invoice threshold
	before, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: c.cid()})
	require.NoError(w.t, err)
	_, err = client.UpdateCustomer(ctx, c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: owed}}})
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
			require.EqualValues(w.t, owed, invoice.TotalAmount)
			return invoice.ID
		}
	}
	w.t.Fatal("no invoice for the usage")
	return billing.InvoiceID{}
}

// collectInvoices runs the scheduled invoice collection once, and waits for
// any scan a refresh started alongside it.
func (w *world) collectInvoices() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), collectInvoicePass{Collect: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
	w.settleCollectionScans()
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

// collectionMandate is c's active unscheduled mandate for currency.
func (w *world) collectionMandate(c *customer, currency string) billing.Mandate {
	w.t.Helper()
	var live []billing.Mandate
	for _, m := range w.mandates(embedded, c.cid()) {
		if m.Kind == billing.MandateUnscheduled && str(m.Currency) == currency && m.Status == billing.MandateActive {
			live = append(live, m)
		}
	}
	require.Len(w.t, live, 1, "one active collection mandate per currency")
	return live[0]
}

// invoiceAttempt is c's invoice attempt for a provider transaction.
func (w *world) invoiceAttempt(c *customer, transaction string) billing.PaymentAttempt {
	w.t.Helper()
	page, err := w.client[embedded].ListPaymentAttempts(w.t.Context(), billing.PaymentAttemptListParams{CustomerID: c.cid(), Kind: []string{"invoice"}})
	require.NoError(w.t, err)
	for _, a := range page.Items {
		if a.TransactionID == transaction {
			return a
		}
	}
	w.t.Fatalf("no invoice attempt for %s", transaction)
	return billing.PaymentAttempt{}
}

// A card replaced in place ends the replaced card's mandates: its
// verification declares a recurring agreement, which no subscription of this
// customer takes. Invoice collection stops for the customer rather than cite
// the old card's collection mandate; the customer's own payment anchors the
// new card's, and later collections cite it.
func TestReplacedCardDropsUnscheduledAgreement(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	saved := w.nmi.Validations(w.vaultOf(method))[0]
	first := w.arrearsInvoice(c)
	w.refreshProviders()
	w.settleCollectionScans()
	c.must(http.MethodPost, "/invoices/"+first.String()+"/pay-now", "pay-"+uuid.NewString(), map[string]any{"payment_method_id": method})
	w.settle()
	_, old := w.agreements(method)
	require.Equal(t, saved.TransactionID, old, "saving the card stored it for reuse")
	require.Equal(t, []string{"customer", "used", old}, credentialFields(w.nmi.LastSale()), "the customer's payment uses it")
	c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
	replaced := w.collectionMandate(c, "USD")
	require.Equal(t, old, str(replaced.InitialTransactionID), "the collection mandate cites the card's lineage")

	c.must(http.MethodPut, "/payment-methods/"+method, "", map[string]any{"payment_token": w.nmi.Tokenize(mastercard)})
	w.settle()
	recurring, unscheduled := w.agreements(method)
	require.Empty(t, unscheduled, "the replaced card's collection mandate goes with it")
	require.Empty(t, recurring, "no subscription takes the replacement's recurring verification")
	ended := w.mandate(embedded, c.cid(), replaced.ID)
	require.Equal(t, billing.MandateEnded, ended.Status)
	require.Equal(t, billing.MandateEndReplaced, *ended.EndReason)
	verified := w.nmi.Validations(w.vaultOf(method))
	require.Equal(t, "recurring", verified[len(verified)-1].Form.Get("billing_method"))

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
	collection := w.collectionMandate(c, "USD")
	require.Equal(t, unscheduled, str(collection.InitialTransactionID), "the customer's payment anchors the new collection mandate")

	third := w.arrearsInvoice(c)
	w.refreshProviders()
	w.collectInvoices()
	sale = w.nmi.LastSale()
	require.Equal(t, []string{"merchant", "used", unscheduled}, credentialFields(sale), "collection cites the new card's mandate")
	require.Equal(t, billing.InvoicePaid, w.invoice(third).Status)
	require.NotEqual(t, old, sale.Initial)
	attempt := w.invoiceAttempt(c, sale.TransactionID)
	require.NotNil(t, attempt.MandateID)
	require.Equal(t, collection.ID, *attempt.MandateID)
	require.Equal(t, unscheduled, attempt.SentInitialTransactionID)
}

// NMI's Account Updater reissues a member's card. A same-brand reissue keeps
// billing on the same card. A reissue under another brand holds the card's
// agreements for reconsent: the member is asked to act, the renewal is not
// sent, and the customer's verification of the same card anchors the
// agreement renewals name (#1168).
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
	require.Equal(t, []string{"customer_save/saved", "nmi_acu/updated", "nmi_acu/brand_changed"}, w.cardVersions(vault))
	require.Equal(t, "mastercard", w.methodRow(e.method, "card_brand"))
	r, u := w.agreements(e.method)
	require.Equal(t, []string{"", ""}, []string{r, u}, "another brand voids the card's agreements")
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"), "the member is asked to act")

	sent := len(w.nmi.Attempts())
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), sent, "no renewal without the customer's agreement")
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status)

	require.Equal(t, []string{"card_on_file/requires_reconsent", "recurring/requires_reconsent"}, w.mandateStates(e.method))
	validations := len(w.nmi.Validations(vault))
	verified := e.c.must(http.MethodPost, "/payment-methods/"+e.method+"/verify", uuid.NewString(), nil)
	require.Len(t, w.nmi.Validations(vault), validations+2, "one verification declaring recurring, one for reuse")
	require.Equal(t, true, verified["reusable"])
	recurring, _ = w.agreements(e.method)
	require.NotEmpty(t, recurring, "the customer's verification anchors the agreement")
	require.Equal(t, []string{"card_on_file/ended/brand_changed", "recurring/ended/brand_changed", "card_on_file/active", "recurring/active"}, w.mandateStates(e.method))
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
	require.Equal(t, []string{"merchant", "used", recurring}, credentialFields(w.nmi.LastSale()))
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
	_, err := w.client[embedded].UpdateCustomer(t.Context(), e.c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}})
	require.NoError(t, err)
	e.c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": e.method})
	pm := w.methodRow(e.method, "rail_method_ref")
	recurring, _ := w.agreements(e.method)
	require.NotEmpty(t, recurring)
	reissued := func(brand, last4, fingerprint string) {
		t.Helper()
		w.stripe.ReissueCard(pm, brand, last4, 3, fingerprint)
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("payment_method.automatically_updated", obj{"object": "payment_method", "id": pm, "customer": w.stripe.CustomerOf(pm)})))
		w.settle()
	}

	reissued("visa", "1881", "fp_reissued")
	require.Equal(t, []string{"visa", "1881", "3", "2036", "fp_reissued"}, []string{w.methodRow(e.method, "card_brand"), w.methodRow(e.method, "card_last4"),
		w.methodRow(e.method, "card_exp_month::text"), w.methodRow(e.method, "card_exp_year::text"), w.methodRow(e.method, "fingerprint")}, "the same method takes Stripe's card")
	kept, _ := w.agreements(e.method)
	require.Equal(t, recurring, kept, "a same-brand reissue keeps its agreement")
	require.Equal(t, []string{"customer_save/saved", "stripe_updater/updated"}, w.methodVersions(e.method))

	reissued("mastercard", "5100", "fp_mastercard")
	require.Equal(t, "mastercard", w.methodRow(e.method, "card_brand"))
	r, u := w.agreements(e.method)
	require.Equal(t, []string{"", ""}, []string{r, u}, "another brand voids the card's agreements")
	require.Equal(t, []string{"customer_save/saved", "stripe_updater/updated", "stripe_updater/brand_changed"}, w.methodVersions(e.method))
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"), "the member is asked to act")

	sent := e.providerAttempts()
	invoice := w.arrearsInvoice(e.c)
	w.collectInvoices()
	require.Equal(t, sent, e.providerAttempts())
	require.Equal(t, agreementRequired, *w.invoice(invoice).LastCollectionFailureCode, "invoice collection waits for the customer")
	require.Empty(t, w.stripe.Unexpected())
	status, body := e.c.call(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": e.method})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)

	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Equal(t, sent, e.providerAttempts(), "no off-session renewal without the customer's agreement")
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status)

	// The customer verifies the same card: a SetupIntent confirmed with them
	// present is the new agreement, and the renewal is collected.
	e.c.must(http.MethodPost, "/payment-methods/"+e.method+"/verify", uuid.NewString(), nil)
	setups := w.stripe.Mutations("/v1/setup_intents")
	require.Equal(t, "true", setups[len(setups)-1].Form.Get("confirm"))
	require.Equal(t, pm, setups[len(setups)-1].Form.Get("payment_method"))
	recurring, _ = w.agreements(e.method)
	require.True(t, strings.HasPrefix(recurring, "seti_"), "the agreement cites the customer's setup, got %q", recurring)
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
	require.Empty(t, w.stripe.Unexpected())
}
