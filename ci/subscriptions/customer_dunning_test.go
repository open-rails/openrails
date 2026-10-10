//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
)

type delinquencyPass struct{}

func (delinquencyPass) Kind() string { return "openrails.delinquency" }

func (w *world) runPass(args river.JobArgs) {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), args, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

// The customer read summarizes a customer's money: its settings, and per
// currency its balance and the card that pays its invoices.
func TestCustomerRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[remote]
	debtor, saver := w.newCustomer(), w.newCustomer()
	const owed = int64(50_000_000)
	limit := billing.CreditLimit{Currency: "USD", Amount: 2 * owed}
	_, err := client.UpdateCustomer(ctx, debtor.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{limit}})
	require.NoError(t, err)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: debtor.cid(), Invoker: debtor.id, Currency: "USD", EventType: "customer-read", Amount: owed, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	_, err = createCreditGrant(ctx, client, saver.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 5_000_000, Source: "e2e", SourceID: uuid.NewString()})
	require.NoError(t, err)
	card := saver.saveCard("stripe", visa)
	saver.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": card})
	cardID, err := billing.ParsePaymentMethodID(card)
	require.NoError(t, err)

	for _, tp := range []topology{embedded, remote} {
		got, err := w.client[tp].GetCustomer(ctx, debtor.cid())
		require.NoError(t, err)
		require.Equal(t, debtor.cid(), got.ID)
		require.Equal(t, []billing.CreditLimit{limit}, got.Settings.CreditLimits)
		require.Equal(t, []billing.Balance{{CustomerID: debtor.cid(), Currency: "USD", BillingMode: billing.BillingModeArrears, OwedAmount: owed}}, got.Balances)
		require.Empty(t, got.DefaultPaymentMethods)

		got, err = w.client[tp].GetCustomer(ctx, saver.cid())
		require.NoError(t, err)
		require.Equal(t, []billing.Balance{{CustomerID: saver.cid(), Currency: "USD", BillingMode: billing.BillingModePrepaid, BalanceAmount: 5_000_000, AvailableAmount: 5_000_000}}, got.Balances)
		require.Equal(t, []billing.DefaultPaymentMethod{{Currency: "USD", PaymentMethodID: cardID}}, got.DefaultPaymentMethods)

		page, err := w.client[tp].ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{saver.cid()}})
		require.NoError(t, err)
		require.Equal(t, []billing.Customer{*got}, page.Items, "the list and the read answer one object")
	}

	// The customer's own summary is the same money.
	me := saver.must(http.MethodGet, "", "", nil)
	require.Equal(t, saver.id, me["id"])
	require.Equal(t, "5000000", me["balances"].([]any)[0].(map[string]any)["balance_amount"])
	require.Equal(t, card, me["default_payment_methods"].([]any)[0].(map[string]any)["payment_method_id"])
	require.EqualValues(t, 0, me["unread_notifications"])
}

// An unpaid invoice is overdue from its due date until it is paid, voided or
// written off. Past the merchant's grace it is delinquent: the customer's new
// usage in its currency is refused.
func TestOverdueInvoices(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[remote]
	debtor := w.newCustomer()
	const owed = int64(50_000_000)
	_, err := client.UpdateCustomer(ctx, debtor.cid(), billing.UpdateCustomerParams{
		CreditLimits:   []billing.CreditLimit{{Currency: "USD", Amount: 2 * owed}},
		InvoiceProfile: catalog.Value(billing.InvoiceProfile{CollectionMethod: billing.CollectSendInvoice, NetTermsDays: 30}),
	})
	require.NoError(t, err)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: debtor.cid(), Invoker: debtor.id, Currency: "USD", EventType: "overdue", Amount: owed, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	w.advance(time.Minute)
	w.runPass(invoicePass{})
	invoices, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: debtor.cid()})
	require.NoError(t, err)
	require.Len(t, invoices.Items, 1)
	invoice := invoices.Items[0]
	require.Equal(t, billing.InvoiceOpen, invoice.Status)
	require.NotNil(t, invoice.DueAt)
	overdue := func(tp topology) []billing.Invoice {
		page, err := w.client[tp].ListInvoices(ctx, billing.InvoiceListParams{Overdue: true})
		require.NoError(t, err)
		return page.Items
	}
	require.Empty(t, overdue(remote), "not overdue before its due date")

	// In grace it is overdue and still open; usage is still admitted.
	w.advanceTo(invoice.DueAt.Add(time.Hour))
	w.runPass(delinquencyPass{})
	late := overdue(remote)
	require.Len(t, late, 1)
	require.Equal(t, invoice.ID, late[0].ID)
	require.Equal(t, billing.InvoiceOpen, late[0].Status, "overdue is a filter, not a status")
	require.False(t, late[0].Delinquent, "inside grace")

	// Past grace it is delinquent, and the customer's new usage is refused.
	w.advanceTo(invoice.DueAt.Add(15 * day))
	w.runPass(delinquencyPass{})
	for _, tp := range []topology{embedded, remote} {
		late = overdue(tp)
		require.Len(t, late, 1)
		require.True(t, late[0].Delinquent)
		read, err := w.client[tp].GetInvoice(ctx, invoice.ID)
		require.NoError(t, err)
		require.True(t, read.Delinquent)
	}
	expires := w.clock.Now().Add(time.Hour)
	verdicts, err := client.Admit(ctx, []billing.AdmitParams{{CustomerID: debtor.cid(), Invoker: debtor.id, InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 1, RequestID: uuid.NewString(), ExpiresAt: &expires}})
	require.NoError(t, err)
	require.False(t, verdicts[0].Allowed())
	require.Equal(t, "delinquent_unpaid_invoice", *verdicts[0].Admission.DenyCode)
	status, body := w.staffJSON(http.MethodGet, "/v1/admin/invoices?overdue=perhaps", nil)
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	status, body = w.staffJSON(http.MethodGet, "/v1/admin/invoices?status=past_due", nil)
	require.Equal(t, http.StatusBadRequest, status, "past_due is not an invoice status: %v", body)

	// Paid, it is no longer overdue.
	_, err = client.CreatePayment(ctx, billing.CreatePaymentParams{InvoiceID: &invoice.ID, Amount: owed, TransactionID: "wire-1"})
	require.NoError(t, err)
	paid, err := client.GetInvoice(ctx, invoice.ID)
	require.NoError(t, err)
	require.False(t, paid.Delinquent)
	require.Empty(t, overdue(remote))
}

