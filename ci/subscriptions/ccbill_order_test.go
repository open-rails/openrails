//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/openrailstest"
)

// CCBill sells a membership on its FlexForm through an order attempt, as the
// card PSP's fallback: the order offers it once its card PSP is unavailable
// or has declined twice on the order; Solana is a peer, always offered.
// CCBill's NewSaleSuccess for the attempt completes the order with the
// subscription CCBill bills, once; a sale the order no longer takes is never
// granted.

type ccbillShop struct {
	w         *world
	directory *openrailstest.UserInfo
	price     string
	ent       string
}

// openCCBillShop is the merchant selling premium as the doujins catalog
// does: card (nmi), CCBill declared nmi's fallback, and a Solana plan. nmi,
// when given, replaces the card PSP's declaration.
func openCCBillShop(t *testing.T, nmi *openrails.PSPConfig) *ccbillShop {
	t.Helper()
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12)
	fake, signer := withSolana(t, w)
	solana := w.declare
	w.declare = func(psps map[string]openrails.PSPConfig) {
		solana(psps)
		delete(psps, "stripe")
		psps["ccbill"] = openrails.CCBillPSP{AccountID: ccbillAcct, Salt: "e2e-ccbill-salt", FallbackFor: []string{"nmi"}}.PSPConfig()
		if nmi != nil {
			psps["nmi"] = *nmi
		}
	}
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.start()
	plan, err := fake.Plan(signer.PublicKey(), 4242, solanafake.DevnetDUSDMint, 23_000_000, monthHours)
	require.NoError(t, err)
	key, err := w.applyCatalog(fmt.Sprintf(`  "{key}-monthly":
    currency: usd
    unit_amount: 23000000
    billing_interval_hours: 720
    access_duration_hours: 720
    psps:
      nmi: {}
      ccbill:
        flex_id: %s
        recurring_billing_option_id: "%s"
      solana:
        plan_pda: %s
        plan_id: "4242"
`, ccbillFlexID, ccbillRBO, plan))
	require.NoError(t, err)
	return &ccbillShop{w: w, directory: directory, price: priceID(t, w, key, key+"-monthly"), ent: key}
}

// buyer is a customer the merchant's directory names.
func (s *ccbillShop) buyer() (*customer, string) {
	c := s.w.newCustomer()
	name := "Buyer" + uuid.NewString()[:8]
	s.directory.Put(userinfo.User{ID: c.id, Email: strings.ToLower(name) + "@example.test", Username: name})
	return c, name
}

// openOrder is c's open order of the membership.
func (s *ccbillShop) openOrder(c *customer) string {
	s.w.t.Helper()
	created := c.order(http.MethodPost, "/orders", "", map[string]any{"lines": []any{map[string]any{"price_id": s.price}}})
	require.Equal(s.w.t, http.StatusCreated, created.status, "%v", created.body)
	return created.body["id"].(string)
}

// offered is the order's payment options by rail.
func offered(order map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, raw := range order["payment_options"].([]any) {
		option := raw.(map[string]any)
		out[option["rail"].(string)] = option
	}
	return out
}

func (s *ccbillShop) read(c *customer, id string) map[string]any {
	return c.order(http.MethodGet, "/orders/"+id, "", nil).body
}

func (s *ccbillShop) payCard(c *customer, id string, decline string) orderCall {
	token := s.w.nmi.Tokenize(card{Brand: "visa", Last4: "1111"})
	if decline != "" {
		token = s.w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: decline})
	}
	return c.order(http.MethodPost, "/orders/"+id+"/pay", "card-"+uuid.NewString(), map[string]any{"expected_total": "23000000",
		"payment": map[string]any{"token": token, "psp_id": s.w.psp["nmi"], "billing_details": map[string]any{"name": "Page Payer"}}})
}

