//go:build greenfield && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// CCBill is a retained legacy cohort: OpenRails never enrolls a new CCBill
// subscription, but imported ones keep receiving CCBill's background posts.
// CCBill signs nothing; its authentication is the documented source range
// (seen through the site's trusted proxy) plus the armed account identity.

const (
	ccbillSourceIP = "64.38.212.17"
	ccbillFormName = "basic-monthly"
	ccbillFlexID   = "681cb38f-afb9-4665-931f-2b896072178a"
	ccbillRBO      = "0000007498"
)

type ccbillMember struct {
	*legacy
	paidThrough time.Time
	saleTxn     string
}

// importCCBill lands one provider-owned CCBill membership through
// ImportBilling, as the legacy migration does, paid through 20 days from now.
func importCCBill(t *testing.T, w *world) *ccbillMember {
	t.Helper()
	l := &legacy{w: w, rail: "ccbill", tp: embedded, ent: "content:ccbill", c: w.newCustomer()}
	client := w.client[embedded]
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "ccbill-" + uuid.NewString()[:8], DisplayName: "CCBill membership", EntitlementsSpec: map[string]*int{l.ent: nil}})
	require.NoError(t, err)
	hours := monthHours
	l.price, err = client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"ccbill": {"form_name": ccbillFormName, "flex_id": ccbillFlexID, "recurring_billing_option_id": ccbillRBO}}})
	require.NoError(t, err)

	start := w.clock.Now().Add(-10 * day)
	end := start.Add(monthHours * time.Hour)
	customerID, err := openrails.ParseCustomerID(l.c.id)
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(l.price.ID)
	require.NoError(t, err)
	l.railSub = ccbillNumericID()
	m := &ccbillMember{legacy: l, paidThrough: end, saleTxn: ccbillNumericID()}
	result, err := client.ImportBilling(t.Context(), openrails.DeclaredBilling{
		AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "ccbill"},
		Customers:     []openrails.DeclaredCustomer{{Customer: customerID}},
		Subscriptions: []openrails.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: customerID, Price: priceID, Rail: "ccbill", RailSubscriptionID: l.railSub, StartedAt: start, PaidThrough: &end}},
		Transactions:  []openrails.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: m.saleTxn, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := client.ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: l.c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 1)
	l.sub = subs.Data[0].ID
	require.Equal(t, "active", subs.Data[0].Status)
	require.Equal(t, "ccbill", subs.Data[0].Rail)
	require.Equal(t, l.railSub, subs.Data[0].RailSubscriptionID)
	w.converge()
	require.True(t, l.c.entitled(l.ent))
	return m
}

func ccbillNumericID() string { return fmt.Sprintf("%019d", rand.Int64N(1e18)) }

func ccbillDate(t time.Time) string      { return t.UTC().Format("2006-01-02") }
func ccbillTimestamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// postCCBill delivers one CCBill background post as CCBill sends it: a
// form-encoded body with eventType/eventGroupType in the query string.
func (w *world) postCCBill(eventType, sourceIP string, fields map[string]string) (int, map[string]any) {
	w.t.Helper()
	form := url.Values{"clientAccnum": {"945280"}, "clientSubacc": {"0000"}, "timestamp": {ccbillTimestamp(w.clock.Now())}}
	for k, v := range fields {
		form.Set(k, v)
	}
	query := url.Values{"eventType": {eventType}, "eventGroupType": {"Subscription"}}
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/webhooks/ccbill/"+ccbillAcct+"?"+query.Encode(), strings.NewReader(form.Encode()))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sourceIP != "" {
		req.Header.Set("X-Forwarded-For", sourceIP)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	w.t.Logf("ccbill %s -> %d %s", eventType, res.StatusCode, raw)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	w.settle()
	return res.StatusCode, out
}