// A declined renewal's dunning counts down the policy the case opened under:
// each retry spends one, the final retry is fixed by the schedule, and the
// case closes when the retries give up.
func TestSubscriptionDunningCountsDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	policy := &billing.DunningPolicy{Tiers: []billing.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{24, 48}}}}
	require.NoError(t, w.applySettings(t.Context(), billing.MerchantSettings{DunningPolicy: policy}))
	e := enroll(t, w, "nmi", embedded)
	require.Nil(t, w.subscription(embedded, e.sub).Dunning, "no case, no dunning")
	e.refreshBeforePeriodEnd()
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	first := w.clock.Now()
	w.runRenewals()

	reason := billing.DeclineInsufficientFunds
	for attempts := 1; attempts <= 2; attempts++ {
		var next time.Time
		for _, tp := range []topology{embedded, remote} {
			sub := w.subscription(tp, e.sub)
			require.Equal(t, billing.SubscriptionPastDue, sub.Status)
			d := sub.Dunning
			require.NotNil(t, d)
			require.Equal(t, attempts, d.Attempts)
			require.NotNil(t, d.RetriesLeft)
			require.Equal(t, 3-attempts, *d.RetriesLeft)
			require.NotNil(t, d.NextRetryAt)
			require.Equal(t, time.Duration(attempts)*24*time.Hour, d.NextRetryAt.Sub(first).Round(time.Hour))
			require.NotNil(t, d.FinalRetryAt)
			require.Equal(t, time.Duration(2-attempts)*24*time.Hour, d.FinalRetryAt.Sub(*d.NextRetryAt), "the final retry is the policy's last")
			require.False(t, d.WaitingForNewCard)
			require.Equal(t, &reason, d.LastFailureReason)
			next = *d.NextRetryAt
		}
		page, err := w.client[remote].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Status: billing.SubscriptionPastDue})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, attempts, page.Items[0].Dunning.Attempts)
		inDunning, err := w.client[remote].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Dunning: true})
		require.NoError(t, err)
		require.Equal(t, page.Items, inDunning.Items)
		own := e.c.must(http.MethodGet, "/subscriptions/"+e.sub.String(), "", nil)["dunning"].(map[string]any)
		require.EqualValues(t, 3-attempts, own["retries_left"], "the customer reads the same case")
		w.advanceHealthyTo(next.Add(time.Second))
		w.runRenewals()
	}
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionCanceled, sub.Status, "the retries gave up")
	require.Nil(t, sub.Dunning)
	page, err := w.client[remote].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Status: billing.SubscriptionPastDue})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	page, err = w.client[remote].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Dunning: true})
	require.NoError(t, err)
	require.Empty(t, page.Items, "a given-up case is out of dunning")
}

// A card the issuer refuses for good waits for a new card: nothing is
// retried, and the case ends at its deadline unless one arrives.
func TestAwaitingMethodDunning(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.setDecline(visa.Last4, "expired_card", "223")
	e.toFreshPeriodEnd()
	w.runRenewals()
	window, err := collection.Window(monthHours)
	require.NoError(t, err)
	most, err := collection.MaxFailures(monthHours)
	require.NoError(t, err)

	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionAwaitingMethod, sub.Status)
	d := sub.Dunning
	require.NotNil(t, d)
	require.True(t, d.WaitingForNewCard)
	require.Nil(t, d.NextRetryAt, "nothing is charged until a card arrives")
	require.NotNil(t, d.FinalRetryAt)
	require.True(t, d.FinalRetryAt.Equal(end.Add(window)), "the wait ends with the dunning window")
	require.NotNil(t, d.RetriesLeft)
	require.Equal(t, most, *d.RetriesLeft, "a new card gets the whole schedule")
	require.Equal(t, 1, d.Attempts)
	reason := billing.DeclineExpiredCard
	require.Equal(t, &reason, d.LastFailureReason)
	inDunning, err := w.client[remote].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Dunning: true})
	require.NoError(t, err)
	require.Len(t, inDunning.Items, 1, "waiting for a card is dunning")
	require.Equal(t, e.sub, inDunning.Items[0].ID)
	pastDue, err := w.client[remote].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Status: billing.SubscriptionPastDue})
	require.NoError(t, err)
	require.Empty(t, pastDue.Items)

	w.advanceHealthyTo(d.FinalRetryAt.Add(time.Hour))
	w.runRenewals()
	sub = w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionCanceled, sub.Status, "no card came")
	require.Nil(t, sub.Dunning)
}

// A provider that runs its own retries owns the schedule: OpenRails reads
// the case but holds no retries for it.
func TestProviderDunningHasNoSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.advanceHealthyTo(l.periodEnd().Add(time.Hour))
	require.Equal(t, http.StatusOK, w.deliver("stripe", l.providerRenewal(false)))
	w.settle()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	d := sub.Dunning
	require.NotNil(t, d)
	require.Nil(t, d.RetriesLeft)
	require.Nil(t, d.FinalRetryAt)
	require.Nil(t, d.NextRetryAt)
	require.False(t, d.WaitingForNewCard)
}
