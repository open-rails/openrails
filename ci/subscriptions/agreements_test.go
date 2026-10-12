//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/openrailstest"
)

// Each charge agreement a customer gave the merchant is its own to revoke,
// on the merchant's site as in the portal, one at a time, as Visa and
// Mastercard require: every charge's receipt says how to cancel; a
// revocation is confirmed by email; the card is never charged under it
// again; usage owed before it is paid only by the customer, present; and the
// merchant hears mandate.revoked.
func TestRevokingAgreements(t *testing.T) {
	t.Parallel()
	const billingURL = "https://host.test/account/billing"
	srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.e2e-key"})
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12, func(c *openrails.Config) {
		c.SMTP = &openrails.SMTPConfig{Host: srv.Host, Port: srv.Port, Username: "apikey", Password: "SG.e2e-key",
			From: openrails.EmailAddress{Name: "Merchant Billing", Address: "noreply@deploy.test"}}
	})
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.displayName = "Host Shop"
	w.settings = billing.MerchantSettings{Profile: &billing.MerchantProfile{BillingURL: billingURL}}
	w.start()
	ctx, client := t.Context(), w.client[remote]
	declareDynamicMeter(t, client, "agreements", "units")
	mail := func(subject string) smtptest.Message {
		t.Helper()
		w.mailSettled()
		var found *smtptest.Message
		require.Eventually(t, func() bool {
			for _, m := range srv.Messages() {
				if strings.Contains(m.Subject, subject) {
					found = &m
					return true
				}
			}
			return false
		}, 20*time.Second, 50*time.Millisecond, "an email %q", subject)
		return *found
	}
	saysHowToCancel := func(m smtptest.Message) {
		t.Helper()
		for _, body := range []string{m.Text, m.HTML} {
			require.Contains(t, body, "To cancel", m.Subject)
			require.Contains(t, body, billingURL, m.Subject)
		}
	}

	c := w.newCustomer()
	directory.Put(userinfo.User{ID: c.id, Email: "member@host.test", Username: "member"})
	price := w.membershipEvery("content:members", 9_990_000, monthHours)
	card := c.saveCard("nmi", visa)
	e := &engineCase{w: w, rail: "nmi", tp: embedded, c: c, price: price.ID.String(), ent: "content:members", method: card, started: w.clock.Now()}
	e.sub = c.subscribe(embedded, "nmi", e.price, e.ent, card)
	saysHowToCancel(mail("Welcome to"))
	e.toFreshPeriodEnd()
	w.runRenewals()
	saysHowToCancel(mail("has been renewed"))

	// Usage billed in arrears under the customer's collection agreement.
	_, err := client.UpdateCustomer(ctx, c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 200_000_000}}})
	require.NoError(t, err)
	c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": card})
	use := func(amount int64) error {
		_, err := recordUsage(ctx, client, billing.RecordUsageParams{Dimensions: map[string]int64{"units": 1}, CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "agreements",
			Amount: new(int64(amount)), Source: "test", SourceID: uuid.NewString()})
		return err
	}
	// collect runs the usage pass a minute on, so the usage so far is in it.
	collect := func() {
		t.Helper()
		w.advance(time.Minute)
		w.runPass(riverjobs.UsageOrderArgs{Collect: true})
		w.settle()
	}
	require.NoError(t, use(60_000_000))
	collect()
	saysHowToCancel(mail("Your receipt from"))

	agreements := func() map[billing.MandateKind]map[string]any {
		out := map[billing.MandateKind]map[string]any{}
		for _, raw := range c.must(http.MethodGet, "/agreements", "", nil)["data"].([]any) {
			a := raw.(map[string]any)
			out[billing.MandateKind(a["kind"].(string))] = a
		}
		return out
	}
	live := agreements()
	require.Len(t, live, 2, "the subscription's and the usage's")
	require.Equal(t, e.sub.String(), live[billing.MandateRecurring]["subscription"].(map[string]any)["id"])
	require.Equal(t, "USD", live[billing.MandateUnscheduled]["currency"])
	require.Nil(t, live[billing.MandateUnscheduled]["subscription"])
	require.Equal(t, card, live[billing.MandateUnscheduled]["payment_method_id"])
	revoked := func(kind billing.MandateKind) []billing.MandateHostEvent {
		events, err := client.ListHostEvents(ctx, billing.HostEventListParams{Type: billing.HostEventMandateRevoked})
		require.NoError(t, err)
		var out []billing.MandateHostEvent
		for _, ev := range events.Items {
			if ev.Mandate.Kind == kind {
				out = append(out, *ev.Mandate)
			}
		}
		return out
	}

	// Revoking the usage agreement: what is owed until now is an open order
	// the customer pays itself; nothing more is charged to the card; new
	// usage spends only the balance.
	require.NoError(t, use(30_000_000))
	charged := len(w.nmi.ledger(""))
	usageID := live[billing.MandateUnscheduled]["id"].(string)
	out := c.must(http.MethodPost, "/agreements/"+usageID+"/revoke", "", nil)
	require.Equal(t, "revoked", out["agreement"].(map[string]any)["status"])
	owed := out["usage_order"].(map[string]any)
	require.Equal(t, "open", owed["status"])
	require.Equal(t, "30000000", owed["total"])
	status, body := c.call(http.MethodPost, "/agreements/"+usageID+"/revoke", "", nil)
	require.Equal(t, http.StatusNotFound, status, "%v", body)
	events := revoked(billing.MandateUnscheduled)
	require.Len(t, events, 1)
	require.Equal(t, usageID, events[0].MandateID.String())
	require.Equal(t, "USD", events[0].Currency)
	require.Nil(t, events[0].SubscriptionID)
	stopped := mail("Usage charges at Host Shop stopped")
	for _, b := range []string{stopped.Text, stopped.HTML} {
		require.Contains(t, b, "30.00 USD", "what it still owes")
		require.Contains(t, b, billingURL, "where to pay it")
	}
	collect()
	require.Len(t, w.nmi.ledger(""), charged, "never charged under the revoked agreement")
	owedID, err := billing.ParseOrderID(owed["id"].(string))
	require.NoError(t, err)
	order, err := client.GetOrder(ctx, owedID)
	require.NoError(t, err)
	require.Equal(t, billing.OrderOpen, order.Status)
	paid := c.must(http.MethodPost, "/orders/"+owed["id"].(string)+"/pay", "pay-"+uuid.NewString(),
		map[string]any{"payment": map[string]any{"payment_method_id": card}, "expected_total": "30000000"})
	require.Equal(t, "complete", paid["status"], "%v", paid)
	require.Len(t, w.nmi.ledger(""), charged+1, "the customer's own payment")
	// Admission is the gate: usage, once done, is recorded and owed.
	admit := func(amount int64) *billing.Admission {
		t.Helper()
		expires := w.clock.Now().Add(time.Hour)
		verdicts, err := client.Admit(ctx, []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: c.cid(), Invoker: c.id,
			InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: amount, ExpiresAt: &expires}})
		require.NoError(t, err)
		return verdicts[0].Admission
	}
	refused := admit(10_000_000)
	require.False(t, refused.Allowed, "no credit without the agreement")
	require.Contains(t, *refused.DenyCode, "insufficient")
	_, err = createCreditGrant(ctx, client, c.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 20_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	require.True(t, admit(10_000_000).Allowed, "the balance stays the customer's to spend")
	require.False(t, admit(50_000_000).Allowed, "but no more than it")
	c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": card})
	require.True(t, admit(50_000_000).Allowed, "a new agreement: credit again")

	// Revoking the subscription's agreement cancels it at the period's end,
	// confirmed by email; the next renewal never comes.
	recurringID := live[billing.MandateRecurring]["id"].(string)
	out = c.must(http.MethodPost, "/agreements/"+recurringID+"/revoke", "", nil)
	require.Equal(t, "revoked", out["agreement"].(map[string]any)["status"])
	sub := out["subscription"].(map[string]any)
	require.Equal(t, true, sub["cancel_scheduled"])
	require.Nil(t, out["usage_order"])
	events = revoked(billing.MandateRecurring)
	require.Len(t, events, 1)
	require.Equal(t, e.sub, *events[0].SubscriptionID)
	require.Equal(t, "USD", events[0].Currency)
	end := e.periodEnd()
	confirmed := mail("has been canceled")
	for _, b := range []string{confirmed.Text, confirmed.HTML} {
		require.Contains(t, b, e.sub.UUID().String(), "it names the subscription")
		require.Contains(t, b, "No further payments")
	}
	left := agreements()
	require.Len(t, left, 1, "only the new usage agreement")
	require.Contains(t, left, billing.MandateUnscheduled)
	charged = len(w.nmi.ledger(""))
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Len(t, w.nmi.ledger(""), charged, "no renewal under the revoked agreement")
	ended := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionCanceled, ended.Status)
	require.True(t, ended.CurrentPeriodEndsAt.Equal(end))
}