func (s *ccbillShop) payCCBill(c *customer, id string) orderCall {
	return c.order(http.MethodPost, "/orders/"+id+"/pay", "ccbill-"+uuid.NewString(), map[string]any{"expected_total": "23000000",
		"payment": map[string]any{"return_url": "https://e2e.test/return", "psp_id": s.w.psp["ccbill"]}})
}

// flexForm is the FlexForm link an order awaiting CCBill sends its customer
// to.
func flexForm(t *testing.T, order map[string]any) *url.URL {
	t.Helper()
	payment := order["payment"].(map[string]any)
	require.Equal(t, "requires_action", payment["status"], "%v", order)
	action := payment["next_action"].(map[string]any)
	require.Equal(t, "redirect_to_url", action["type"])
	link, err := url.Parse(action["url"].(string))
	require.NoError(t, err)
	return link
}

// liveAttempt is the order's live attempt id, as the FlexForm names it.
func (s *ccbillShop) liveAttempt(id string) string {
	s.w.t.Helper()
	order, err := billing.ParseOrderID(id)
	require.NoError(s.w.t, err)
	var attempt uuid.UUID
	require.NoError(s.w.t, s.w.pool.QueryRow(s.w.t.Context(), s.w.q(`SELECT attempt_id FROM billing.orders WHERE id = $1`), order.UUID()).Scan(&attempt))
	return billing.OrderAttemptID(attempt).String()
}

func (s *ccbillShop) attemptStatus(reservation string) string {
	s.w.t.Helper()
	id, err := billing.ParseOrderAttemptID(reservation)
	require.NoError(s.w.t, err)
	return s.w.attemptStatus(id)
}

// sale is CCBill's NewSaleSuccess for the FlexForm link: its customer,
// form, price and reservation.
func (s *ccbillShop) sale(link *url.URL, subscriptionID, transactionID string) map[string]string {
	q := link.Query()
	return map[string]string{
		"subscriptionId": subscriptionID, "transactionId": transactionID, "reservationId": q.Get("reservationId"),
		"email": q.Get("email"), "username": q.Get("username"),
		"flexId": ccbillFlexID, "subscriptionTypeId": ccbillRBO,
		"billedInitialPrice": "23.00", "billedRecurringPrice": "23.00", "billedCurrencyCode": "840",
		"subscriptionInitialPrice": "23.00", "subscriptionRecurringPrice": "23.00", "subscriptionCurrencyCode": "840",
		"initialPeriod": "30", "recurringPeriod": "30", "rebills": "99",
		"nextRenewalDate": ccbillDate(s.w.clock.Now().Add(30 * day)),
		"paymentType":     "CREDIT", "cardType": "VISA", "last4": "1111", "expDate": "1235",
	}
}

func (s *ccbillShop) memberships(c *customer) []billing.Subscription {
	return s.w.memberships(c)
}

