//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// A CCBill subscription id is a number: a legacy bigint column kept the
// 19-digit id CCBill sends without its leading 0.
const (
	ccbillStored   = "112233445566778899"
	ccbillProvider = "0112233445566778899"
)

// ccbillWebhookWorld is alpha with a sandbox CCBill account whose webhooks
// come from loopback: CCBill signs nothing, so the source address is its
// authentication.
func ccbillWebhookWorld(t *testing.T, configure ...func(*world)) *authorityWorld {
	t.Helper()
	return newAuthorityWorld(t, func(w *world) {
		w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillWebhookSources: []string{"127.0.0.1/32"}}
		m := cfgMerchant(w.cfg, "alpha")
		m.PSPs = billing.PSPs{"ccbill": openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: billing.EnvironmentSandbox,
			Salt: "critical-ccbill-salt"}.PSPConfig()}
		setCfgMerchant(&w.cfg, "alpha", m)
		for _, change := range configure {
			change(w)
		}
	})
}

// ccbillMemberPrice is a 10.00 USD 30-day CCBill price granting ccbill:member.
func ccbillMemberPrice(t *testing.T, client *openrails.Client) billing.PriceID {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "ccbill-member", DisplayName: "CCBill member", Entitlements: []string{"ccbill:member"}})
	require.NoError(t, err)
	price, err := client.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "ccbill-monthly", PSP: "ccbill", FlexID: "critical-flex",
		Currency: "USD", UnitAmount: ptr(int64(10_000_000)), BillingIntervalHours: ptr(720)})
	require.NoError(t, err)
	return price.ID
}

// legacyCCBillMember is a host's legacy book row: a CCBill membership paid
// through paidThrough under the id ref, with its initial sale.
func legacyCCBillMember(customer billing.CustomerID, price billing.PriceID, ref, sale string, paidThrough time.Time) (billing.DeclaredSubscription, billing.DeclaredTransaction) {
	start := paidThrough.Add(-720 * time.Hour)
	return billing.DeclaredSubscription{SourceID: "legacy-" + ref, Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: ref, StartedAt: start, PaidThrough: &paidThrough,
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: "legacy-" + ref, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paidThrough}},
		billing.DeclaredTransaction{RailSubscriptionID: ref, TransactionID: sale, Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start}
}

// importLegacyCCBill lands the members as the host's legacy book.
func importLegacyCCBill(t *testing.T, client *openrails.Client, asOf time.Time) func(billing.DeclaredSubscription, billing.DeclaredTransaction) {
	t.Helper()
	return func(sub billing.DeclaredSubscription, sale billing.DeclaredTransaction) {
		t.Helper()
		book := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: sub.Customer}},
			Subscriptions: []billing.DeclaredSubscription{sub}, Transactions: []billing.DeclaredTransaction{sale}}
		res, err := client.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Equal(t, []string{sub.SourceID}, res.Imported, "%v", res.Reasons)
	}
}

// ccbillNotice posts a CCBill webhook as CCBill sends it and returns the
// status and body.
func ccbillNotice(t *testing.T, a *authorityWorld, eventType string, fields map[string]any) (int, string) {
	t.Helper()
	return ccbillNoticeTo(t, a, "999999", "0000", eventType, fields)
}

// ccbillNoticeTo is ccbillNotice for the CCBill account accnum-subacc.
func ccbillNoticeTo(t *testing.T, a *authorityWorld, accnum, subacc, eventType string, fields map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"clientAccnum": accnum, "clientSubacc": subacc, "timestamp": a.clock.Now().UTC().Format(time.DateTime)}
	for k, v := range fields {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/webhooks/ccbill/"+accnum+"-"+subacc+"?eventType="+eventType, bytes.NewReader(raw))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	response, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(out)
}

// requireCCBillCard: the customer's payment of the CCBill transaction names
// the card CCBill reported.
func requireCCBillCard(t *testing.T, client *openrails.Client, customer billing.CustomerID, transaction, brand, last4 string) {
	t.Helper()
	page, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
	require.NoError(t, err)
	for _, payment := range page.Items {
		if payment.TransactionID == transaction {
			require.NotNil(t, payment.Card, "the CCBill payment names its card")
			require.Equal(t, brand, *payment.Card.Brand)
			require.Equal(t, last4, *payment.Card.Last4)
			return
		}
	}
	t.Fatalf("no payment of CCBill transaction %s", transaction)
}

func onlySubscription(t *testing.T, client *openrails.Client, customer billing.CustomerID) billing.Subscription {
	t.Helper()
	subs, err := client.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	return subs.Items[0]
}

func reasonOf(r billing.PSPImportRecord) string {
	if r.Reason == nil {
		return ""
	}
	return *r.Reason
}

func entitledTo(t *testing.T, client *openrails.Client, customer billing.CustomerID, entitlement string) bool {
	t.Helper()
	access, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}})
	require.NoError(t, err)
	return access.Entitlements[entitlement]
}

