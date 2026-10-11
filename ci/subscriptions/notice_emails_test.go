//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/openrailstest"
)

// noticeEmail is the email to address whose subject holds subject, after a
// sweep delivers what is pending.
func (w *world) noticeEmail(mail *mailbox, address, subject string) openrails.Email {
	t := w.t
	t.Helper()
	w.runPass(emailSweep{})
	var sent openrails.Email
	require.Eventually(t, func() bool {
		for _, e := range mail.all() {
			if e.To == address && strings.Contains(e.Subject, subject) {
				sent = e
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "an email to %s about %q; sent %+v", address, subject, mail.all())
	return sent
}

// Every billing notice a customer is sent reaches them by email, made by its
// own path: a usage order billed, overdue, declined, its collection stopped;
// the account delinquent and settled; a scheduled price or plan change. The
// notices about a recurring agreement say how to cancel it.
func TestCustomerNoticesAreEmailed(t *testing.T) {
	t.Parallel()
	const billingPage = "https://shop.e2e.test/billing"
	directory, mail := &openrailstest.UserInfo{}, &mailbox{}
	w := prepareWorld(t, 12)
	w.displayName = "Notice Shop"
	w.settings = billing.MerchantSettings{Profile: &billing.MerchantProfile{BillingURL: billingPage}}
	w.deps = func(d *openrails.Deps) { d.UserInfo, d.Email = directory, mail }
	w.start()
	ctx, client := t.Context(), w.client[remote]
	const owed = int64(50_000_000) // the default collection threshold
	member := func(c *customer) string {
		email := "member-" + c.id[:8] + "@host.test"
		directory.Put(userinfo.User{ID: c.id, Email: email, Username: "member"})
		return email
	}
	bill := func(c *customer, profile *billing.InvoiceProfile) billing.Order {
		t.Helper()
		params := billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 2 * owed}}}
		if profile != nil {
			params.InvoiceProfile = catalog.Value(*profile)
		}
		_, err := client.UpdateCustomer(ctx, c.cid(), params)
		require.NoError(t, err)
		_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "notice", Amount: owed, Source: "test", SourceID: uuid.NewString()})
		require.NoError(t, err)
		w.advance(time.Minute)
		w.runPass(invoicePass{})
		orders, err := client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		return orders.Items[0]
	}
	says := func(e openrails.Email, lines ...string) {
		t.Helper()
		require.Equal(t, "Notice Shop", e.From.Name)
		for _, body := range []string{e.Text, e.HTML} {
			require.Contains(t, body, "Hi member,")
			for _, line := range lines {
				require.Contains(t, body, line, e.Subject)
			}
		}
	}
	cancels := func(e openrails.Email, what string) {
		t.Helper()
		require.Contains(t, e.Text, "To cancel "+what+", and with it your agreement to these charges, visit "+billingPage+".")
		require.Contains(t, e.HTML, `visit <a href="`+billingPage+`">`)
	}

	t.Run("an invoiced order: due, overdue, delinquent, settled", func(t *testing.T) {
		debtor := w.newCustomer()
		to := member(debtor)
		order := bill(debtor, &billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice, NetTermsDays: 30})
		due := w.noticeEmail(mail, to, "bill: 50.00 USD due")
		says(due, "50.00 USD", order.Usage.DueAt.UTC().Format("January 2, 2006"), "Please pay it by then")
		require.NotContains(t, due.Text, "To cancel", "an invoiced order charges no card")

		w.advanceTo(order.Usage.DueAt.Add(time.Hour))
		w.runPass(riverjobs.UsageOrderArgs{Collect: true})
		says(w.noticeEmail(mail, to, "bill is overdue"), "50.00 USD", "still unpaid")
		require.Contains(t, debtor.noticeKinds(), "order_overdue", "the customer reads the overdue notice")

		w.advanceTo(order.Usage.DueAt.Add(15 * day))
		w.runPass(delinquencyPass{})
		says(w.noticeEmail(mail, to, "account is past due"), "50.00 USD", "new usage is paused")

		_, err := client.CreatePayment(ctx, billing.CreatePaymentParams{OrderID: order.ID, Amount: owed, TransactionID: "wire-" + uuid.NewString()})
		require.NoError(t, err)
		w.runPass(delinquencyPass{})
		says(w.noticeEmail(mail, to, "account is in good standing"), "no longer paused")
	})

	t.Run("orders charged to the card on file: due, declined, stopped", func(t *testing.T) {
		w.refreshProviders()
		automatic := func(last4 string) (*customer, string, billing.Order) {
			c := w.newCustomer()
			to := member(c)
			// The credit line first: naming a default card opens a prepaid account.
			_, err := client.UpdateCustomer(ctx, c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 2 * owed}}})
			require.NoError(t, err)
			method := c.saveCard("nmi", card{Brand: "visa", Last4: last4})
			c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
			return c, to, bill(c, nil)
		}
		_, retried, _ := automatic("1202")
		_, expired, _ := automatic("1223")
		_, stolen, _ := automatic("1252")
		for _, to := range []string{retried, expired, stolen} {
			due := w.noticeEmail(mail, to, "bill: 50.00 USD due")
			says(due, "50.00 USD", "card on file")
			cancels(due, "charges for your usage")
		}

		w.nmi.SetDecline("1202", "202")
		w.nmi.SetDecline("1223", "223")
		w.nmi.SetDecline("1252", "252")
		w.refreshProviders()
		w.settleCollectionScans()
		w.settle()
		says(w.noticeEmail(mail, retried, "couldn't charge your card"), "50.00 USD", "We'll try again on")
		says(w.noticeEmail(mail, expired, "update your card"), "50.00 USD", "stopped charging it")
		says(w.noticeEmail(mail, stolen, "stopped charging your card"), "50.00 USD", "can no longer be charged", "still owed")
		for _, to := range []string{retried, expired, stolen} {
			for _, e := range mail.all() {
				if e.To == to {
					require.NotContains(t, e.Subject, "renew", "a usage order's notice is not a subscription's")
				}
			}
		}
	})

	t.Run("a scheduled price change and plan change", func(t *testing.T) {
		repriced := enroll(t, w, "stripe", embedded)
		to := member(repriced.c)
		old, err := w.client[embedded].GetPrice(ctx, pid(repriced.price), billing.GetPriceParams{})
		require.NoError(t, err)
		hours := monthHours
		cheaper, err := w.client[embedded].CreatePrice(ctx, billing.CreatePriceParams{ProductID: old.ProductID, Key: "cheaper-" + uuid.NewString()[:8],
			UnitAmount: 7_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		_, err = w.client[embedded].CreatePriceMigration(ctx, billing.CreatePriceMigrationParams{FromPriceID: old.ID, ToPriceID: cheaper.ID})
		require.NoError(t, err)
		notice := w.noticeEmail(mail, to, "Membership price is changing")
		says(notice, "9.99 USD", "7.99 USD", "cancel before")
		cancels(notice, "this subscription")

		moved := enroll(t, w, "stripe", embedded)
		to = member(moved.c)
		group := benefitGroup(moved.ent)
		plan, err := w.client[embedded].CreateProduct(ctx, billing.CreateProductParams{Key: "plan-b-" + uuid.NewString()[:8], DisplayName: "Plan B",
			TierGroup: &group, Entitlements: []string{"content:plan-b"}})
		require.NoError(t, err)
		target, err := w.client[embedded].CreatePrice(ctx, billing.CreatePriceParams{ProductID: plan.ID, Key: plan.Key + "-usd", UnitAmount: 7_990_000,
			Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		_, err = w.client[embedded].CreatePriceMigration(ctx, billing.CreatePriceMigrationParams{FromPriceID: pid(moved.price), ToPriceID: target.ID})
		require.NoError(t, err)
		notice = w.noticeEmail(mail, to, "Membership plan is changing")
		says(notice, "moves to Plan B", "Plan B, 7.99 USD", "Membership, 9.99 USD")
		cancels(notice, "this subscription")
	})
}