func TestCCBillFallbackOrder(t *testing.T) {
	t.Run("CCBill stands in after two card declines and its sale completes the order once", func(t *testing.T) {
		s := openCCBillShop(t, nil)
		w := s.w
		c, name := s.buyer()
		id := s.openOrder(c)

		options := offered(s.read(c, id))
		require.Contains(t, options, "nmi")
		require.Contains(t, options, "solana", "Solana is a peer, always offered")
		require.NotContains(t, options, "ccbill", "CCBill waits while the card PSP takes the order")
		refused := s.payCCBill(c, id)
		require.Equal(t, http.StatusConflict, refused.status, "%v", refused.body)
		require.Equal(t, "payment_option_withheld", orderError(refused))
		stranger, _ := s.buyer()
		created := stranger.order(http.MethodPost, "/orders", "direct-"+uuid.NewString(), map[string]any{"lines": []any{map[string]any{"price_id": s.price}}, "expected_total": "23000000",
			"payment": map[string]any{"return_url": "https://e2e.test/return", "psp_id": w.psp["ccbill"]}})
		require.Equal(t, "payment_option_withheld", orderError(created), "%v", created.body)

		declined := s.payCard(c, id, "202")
		require.Equal(t, http.StatusPaymentRequired, declined.status, "%v", declined.body)
		options = offered(orderOf(declined))
		require.NotContains(t, options, "ccbill", "one decline is not the threshold")
		require.Contains(t, options, "solana")

		declined = s.payCard(c, id, "202")
		require.Equal(t, http.StatusPaymentRequired, declined.status, "%v", declined.body)
		options = offered(s.read(c, id))
		require.Contains(t, options, "ccbill", "a second card decline unlocks the fallback: %v", options)
		require.Equal(t, true, options["ccbill"]["fallback"])
		require.Equal(t, []any{"redirect"}, options["ccbill"]["accepts"])
		require.Contains(t, options, "nmi", "the card stays offered")
		require.Contains(t, options, "solana")

		// The FlexForm link carries the attempt, the customer and the price.
		paying := s.payCCBill(c, id)
		require.Equal(t, http.StatusOK, paying.status, "%v", paying.body)
		link := flexForm(t, paying.body)
		require.Equal(t, "https://sandbox-api.ccbill.com/wap-frontflex/flexforms/"+ccbillFlexID, link.Scheme+"://"+link.Host+link.Path)
		q := link.Query()
		require.Equal(t, s.liveAttempt(id), q.Get("reservationId"))
		require.Equal(t, name, q.Get("username"))
		require.Equal(t, strings.ToLower(name)+"@example.test", q.Get("email"), "the directory's email, never the browser's")
		require.NotContains(t, q, "formName", "a FlexForm is its flex_id alone")
		require.Equal(t, "840", q.Get("currencyCode"))
		require.Equal(t, "999999", q.Get("clientAccnum"))
		require.NotEmpty(t, q.Get("signature"))

		// One live CCBill page per order: paying again answers it.
		again := s.payCCBill(c, id)
		require.Equal(t, http.StatusOK, again.status, "%v", again.body)
		require.Equal(t, link.String(), flexForm(t, again.body).String())
		busy := s.payCard(c, id, "")
		require.Equal(t, "order_payment_in_progress", orderError(busy), "the card waits on the live page: %v", busy.body)

		// CCBill declines the card on its page: the order takes another
		// payment, the attempt failed.
		failed := ccbillNumericID()
		w.deliverCCBill("NewSaleFailure", map[string]string{"transactionId": failed, "reservationId": q.Get("reservationId"),
			"email": q.Get("email"), "username": name, "flexId": ccbillFlexID, "subscriptionTypeId": ccbillRBO,
			"billedCurrencyCode": "840", "failureCode": "BE-140", "failureReason": "Card declined"})
		read := s.read(c, id)
		require.Equal(t, "open", read["status"])
		require.Equal(t, "requires_payment_method", read["payment"].(map[string]any)["status"])
		require.NotNil(t, read["payment"].(map[string]any)["last_payment_error"])
		require.Equal(t, "failed", s.attemptStatus(q.Get("reservationId")))
		require.True(t, c.hasNotification("payment_method_failed"))
		require.Contains(t, offered(read), "ccbill", "CCBill's own decline does not withhold it")

		// The next page is a new attempt; its sale completes the order.
		paying = s.payCCBill(c, id)
		require.Equal(t, http.StatusOK, paying.status, "%v", paying.body)
		link = flexForm(t, paying.body)
		require.NotEqual(t, q.Get("reservationId"), link.Query().Get("reservationId"))
		railSub, txn := ccbillNumericID(), ccbillNumericID()
		body := w.deliverCCBill("NewSaleSuccess", s.sale(link, railSub, txn))
		require.Equal(t, "accepted", body["status"], "%v", body)

		read = s.read(c, id)
		require.Equal(t, "complete", read["status"], "%v", read)
		require.Equal(t, "succeeded", read["payment"].(map[string]any)["status"])
		require.Equal(t, "succeeded", s.attemptStatus(link.Query().Get("reservationId")))
		subs := s.memberships(c)
		require.Len(t, subs, 1)
		require.Equal(t, "ccbill", subs[0].Rail)
		require.Equal(t, railSub, str(subs[0].RailSubscriptionID))
		require.Equal(t, "provider", subs[0].CollectionPolicy, "CCBill bills it")
		require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		w.converge()
		require.True(t, c.entitled(s.ent))
		paid := completed(w.payments(embedded, c.id))
		require.Len(t, paid, 1)
		require.Equal(t, txn, paid[0].TransactionID)
		require.EqualValues(t, 23_000_000, paid[0].Amount)

		// CCBill redelivers: nothing changes.
		body = w.deliverCCBill("NewSaleSuccess", s.sale(link, railSub, txn))
		require.Equal(t, "accepted", body["status"], "%v", body)
		require.Len(t, s.memberships(c), 1)
		require.Len(t, completed(w.payments(embedded, c.id)), 1)

		// A second CCBill sale on the same page is never a second membership.
		other := ccbillNumericID()
		body = w.deliverCCBill("NewSaleSuccess", s.sale(link, ccbillNumericID(), other))
		require.Equal(t, "refused", body["status"], "%v", body)
		require.Equal(t, "ccbill_sale_unhonored", body["code"], "%v", body)
		require.Len(t, s.memberships(c), 1)
		require.Len(t, completed(w.payments(embedded, c.id)), 1)
		require.Contains(t, w.openFindings("consistency.ledger.unbooked"), "ccbill_sale_unhonored:"+other)
	})

	t.Run("a sale for an order paid on the card meanwhile is never granted", func(t *testing.T) {
		s := openCCBillShop(t, nil)
		w := s.w
		c, _ := s.buyer()
		id := s.openOrder(c)
		for range 2 {
			require.Equal(t, http.StatusPaymentRequired, s.payCard(c, id, "202").status)
		}
		paying := s.payCCBill(c, id)
		require.Equal(t, http.StatusOK, paying.status, "%v", paying.body)
		link := flexForm(t, paying.body)

		// The customer leaves CCBill's page; once it lapses the card pays.
		w.advance(2 * time.Hour)
		paid := s.payCard(c, id, "")
		require.Equal(t, http.StatusOK, paid.status, "%v", paid.body)
		require.Equal(t, "complete", orderOf(paid)["status"], "%v", paid.body)
		require.Equal(t, "failed", s.attemptStatus(link.Query().Get("reservationId")), "the lapsed page is closed")
		require.Len(t, s.memberships(c), 1)

		// CCBill reports the sale on the lapsed page late.
		txn := ccbillNumericID()
		body := w.deliverCCBill("NewSaleSuccess", s.sale(link, ccbillNumericID(), txn))
		require.Equal(t, "refused", body["status"], "%v", body)
		require.Equal(t, "ccbill_sale_unhonored", body["code"], "%v", body)
		subs := s.memberships(c)
		require.Len(t, subs, 1, "no second membership")
		require.Equal(t, "nmi", subs[0].Rail)
		require.Len(t, completed(w.payments(embedded, c.id)), 1, "the late sale is not booked as the order's")
		require.Contains(t, w.openFindings("consistency.ledger.unbooked"), "ccbill_sale_unhonored:"+txn, "the merchant refunds and cancels it at CCBill")
	})

	t.Run("CCBill is offered at once while the card PSP is unavailable", func(t *testing.T) {
		unarmed := openrails.NMIPSP{AccountID: nmiAcct}.PSPConfig()
		s := openCCBillShop(t, &unarmed)
		c, _ := s.buyer()
		id := s.openOrder(c)
		options := offered(s.read(c, id))
		require.NotContains(t, options, "nmi")
		require.Contains(t, options, "ccbill", "%v", options)
		require.Equal(t, true, options["ccbill"]["fallback"])
		require.Contains(t, options, "solana")
		paying := s.payCCBill(c, id)
		require.Equal(t, http.StatusOK, paying.status, "%v", paying.body)
		flexForm(t, paying.body)
	})
}