func TestCCBillIdentity(t *testing.T) {
	t.Run("OR-J38-S37", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		carol, dave := authtest.NewUser(t, a.auth), authtest.NewUser(t, a.auth)
		alice, bob, carolID, daveID := customerOf(a.alice), customerOf(a.bob), customerOf(carol), customerOf(dave)
		// Payments are never in the future: the business clock stays put, and
		// alice's and dave's paid periods have just ended.
		now := a.clock.Now().UTC().Truncate(time.Second)
		paid, due := now.Add(5*24*time.Hour), now.Add(-time.Hour)
		land := importLegacyCCBill(t, a.alpha, now)
		land(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due))
		land(legacyCCBillMember(bob, price, "223344556677889900", "0812233445566778802", paid))
		land(legacyCCBillMember(carolID, price, "334455667788990011", "0812233445566778803", paid))
		land(legacyCCBillMember(daveID, price, "445566778899001122", "0812233445566778804", due))
		for _, c := range []billing.CustomerID{bob, carolID} {
			require.True(t, entitledTo(t, a.alpha, c, "ccbill:member"))
		}

		t.Run("another-number-matches-nothing", func(t *testing.T) {
			for _, other := range []string{"0112233445566778898", "1122334455667788990"} {
				status, body := ccbillNotice(t, a, "Cancellation", map[string]any{"subscriptionId": other, "reason": "user", "source": "webAdmin"})
				require.NotEqual(t, http.StatusOK, status, "%s names no subscription: %s", other, body)
			}
			sub := onlySubscription(t, a.alpha, alice)
			require.Equal(t, ccbillStored, *sub.RailSubscriptionID)
			require.NotEqual(t, billing.SubscriptionStatus("canceled"), sub.Status)
			require.Nil(t, sub.CanceledAt)
		})

		t.Run("cancellation-applies-and-takes-the-providers-spelling", func(t *testing.T) {
			status, body := ccbillNotice(t, a, "Cancellation", map[string]any{"subscriptionId": "0223344556677889900", "reason": "user", "source": "webAdmin"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, bob)
			require.Equal(t, "0223344556677889900", *sub.RailSubscriptionID, "the provider's spelling wins")
			require.NotNil(t, sub.CanceledAt, "status %s", sub.Status)
			require.True(t, entitledTo(t, a.alpha, bob, "ccbill:member"), "a cancellation keeps the paid period")
		})

		t.Run("chargeback-applies-and-takes-the-providers-spelling", func(t *testing.T) {
			status, body := ccbillNotice(t, a, "Chargeback", map[string]any{"subscriptionId": "0334455667788990011", "transactionId": "0812233445566778803",
				"amount": "10.00", "currencyCode": "840", "reason": "fraud"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, carolID)
			require.Equal(t, "0334455667788990011", *sub.RailSubscriptionID)
			require.Equal(t, billing.SubscriptionStatus("canceled"), sub.Status)
			require.Equal(t, "chargeback", *sub.CancelType)
			require.False(t, entitledTo(t, a.alpha, carolID, "ccbill:member"), "a chargeback ends access now")
			payments := ccbillPayments(t, a.world, carolID)
			require.Contains(t, payments, "chargeback:0812233445566778803", "%v", payments)
		})

		t.Run("renewal-extends-paid-through-and-access", func(t *testing.T) {
			at := a.clock.Now().UTC()
			require.True(t, onlySubscription(t, a.alpha, alice).CurrentPeriodEndsAt.Equal(due))
			next := due.Add(720 * time.Hour)
			status, body := ccbillNotice(t, a, "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider, "transactionId": "0912233445566778801",
				"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": at.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly),
				"cardType": "VISA", "last4": "1111"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, alice)
			require.Equal(t, ccbillProvider, *sub.RailSubscriptionID, "the provider's spelling wins")
			require.Equal(t, billing.SubscriptionStatus("active"), sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(time.Date(next.Year(), next.Month(), next.Day(), 23, 59, 59, 0, time.UTC)), "paid through %s", sub.CurrentPeriodEndsAt)
			require.True(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
			require.EqualValues(t, 10_000_000, ccbillPayments(t, a.world, alice)["0912233445566778801"])
			requireCCBillCard(t, a.alpha, alice, "0912233445566778801", "visa", "1111")
		})

		t.Run("an-invoice-sale-names-the-card-ccbill-billed", func(t *testing.T) {
			// Selling a FlexForm is a provider write.
			a := ccbillWebhookWorld(t, func(w *world) { w.posture = "full" })
			price := ccbillMemberPrice(t, a.alpha)
			erin := authtest.NewUser(t, a.auth)
			token := authtest.SignIn(t, a.auth, erin).AccessToken
			// The page names the customer: the options say what it still needs,
			// and paying without it is refused in the customer's words.
			requires := func(token string) []string {
				t.Helper()
				status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/preview", token, billing.PreviewMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}})
				require.Equal(t, http.StatusOK, status, string(raw))
				var preview billing.InvoicePreview
				require.NoError(t, json.Unmarshal(raw, &preview))
				require.Len(t, preview.PaymentOptions, 1)
				require.Equal(t, billing.RailCCBill, preview.PaymentOptions[0].Rail)
				return preview.PaymentOptions[0].Requires
			}
			require.Empty(t, requires(token), "a verified email and username name the customer")
			unverified, err := a.auth.CreateUser(t.Context(), iam.NewUser{Email: "unverified-" + erin.Username + "@example.com", Username: "unverified" + erin.Username[4:], Password: authtest.Password})
			require.NoError(t, err)
			pending := authtest.SignIn(t, a.auth, authtest.User{User: unverified, Password: authtest.Password}).AccessToken
			require.Equal(t, []string{"verified_email"}, requires(pending))
			status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", pending, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}, ExpectedTotal: ptr(int64(10_000_000)),
				Payment: &billing.InvoicePaymentParams{ReturnURL: alphaSite + "/paid"}},
				map[string]string{"Idempotency-Key": "ccbill-unverified-invoice"})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			require.Contains(t, string(raw), "customer_email_required")
			require.Contains(t, string(raw), "Verify your email and set a username to pay with CCBill.")
			status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", token, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}, ExpectedTotal: ptr(int64(10_000_000)),
				Payment: &billing.InvoicePaymentParams{ReturnURL: alphaSite + "/paid"}},
				map[string]string{"Idempotency-Key": "ccbill-card-invoice"})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var invoice billing.Invoice
			require.NoError(t, json.Unmarshal(raw, &invoice))
			require.NotNil(t, invoice.Payment.NextAction, string(raw))
			page, err := url.Parse(*invoice.Payment.NextAction.URL)
			require.NoError(t, err)
			next := a.clock.Now().UTC().Add(720 * time.Hour)
			status, body := ccbillNotice(t, a, "NewSaleSuccess", map[string]any{"subscriptionId": "0556677889900112233", "transactionId": "0812233445566778809",
				"reservationId": page.Query().Get("reservationId"), "flexId": "critical-flex", "username": erin.Username,
				"billedInitialPrice": "10.00", "billedRecurringPrice": "10.00", "billedCurrencyCode": "840", "nextRenewalDate": next.Format(time.DateOnly),
				"cardType": "MASTERCARD", "last4": "5100"})
			require.Equal(t, http.StatusOK, status, body)
			requireCCBillCard(t, a.alpha, customerOf(erin), "0812233445566778809", "mastercard", "5100")
		})

		t.Run("an-upgrade-names-the-card-ccbill-billed", func(t *testing.T) {
			upper, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "ccbill-plus", DisplayName: "CCBill plus", Entitlements: []string{"ccbill:plus"}})
			require.NoError(t, err)
			_, err = a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: upper.ID, Key: "ccbill-plus-monthly", PSP: "ccbill", FlexID: "critical-plus-flex",
				Currency: "USD", UnitAmount: ptr(int64(20_000_000)), BillingIntervalHours: ptr(720)})
			require.NoError(t, err)
			next := a.clock.Now().UTC().Add(720 * time.Hour)
			status, body := ccbillNotice(t, a, "UpgradeSuccess", map[string]any{"originalSubscriptionId": "0223344556677889900", "subscriptionId": "0667788990011223344",
				"transactionId": "0812233445566778810", "flexId": "critical-plus-flex", "billedInitialPrice": "20.00", "billedRecurringPrice": "20.00", "recurringPeriod": "30",
				"billedCurrencyCode": "840", "nextRenewalDate": next.Format(time.DateOnly), "cardType": "AMEX", "last4": "0005"})
			require.Equal(t, http.StatusOK, status, body)
			requireCCBillCard(t, a.alpha, bob, "0812233445566778810", "amex", "0005")
		})

		t.Run("a-replayed-legacy-book-names-the-respelled-row", func(t *testing.T) {
			renewed := onlySubscription(t, a.alpha, alice)
			sub, sale := legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due)
			one := 1
			res, err := a.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "ccbill"}, SubscriptionsExhaustive: true, ExpectedSubscriptions: &one,
				Customers: []billing.DeclaredCustomer{{Customer: alice}}, Subscriptions: []billing.DeclaredSubscription{sub}, Transactions: []billing.DeclaredTransaction{sale}})
			require.NoError(t, err)
			require.Empty(t, res.Blocked, "%v", res.Reasons)
			after := onlySubscription(t, a.alpha, alice)
			require.Equal(t, ccbillProvider, *after.RailSubscriptionID, "a host book never respells a row")
			require.Equal(t, billing.SubscriptionStatus("active"), after.Status, "the book names the row it holds: nothing is absent")
			require.True(t, after.CurrentPeriodEndsAt.Equal(*renewed.CurrentPeriodEndsAt))
			require.True(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
		})

		t.Run("expiration-ends-a-lapsed-membership", func(t *testing.T) {
			status, body := ccbillNotice(t, a, "Expiration", map[string]any{"subscriptionId": "0445566778899001122"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, daveID)
			require.Equal(t, "0445566778899001122", *sub.RailSubscriptionID)
			require.Equal(t, billing.SubscriptionStatus("canceled"), sub.Status)
			require.False(t, entitledTo(t, a.alpha, daveID, "ccbill:member"))
		})
	})

	t.Run("OR-J38-S82/a-postback-names-its-customer-by-a-username-many-others-contain", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		name := "member" + uuid.NewString()[:8]
		user := func(username string) iam.User {
			t.Helper()
			u, err := a.auth.CreateUser(t.Context(), iam.NewUser{Email: username + "@example.com", Username: username, Password: authtest.Password, EmailVerified: true})
			require.NoError(t, err)
			return u
		}
		// More than one search's worth of people whose usernames contain the
		// buyer's join the host's directory first.
		for i := range 55 {
			user(fmt.Sprintf("%sx%02d", name, i))
		}
		buyer := billing.CustomerID(uuid.MustParse(user(name).ID))
		status, body := ccbillNotice(t, a, "NewSaleFailure", map[string]any{"transactionId": "0912233445566778897", "username": name,
			"flexId": "critical-flex", "billedCurrencyCode": "840", "failureCode": "15", "failureReason": "Insufficient funds"})
		require.Equal(t, http.StatusOK, status, body)
		require.True(t, factEmitted(t, a.alpha, "payment_method_failed", buyer), "the decline names the one customer whose username it is")
	})
	t.Run("OR-J38-S75/an-end-the-provider-confirmed-is-not-revived-by-a-later-renewal", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		alice := customerOf(a.alice)
		now := a.clock.Now().UTC().Truncate(time.Second)
		due := now.Add(-time.Hour)
		sub, sale := legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due)
		sub.Cancel = billing.CancelEvidence{Kind: "provider_terminated", At: due}
		importLegacyCCBill(t, a.alpha, now)(sub, sale)
		ended := onlySubscription(t, a.alpha, alice)
		require.Equal(t, billing.SubscriptionStatus("canceled"), ended.Status)
		require.NotNil(t, ended.CancelType)
		require.Equal(t, "provider", *ended.CancelType, "read back, the end is still the provider's")
		// A renewal read from the stored row afterwards cannot reopen it.
		next := due.Add(720 * time.Hour)
		status, body := ccbillNotice(t, a, "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider, "transactionId": "0912233445566778801",
			"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": now.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly),
			"cardType": "VISA", "last4": "1111"})
		require.Equal(t, http.StatusOK, status, body)
		after := onlySubscription(t, a.alpha, alice)
		require.Equal(t, billing.SubscriptionStatus("canceled"), after.Status, "the end the provider confirmed stands")
		require.Equal(t, "provider", *after.CancelType)
		require.False(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
		require.EqualValues(t, 10_000_000, ccbillPayments(t, a.world, alice)["0912233445566778801"], "the charge is kept for refund review")
		review, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.charge.after_cancellation"})
		require.NoError(t, err)
		require.Len(t, review.Items, 1, "a charge after a final end is raised for review")
		require.Equal(t, billing.FindingStatus("requires_review"), review.Items[0].Status)
	})

	t.Run("OR-J38-S79/a-ccbill-cancel-the-pull-lands-is-restored-by-its-rebill", func(t *testing.T) {
		// The Cancellation webhook never came; DataLink reports it, with the
		// time paid for ending two days ago.
		var feed *httptest.Server
		var canceled, due time.Time
		a := ccbillWebhookWorld(t, func(w *world) {
			canceled = w.clock.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
			due = w.clock.Now().UTC().Truncate(time.Second).Add(-48 * time.Hour)
			feed = dataLinkCancellationFeed(t, ccbillProvider, canceled, due)
			declareCCBill(w, feed.URL)
			w.deps.ProviderEndpoints.CCBillWebhookSources = []string{"127.0.0.1/32"}
			// Lifecycle repairs need full writes: a readonly merchant's pull
			// observes, and never reads its unverified rows.
			w.posture = "full"
		})
		price := ccbillMemberPrice(t, a.alpha)
		alice := customerOf(a.alice)
		now := a.clock.Now().UTC().Truncate(time.Second)
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due))
		armDestructive(t, a, true)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
		ended := onlySubscription(t, a.alpha, alice)
		require.Equal(t, billing.SubscriptionStatus("canceled"), ended.Status, "the pull lands the cancel")
		require.NotNil(t, ended.CancelType)
		require.Equal(t, "expired", *ended.CancelType, "CCBill's own end is a lapse")
		require.False(t, entitledTo(t, a.alpha, alice, "ccbill:member"))

		// The member reactivates at CCBill and is rebilled.
		status, body := ccbillNoticeTo(t, a, "945280", "0001", "UserReactivation", map[string]any{"subscriptionId": ccbillProvider, "transactionId": "0912233445566778800"})
		require.Equal(t, http.StatusOK, status, body)
		next := due.Add(720 * time.Hour)
		status, body = ccbillNoticeTo(t, a, "945280", "0001", "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider, "transactionId": "0912233445566778801",
			"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": now.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly),
			"cardType": "VISA", "last4": "1111"})
		require.Equal(t, http.StatusOK, status, body)
		restored := onlySubscription(t, a.alpha, alice)
		require.Equal(t, billing.SubscriptionActive, restored.Status, "the rebill restores the lapsed membership")
		require.True(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
	})

	t.Run("OR-J38-S80/a-ccbill-cancellation-is-undone-by-reactivation-or-a-late-rebill", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		bob, carol := customerOf(a.bob), customerOf(authtest.NewUser(t, a.auth))
		now := a.clock.Now().UTC().Truncate(time.Second)
		paid, due := now.Add(5*24*time.Hour), now.Add(-time.Hour)
		land := importLegacyCCBill(t, a.alpha, now)
		land(legacyCCBillMember(bob, price, "223344556677889900", "0812233445566778802", paid))
		land(legacyCCBillMember(carol, price, "334455667788990011", "0812233445566778803", due))
		cancel := func(sub string) {
			t.Helper()
			status, body := ccbillNotice(t, a, "Cancellation", map[string]any{"subscriptionId": sub, "reason": "user", "source": "webAdmin"})
			require.Equal(t, http.StatusOK, status, body)
		}
		// Inside its paid period, a reactivation resumes it.
		cancel("0223344556677889900")
		canceled := onlySubscription(t, a.alpha, bob)
		require.Equal(t, billing.SubscriptionStatus("canceled"), canceled.Status)
		require.Equal(t, "expired", *canceled.CancelType)
		status, body := ccbillNotice(t, a, "UserReactivation", map[string]any{"subscriptionId": "0223344556677889900", "transactionId": "0912233445566778802"})
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, billing.SubscriptionActive, onlySubscription(t, a.alpha, bob).Status, "the reactivation resumes it")
		require.True(t, entitledTo(t, a.alpha, bob, "ccbill:member"))
		// Past it, CCBill's late rebill restores it.
		cancel("0334455667788990011")
		require.Equal(t, billing.SubscriptionStatus("canceled"), onlySubscription(t, a.alpha, carol).Status)
		next := due.Add(720 * time.Hour)
		status, body = ccbillNotice(t, a, "RenewalSuccess", map[string]any{"subscriptionId": "0334455667788990011", "transactionId": "0912233445566778803",
			"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": now.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly),
			"cardType": "VISA", "last4": "1111"})
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, billing.SubscriptionActive, onlySubscription(t, a.alpha, carol).Status, "the late rebill restores it")
		require.True(t, entitledTo(t, a.alpha, carol, "ccbill:member"))
	})

	t.Run("OR-J38-S81/a-failed-rebill-cancel-on-a-decided-end-keeps-it-decided", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		alice, carol := customerOf(a.alice), customerOf(a.bob)
		now := a.clock.Now().UTC().Truncate(time.Second)
		due := now.Add(-time.Hour)
		// Alice's end the provider confirmed; Carol's is a chargeback.
		provided, sale := legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due)
		provided.Cancel = billing.CancelEvidence{Kind: "provider_terminated", At: due}
		land := importLegacyCCBill(t, a.alpha, now)
		land(provided, sale)
		land(legacyCCBillMember(carol, price, "334455667788990011", "0812233445566778803", due))
		status, body := ccbillNotice(t, a, "Chargeback", map[string]any{"subscriptionId": "0334455667788990011", "transactionId": "0812233445566778803",
			"amount": "10.00", "currencyCode": "840", "reason": "fraud"})
		require.Equal(t, http.StatusOK, status, body)
		for _, member := range []struct {
			customer    billing.CustomerID
			sub, rebill string
			kind        string
		}{{alice, ccbillProvider, "0912233445566778801", "provider"}, {carol, "0334455667788990011", "0912233445566778803", "chargeback"}} {
			require.Equal(t, member.kind, *onlySubscription(t, a.alpha, member.customer).CancelType)
			// CCBill then reports a failed-rebill cancel, and bills again.
			status, body := ccbillNotice(t, a, "Cancellation", map[string]any{"subscriptionId": member.sub, "reason": "failedRB", "source": "failedRB"})
			require.Equal(t, http.StatusOK, status, body)
			require.Equal(t, member.kind, *onlySubscription(t, a.alpha, member.customer).CancelType, "a lapse never replaces a decided end")
			next := due.Add(720 * time.Hour)
			status, body = ccbillNotice(t, a, "RenewalSuccess", map[string]any{"subscriptionId": member.sub, "transactionId": member.rebill,
				"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": now.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly),
				"cardType": "VISA", "last4": "1111"})
			require.Equal(t, http.StatusOK, status, body)
			after := onlySubscription(t, a.alpha, member.customer)
			require.Equal(t, billing.SubscriptionStatus("canceled"), after.Status, "%s stays ended", member.kind)
			require.Equal(t, member.kind, *after.CancelType)
			require.False(t, entitledTo(t, a.alpha, member.customer, "ccbill:member"))
		}
	})

	t.Run("OR-J38-S38/datalink-import-matches-a-zero-dropped-legacy-id", func(t *testing.T) {
		iw := newImportWorld(t, "ccbill")
		price := ccbillMemberPrice(t, iw.alphaRemote)
		alice, bob := customerOf(iw.alice), customerOf(iw.bob)
		now := iw.clock.Now().UTC()
		paid := now.Truncate(24 * time.Hour).Add(5 * 24 * time.Hour)
		importLegacyCCBill(t, iw.alpha, now)(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", paid))
		_, err := iw.alpha.SetPSPImportMappings(t.Context(), billing.SetPSPImportMappingsParams{Mappings: []billing.PSPImportMapping{
			{Mapping: "legacy_ccbill_subscription", Key: "223344556677889900", Customer: &bob}}})
		require.NoError(t, err)

		// CCBill states its next rebill as a date: the cadence it implies is 30 days.
		signup, rebilled := paid.Add(-720*time.Hour), now.Add(-10*24*time.Hour).Truncate(24*time.Hour).Add(18*time.Hour)
		next := rebilled.Add(30 * 24 * time.Hour)
		iw.ccbill.AddMember(ccbillmock.Member{SubscriptionID: ccbillProvider, Username: iw.alice.Username, Email: iw.alice.Email, Signup: signup, Rebill: &paid})
		iw.ccbill.AddMember(ccbillmock.Member{SubscriptionID: "0223344556677889900", Username: "legacy-bob", Email: "legacy-bob@critical.invalid", Signup: signup, Rebill: &next})
		iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: "0223344556677889900", At: rebilled, Amount: "10.00"})
		iw.ccbill.AddMember(ccbillmock.Member{SubscriptionID: "0112233445566778898", Username: "stranger", Email: "stranger@critical.invalid", Signup: signup, Rebill: &paid})
		since := rebilled.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since, Rules: []billing.PSPImportRule{
			{Fact: "subscription_ref", Via: billing.PSPImportViaMapping, Mapping: "legacy_ccbill_subscription"},
			{Fact: "email", Via: billing.PSPImportViaDirectory}}})

		recs := iw.records(t, imp.ID)
		require.Equal(t, billing.PSPImportHeld, recs[ccbillProvider].State, reasonOf(recs[ccbillProvider]))
		require.Equal(t, alice, *recs[ccbillProvider].Customer)
		require.Equal(t, ccbillProvider, *onlySubscription(t, iw.alpha, alice).RailSubscriptionID, "the provider's spelling wins")
		require.Equal(t, billing.PSPImportLinked, recs["0223344556677889900"].State, reasonOf(recs["0223344556677889900"]))
		require.Equal(t, "rule:1", *recs["0223344556677889900"].LinkedVia, "the legacy id maps it")
		require.Equal(t, bob, *recs["0223344556677889900"].Customer)
		require.Equal(t, billing.PSPImportUnmatched, recs["0112233445566778898"].State, "another number is another subscription")

		recs = iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs["0223344556677889900"].State, reasonOf(recs["0223344556677889900"]))
		require.Equal(t, "0223344556677889900", *onlySubscription(t, iw.alpha, bob).RailSubscriptionID)
		held := onlySubscription(t, iw.alpha, alice)
		require.Equal(t, ccbillProvider, *held.RailSubscriptionID)
		require.Equal(t, "provider", iw.held(t, ccbillProvider).Authority, "Apply hands the held host row to the provider")
		require.True(t, iw.entitled(t, alice, "ccbill:member"))
	})

	t.Run("OR-J38-S39/provider-refresh-respells-and-records-the-rebill", func(t *testing.T) {
		var feed *httptest.Server
		var rebill time.Time
		w := newWorld(t, func(w *world) {
			rebill = w.clock.Now().Truncate(time.Second).Add(-24 * time.Hour)
			feed = dataLinkFeedFor(t, ccbillProvider, "0912233445566778801", rebill)
			declareCCBill(w, feed.URL)
		})
		price := ccbillMemberPrice(t, w.alpha)
		customer := billing.CustomerID(uuid.New())
		start := rebill.Add(-720 * time.Hour)
		book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member", Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: ccbillStored,
				StartedAt: start, PaidThrough: &rebill,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ccbillStored, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: rebill}}},
			Transactions: []billing.DeclaredTransaction{
				{RailSubscriptionID: ccbillStored, TransactionID: "0812233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
				{RailSubscriptionID: ccbillStored, TransactionID: "0912233445566778801", Type: "sale", Success: true, Currency: "USD", OccurredAt: rebill},
			}}
		res, err := w.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, res.Blocked, "%+v", res.Reasons)
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		require.Eventually(t, func() bool {
			return len(attributionJobs(t, fleet, rivertype.JobStateScheduled)) == 1
		}, 20*time.Second, 20*time.Millisecond, "the import's read waits for the pull")
		refreshMerchant(t, fleet, w.alpha.MerchantID().UUID())
		require.Equal(t, ccbillProvider, *onlySubscription(t, w.alpha, customer).RailSubscriptionID, "the roster's spelling wins")
		payments := ccbillPayments(t, w, customer)
		require.EqualValues(t, 10_000_000, payments["0912233445566778801"], "the pull records the rebill of the row it names: %v", payments)
	})

	t.Run("OR-J38-S40", func(t *testing.T) {
		var feed *httptest.Server
		var rebill time.Time
		w := newWorld(t, func(w *world) {
			rebill = w.clock.Now().Truncate(time.Second).Add(-10 * 24 * time.Hour)
			feed = dataLinkFeedFor(t, ccbillProvider, "0912233445566778801", rebill)
			declareCCBill(w, feed.URL)
		})
		price := ccbillMemberPrice(t, w.alpha)
		customer := billing.CustomerID(uuid.New())
		start, paid := rebill.Add(-720*time.Hour), rebill.Add(720*time.Hour)
		res, err := w.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"},
			Customers: []billing.DeclaredCustomer{{Customer: customer}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member", Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: ccbillStored,
				StartedAt: start, PaidThrough: &paid,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ccbillStored, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paid}}},
			Transactions: []billing.DeclaredTransaction{
				{RailSubscriptionID: ccbillStored, TransactionID: "0812233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
				{RailSubscriptionID: ccbillStored, TransactionID: "0912233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: rebill},
			}})
		require.NoError(t, err)
		require.Empty(t, res.Blocked, "%+v", res.Reasons)
		require.Equal(t, billing.SubscriptionStatus("active"), onlySubscription(t, w.alpha, customer).Status)

		for _, pass := range []struct {
			name      string
			mode      reconcile.Mode
			mutations *reconcile.LocalMutationPolicy
		}{
			{"advisory-matches-by-identity", reconcile.ModeAdvisory, nil},
			{"enforce-without-insert-matches-by-identity", reconcile.ModeEnforce, &reconcile.LocalMutationPolicy{Overwrite: true}},
		} {
			t.Run(pass.name, func(t *testing.T) {
				run := ccbillPull(t, w, feed, rebill.Add(-24*time.Hour), pass.mode, pass.mutations)
				for _, f := range run.Findings {
					require.NotContains(t, []reconcile.FindingType{reconcile.FindingRemoteSubMissingLocal, reconcile.FindingLocalActiveRemoteDead, reconcile.FindingStatusMismatch},
						f.Type, "the roster names the held row: %s %s %s", f.Type, f.SubjectKey, f.RecommendedAction)
				}
				sub := onlySubscription(t, w.alpha, customer)
				require.Equal(t, billing.SubscriptionStatus("active"), sub.Status)
				require.Nil(t, sub.CanceledAt)
				require.Equal(t, ccbillStored, *sub.RailSubscriptionID, "only a mirror write respells the row")
			})
		}
	})

	t.Run("OR-J38-S43/a-late-sale-on-an-invoice-paid-by-card-is-kept-for-refund", func(t *testing.T) {
		// A payment is never recorded in the database's future.
		a, provider := newPaymentWorld(t, func(w *world) {
			w.clock.Advance(-30 * 24 * time.Hour)
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillWebhookSources: []string{"127.0.0.1/32"}}
			m := cfgMerchant(w.cfg, "alpha")
			m.PSPs["ccbill"] = openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: billing.EnvironmentSandbox, Salt: "critical-ccbill-salt"}.PSPConfig()
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "member", DisplayName: "Member", Entitlements: []string{"member:paid"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "member-monthly", UnitAmount: 10_000_000, Currency: "USD",
			BillingIntervalHours: ptr(720), AccessDurationHours: ptr(720)})
		require.NoError(t, err)
		_, err = a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "member-ccbill", PSP: "ccbill", FlexID: "critical-flex",
			Currency: "USD", UnitAmount: ptr(int64(10_000_000)), BillingIntervalHours: ptr(720)})
		require.NoError(t, err)
		lines := []billing.InvoiceLineParams{{PriceID: price.ID}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/preview", a.aliceToken, billing.PreviewMyInvoiceParams{Lines: lines})
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.InvoicePreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		var page billing.PSPID
		for _, option := range preview.PaymentOptions {
			if option.Rail == "ccbill" {
				page = option.PSPID
			}
		}
		require.False(t, page.IsZero(), "the card sale's FlexForm stands in for it: %s", raw)

		// Alice opens the CCBill page and leaves; once it lapses she pays by card.
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Lines: lines, ExpectedTotal: ptr(int64(10_000_000)),
			Payment: &billing.InvoicePaymentParams{PSPID: &page, ReturnURL: alphaSite + "/paid"}}, map[string]string{"Idempotency-Key": "ccbill-page"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.NotNil(t, invoice.Payment.NextAction, string(raw))
		link, err := url.Parse(*invoice.Payment.NextAction.URL)
		require.NoError(t, err)
		a.clock.Advance(61 * time.Minute)
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, billing.PayInvoiceParams{ExpectedTotal: 10_000_000, Reusable: ptr(true),
			Payment: billing.InvoicePaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "card-after-page"})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.Equal(t, billing.InvoiceComplete, invoice.Status, string(raw))
		card := *invoice.Payment.PaymentID

		// The page is completed after all: CCBill charged her twice and bills a
		// subscription OpenRails never took. Its sale is kept for refund, once.
		alice := customerOf(a.alice)
		sale := map[string]any{"subscriptionId": "0778899001122334455", "transactionId": "0812233445566778820", "reservationId": link.Query().Get("reservationId"),
			"flexId": "critical-flex", "username": a.alice.Username, "billedInitialPrice": "10.00", "billedRecurringPrice": "10.00", "billedCurrencyCode": "840",
			"nextRenewalDate": a.clock.Now().UTC().Add(720 * time.Hour).Format(time.DateOnly), "cardType": "VISA", "last4": "1111"}
		for range 2 {
			status, body := ccbillNotice(t, a, "NewSaleSuccess", sale)
			require.Equal(t, http.StatusOK, status, body)
			a.clock.Advance(time.Second)
		}
		paid, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceComplete, paid.Status)
		require.Equal(t, card, *paid.Payment.PaymentID, "the card still pays the invoice")
		listed, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: alice})
		require.NoError(t, err)
		var held []billing.Payment
		for _, p := range listed.Items {
			if p.TransactionID == "0812233445566778820" {
				held = append(held, p)
			}
		}
		require.Len(t, held, 1, "the second charge is recorded once: %+v", listed.Items)
		require.Equal(t, billing.PaymentSucceeded, held[0].Status)
		require.EqualValues(t, 10_000_000, held[0].Amount)
		require.Equal(t, billing.RailCCBill, *held[0].Rail)
		require.Equal(t, invoice.ID, *held[0].InvoiceID)
		require.Nil(t, held[0].SubscriptionID, "the sale grants nothing")
		booked := glFor(t, a.world, "payment", held[0].ID.UUID().String())
		require.Len(t, booked, 1, "the books keep the second charge unapplied, for its refund")
		require.Equal(t, "cash_received", booked[0].Type)
		require.Equal(t, "cash_clearing", booked[0].Debit)
		require.Len(t, glFor(t, a.world, "payment", card.UUID().String()), 2, "the card's money pays the invoice")
		require.EqualValues(t, 10_000_000, unappliedCash(t, a.world, "USD"))
		require.Equal(t, billing.RailStripe, onlySubscription(t, a.alpha, alice).Rail, "the card's membership is the only one")
		late, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.invoice.late_payment"})
		require.NoError(t, err)
		require.Len(t, late.Items, 1)
		require.Equal(t, held[0].ID.UUID().String(), late.Items[0].SubjectKey)
		require.Equal(t, billing.FindingStatus("requires_review"), late.Items[0].Status)
		require.Contains(t, *late.Items[0].RecommendedAction, "Refund it at the provider")
		unbooked, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
		require.NoError(t, err)
		require.Len(t, unbooked.Items, 1, "one repair: cancel the subscription at CCBill")
		require.Equal(t, "ccbill_sale_invoice_paid", unbooked.Items[0].Evidence["operation"])
		require.Equal(t, "0778899001122334455", unbooked.Items[0].Evidence["metadata"].(map[string]any)["rail_subscription_id"])
		effects := provider.effects(t)
		require.Len(t, effects, 1, "the card is never refunded for CCBill's charge")
		require.Equal(t, "charge", effects[0].Kind)
	})
}