func (w *world) deliverCCBill(eventType string, fields map[string]string) map[string]any {
	w.t.Helper()
	status, body := w.postCCBill(eventType, ccbillSourceIP, fields)
	require.Equal(w.t, http.StatusOK, status, "%v", body)
	return body
}

func (m *ccbillMember) renewal(txn string, next time.Time) map[string]string {
	return map[string]string{
		"subscriptionId": m.railSub, "transactionId": txn,
		"billedAmount": "9.99", "billedCurrency": "USD", "billedCurrencyCode": "840",
		"accountingAmount": "9.99", "accountingCurrency": "USD", "accountingCurrencyCode": "840",
		"renewalDate": ccbillDate(m.paidThrough), "nextRenewalDate": ccbillDate(next),
		"cardType": "VISA", "paymentType": "CREDIT", "last4": "1111", "expDate": "1235",
	}
}

func (m *ccbillMember) reversal(txn, amount string) map[string]string {
	return map[string]string{
		"subscriptionId": m.railSub, "transactionId": txn, "amount": amount,
		"currency": "USD", "currencyCode": "840", "accountingAmount": amount,
		"accountingCurrency": "USD", "accountingCurrencyCode": "840",
		"reason": "customer request", "cardType": "VISA", "paymentType": "CREDIT", "last4": "1111", "expDate": "1235",
	}
}

func endOfDay(t time.Time) time.Time {
	d := t.UTC()
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
}

func (m *ccbillMember) payment(txn string) *openrails.Payment {
	for _, p := range m.w.payments(embedded, m.c.id) {
		if p.TransactionID == txn {
			return &p
		}
	}
	return nil
}

// requireReversal finds the ledger reversal of the imported sale.
func (m *ccbillMember) requireReversal(txn, kind string) {
	m.w.t.Helper()
	sale, reversal := m.payment(m.saleTxn), m.payment(txn)
	require.NotNil(m.w.t, sale)
	require.NotNil(m.w.t, reversal, "the %s is on the ledger: %+v", kind, m.w.payments(embedded, m.c.id))
	require.EqualValues(m.w.t, -9_990_000, reversal.Amount)
	require.NotNil(m.w.t, reversal.RefundedPaymentID)
	require.Equal(m.w.t, sale.ID, *reversal.RefundedPaymentID, "the %s reverses the imported sale", kind)
}