// completeRoster claims the roster it reads is complete. CCBill's never does:
// it is the strongest absence evidence a pull can hold.
type completeRoster struct{ reconcile.RailFetcher }

func (c completeRoster) Fetch(ctx context.Context, params reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	snap, err := c.RailFetcher.Fetch(ctx, params)
	if snap != nil {
		snap.Coverage.SubscriptionsExhaustive = true
	}
	return snap, err
}

// ccbillPull runs alpha's CCBill provider pull over feed in mode, with the
// policy a caller may hold that the provider refresh never does.
func ccbillPull(t *testing.T, w *world, feed *httptest.Server, since time.Time, mode reconcile.Mode, mutations *reconcile.LocalMutationPolicy) *reconcile.RunResult {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	dl := ccbill.NewDataLinkClient(slog.Default(), &config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001", Sandbox: true,
		DataLinkUsername: "critical-datalink", DataLinkPassword: "critical-datalink-password"})
	dl.BaseURL = feed.URL
	engine := reconcile.NewEngine(database, embedded.Graph(w.engine).Config, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderCCBill: completeRoster{reconcile.NewCCBillFetcher(dl)}}, nil)
	engine.Now = w.clock.Now
	psps, err := w.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	binding := reconcile.PSPBinding{ID: psps.Items[0].ID.UUID(), Rail: "ccbill", AccountID: "945280-0001"}
	var res *reconcile.RunResult
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), w.alpha.MerchantID()), func(ctx context.Context) error {
		res, err = engine.Run(ctx, reconcile.RunParams{Mode: mode, Mutations: mutations, Providers: []reconcile.Provider{reconcile.ProviderCCBill},
			PSPs: map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderCCBill: binding}, Since: since, Until: w.clock.Now()})
		return err
	}))
	return res
}