func TestCCBillRetainedCohortWebhooks(t *testing.T) {
	t.Run("renewal success extends the paid window once", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		w.advance(20 * day)
		next := m.paidThrough.Add(monthHours * time.Hour)
		txn := ccbillNumericID()
		w.deliverCCBill("RenewalSuccess", m.renewal(txn, next))

		sub := w.subscription(embedded, m.sub)
		require.Equal(t, "active", sub.Status)
		require.NotNil(t, sub.CurrentPeriodEndsAt)
		require.True(t, sub.CurrentPeriodEndsAt.Equal(endOfDay(next)), "paid through %v", sub.CurrentPeriodEndsAt)
		renewal := m.payment(txn)
		require.NotNil(t, renewal, "the rebill is on the ledger")
		require.Equal(t, "succeeded", renewal.Status)
		require.EqualValues(t, 9_990_000, renewal.Amount)
		require.Equal(t, "USD", renewal.Currency)

		// CCBill redelivers until it sees a 2xx; the replay changes nothing.
		before := len(w.payments(embedded, m.c.id))
		w.deliverCCBill("RenewalSuccess", m.renewal(txn, next))
		require.Len(t, w.payments(embedded, m.c.id), before)

		w.advance(15 * day)
		require.True(t, m.c.entitled(m.ent), "access runs through the renewed period")
		require.Zero(t, w.engineCharges(), "OpenRails never charges a CCBill schedule")
	})

	t.Run("renewal failure is past due with access, then CCBill's retry recovers", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		w.advance(20 * day)
		failed := ccbillNumericID()
		w.deliverCCBill("RenewalFailure", map[string]string{
			"subscriptionId": m.railSub, "transactionId": failed,
			"failureReason": "Insufficient funds", "failureCode": "BE-140",
			"renewalDate": ccbillDate(m.paidThrough), "nextRetryDate": ccbillDate(m.paidThrough.Add(2 * day)),
			"cardType": "VISA", "paymentType": "CREDIT",
		})
		sub := w.subscription(embedded, m.sub)
		require.Equal(t, "past_due", sub.Status)
		require.NotNil(t, sub.NextRetryAt)
		decline := m.payment(failed)
		require.NotNil(t, decline, "the declined rebill is recorded")
		require.Equal(t, "failed", decline.Status)
		require.True(t, m.c.entitled(m.ent), "access survives CCBill's dunning")

		w.advance(2 * day)
		next := m.paidThrough.Add(monthHours * time.Hour)
		recovered := ccbillNumericID()
		w.deliverCCBill("RenewalSuccess", m.renewal(recovered, next))
		sub = w.subscription(embedded, m.sub)
		require.Equal(t, "active", sub.Status)
		require.True(t, sub.CurrentPeriodEndsAt.Equal(endOfDay(next)), "paid through %v", sub.CurrentPeriodEndsAt)
		require.Equal(t, "succeeded", m.payment(recovered).Status)
		require.Zero(t, w.engineCharges())
	})

	t.Run("cancellation keeps paid access until the period ends", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		w.deliverCCBill("Cancellation", map[string]string{"subscriptionId": m.railSub, "reason": "Too expensive", "source": "webAdmin"})
		sub := w.subscription(embedded, m.sub)
		require.Equal(t, "cancelled", sub.Status)
		require.NotNil(t, sub.CancelledAt)
		require.True(t, m.c.entitled(m.ent), "a cancelled CCBill member keeps the paid period")

		// A redelivered cancellation is a no-op on the terminal row.
		w.deliverCCBill("Cancellation", map[string]string{"subscriptionId": m.railSub, "reason": "Too expensive", "source": "webAdmin"})
		require.Equal(t, sub.CancelledAt.UTC(), w.subscription(embedded, m.sub).CancelledAt.UTC())

		w.advance(21 * day)
		require.False(t, m.c.entitled(m.ent), "access ends with the paid period")
	})

	t.Run("failed-rebill cancellation revokes access at once", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		w.deliverCCBill("Cancellation", map[string]string{"subscriptionId": m.railSub, "reason": "Transaction Declined", "source": "failedRB"})
		sub := w.subscription(embedded, m.sub)
		require.Equal(t, "cancelled", sub.Status)
		require.False(t, m.c.entitled(m.ent))
	})

	t.Run("expiration ends a lapsed membership and ignores a paid one", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		w.deliverCCBill("Expiration", map[string]string{"subscriptionId": m.railSub})
		require.Equal(t, "active", w.subscription(embedded, m.sub).Status, "an early expiration cannot end a paid period")
		require.True(t, m.c.entitled(m.ent))

		w.advance(21 * day)
		w.deliverCCBill("Expiration", map[string]string{"subscriptionId": m.railSub})
		sub := w.subscription(embedded, m.sub)
		require.Equal(t, "cancelled", sub.Status, "expiry is terminal: never rebilled")
		require.NotNil(t, sub.CancelType)
		require.Equal(t, "expired", *sub.CancelType)
		require.False(t, m.c.entitled(m.ent))
	})

	t.Run("full refund reverses the sale and revokes access", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		refund := ccbillNumericID()
		w.deliverCCBill("Refund", m.reversal(refund, "9.99"))
		require.False(t, m.c.entitled(m.ent), "a refunded member loses access")
		require.NotEqual(t, "active", w.subscription(embedded, m.sub).Status)
		m.requireReversal("refund:"+refund, "refund")

		before := len(w.payments(embedded, m.c.id))
		w.deliverCCBill("Refund", m.reversal(refund, "9.99"))
		require.Len(t, w.payments(embedded, m.c.id), before, "a redelivered refund is recorded once")
	})

	t.Run("chargeback reverses the sale and revokes access", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		chargeback := ccbillNumericID()
		w.deliverCCBill("Chargeback", m.reversal(chargeback, "9.99"))
		require.False(t, m.c.entitled(m.ent))
		require.NotEqual(t, "active", w.subscription(embedded, m.sub).Status)
		m.requireReversal("chargeback:"+chargeback, "chargeback")
	})

	t.Run("posts from outside CCBill's ranges are refused", func(t *testing.T) {
		w := newWorld(t)
		m := importCCBill(t, w)
		cancel := map[string]string{"subscriptionId": m.railSub, "source": "webAdmin"}
		status, _ := w.postCCBill("Cancellation", "", cancel)
		require.Equal(t, http.StatusForbidden, status, "the loopback peer is a proxy, not CCBill")
		status, _ = w.postCCBill("Cancellation", "203.0.113.9", cancel)
		require.Equal(t, http.StatusForbidden, status)
		require.Equal(t, "active", w.subscription(embedded, m.sub).Status)
	})
}

// A NewSaleSuccess can still arrive from a FlexForm link OpenRails no longer
// issues. It never enrolls a new CCBill agreement: the refusal is explicit
// and leaves an operator repair alert for the money CCBill took.
func TestCCBillNewSaleIsRefused(t *testing.T) {
	w := newWorld(t)
	m := importCCBill(t, w)
	stranger := w.newCustomer()
	sale := func(subscriptionID, txn string) map[string]string {
		return map[string]string{
			"subscriptionId": subscriptionID, "transactionId": txn,
			"email": "stranger@example.test", "username": stranger.id,
			"formName": ccbillFormName, "flexId": ccbillFlexID, "subscriptionTypeId": ccbillRBO,
			"billedInitialPrice": "9.99", "billedRecurringPrice": "9.99", "billedCurrencyCode": "840",
			"subscriptionInitialPrice": "9.99", "subscriptionRecurringPrice": "9.99", "subscriptionCurrencyCode": "840",
			"initialPeriod": "30", "recurringPeriod": "30", "rebills": "99",
			"nextRenewalDate": ccbillDate(w.clock.Now().Add(30 * day)),
			"paymentType":     "CREDIT", "cardType": "VISA", "last4": "1111", "expDate": "1235",
		}
	}

	txn := ccbillNumericID()
	body := w.deliverCCBill("NewSaleSuccess", sale(ccbillNumericID(), txn))
	require.Equal(t, "refused", body["status"], "%v", body)
	require.Equal(t, "ccbill_new_subscription_unsupported", body["code"], "%v", body)
	subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: stranger.id})
	require.NoError(t, err)
	require.Empty(t, subs.Data, "no CCBill agreement is enrolled")
	require.False(t, stranger.entitled(m.ent))

	status, raw := w.staff(http.MethodGet, "/v1/merchant/repair-alerts")
	require.Equal(t, http.StatusOK, status, raw)
	require.Contains(t, raw, "ccbill_new_sale_refused")
	require.Contains(t, raw, txn)

	// A replayed sale for a membership OpenRails already holds changes nothing.
	body = w.deliverCCBill("NewSaleSuccess", sale(m.railSub, m.saleTxn))
	require.Equal(t, "accepted", body["status"], "%v", body)
	subs, err = w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: m.c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 1)
	require.Equal(t, "active", subs.Data[0].Status)
	require.True(t, subs.Data[0].CurrentPeriodEndsAt.Equal(m.paidThrough), "%v", subs.Data[0].CurrentPeriodEndsAt)
}

// engineCharges counts every charge OpenRails submitted on any rail.
func (w *world) engineCharges() int {
	return len(w.stripe.mutations("/v1/payment_intents")) + len(w.nmi.Attempts())
}