// dataLinkCancellationFeed is CCBill's DataLink after sub was canceled at
// canceled, paid through expires: no active member, and the cancellation in
// its export.
func dataLinkCancellationFeed(t *testing.T, sub string, canceled, expires time.Time) *httptest.Server {
	t.Helper()
	mst := time.FixedZone("MST", -7*60*60)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.URL.Path != "/data/main.cgi" {
			http.Error(out, "unexpected DataLink request", http.StatusBadRequest)
			return
		}
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			return
		}
		from, errFrom := time.ParseInLocation("20060102150405", r.Form.Get("startTime"), mst)
		until, errUntil := time.ParseInLocation("20060102150405", r.Form.Get("endTime"), mst)
		if errFrom != nil || errUntil != nil {
			http.Error(out, "unbounded DataLink export", http.StatusBadRequest)
			return
		}
		if until.Sub(from) > 24*time.Hour {
			fmt.Fprint(out, "Error: Selected period is greater than 24 hours.")
			return
		}
		if !canceled.Before(from) && !canceled.After(until) {
			fmt.Fprintf(out, "\"CANCELLATION\",\"945280\",\"0001\",\"%s\",\"%s\",\"%s\",\"N\"\n", sub, expires.UTC().Format(time.DateOnly), canceled.UTC().Format(time.DateOnly))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// dataLinkFeedFor is CCBill's DataLink holding one active membership sub,
// rebilled by txn at rebill.
func dataLinkFeedFor(t *testing.T, sub, txn string, rebill time.Time) *httptest.Server {
	t.Helper()
	mst := time.FixedZone("MST", -7*60*60)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.URL.Path != "/data/main.cgi" {
			http.Error(out, "unexpected DataLink request", http.StatusBadRequest)
			return
		}
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			fmt.Fprintf(out, "\"ACTIVEMEMBERS\",\"945280\",\"critical-flex\",\"%s\",\"%s\",\"member\",\"member@critical.invalid\",\"1\",\"%s\",\"\"\n",
				sub, rebill.Add(-720*time.Hour).Format("2006-01-02"), rebill.Add(720*time.Hour).Format("2006-01-02"))
			return
		}
		from, errFrom := time.ParseInLocation("20060102150405", r.Form.Get("startTime"), mst)
		until, errUntil := time.ParseInLocation("20060102150405", r.Form.Get("endTime"), mst)
		if errFrom != nil || errUntil != nil {
			http.Error(out, "unbounded DataLink export", http.StatusBadRequest)
			return
		}
		if until.Sub(from) > 24*time.Hour {
			fmt.Fprint(out, "Error: Selected period is greater than 24 hours.")
			return
		}
		if !rebill.Before(from) && !rebill.After(until) {
			fmt.Fprintf(out, "REBILL,945280,0001,%s,%s,%s,10.00\n", sub, rebill.UTC().Format("2006-01-02 15:04:05"), txn)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
