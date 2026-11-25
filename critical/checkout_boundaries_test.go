//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestCheckoutBoundaries(t *testing.T) {
	testNMISaleBoundaries(t)
	t.Run("OR-J09-S02/customer-challenge-settles-only-after-confirmation", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "auth")
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "challenged-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var waiting billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.Equal(t, billing.InvoicePaymentRequiresAction, waiting.Payment.Status)
		require.NotNil(t, waiting.Payment.NextAction)
		require.Empty(t, p.effects(t))
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		calls := p.core.Submitted("/v1/payment_intents")
		require.Len(t, calls, 1)
		// Authenticate is the external bank's challenge completion; only the
		// following public confirmation can admit local access and settlement.
		intent, _, ok := strings.Cut(waiting.Payment.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok, "the challenge must carry the provider's client capability")
		require.True(t, p.core.Authenticate(intent))
		var completed billing.Invoice
		for range 2 {
			status, raw = a.request(t, http.MethodPost, "/v1/me/invoices/"+waiting.ID.String()+"/confirm", a.aliceToken, map[string]any{})
			require.Equal(t, http.StatusOK, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &completed))
			require.Equal(t, billing.InvoiceComplete, completed.Status)
			require.Equal(t, waiting.ID, completed.ID)
		}
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
		payment, err := a.alpha.GetPayment(t.Context(), *completed.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: payment.CustomerID, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["sale:owned"])
	})
	t.Run("OR-J09-S03/definitive-decline-keeps-invoice-unpaid", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "insufficient_funds")
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "declined-purchase"})
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		invoices, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, invoices.Items, 1)
		require.NotEqual(t, billing.InvoiceComplete, invoices.Items[0].Status)
		require.NotEqual(t, billing.InvoicePaymentProcessing, invoices.Items[0].Payment.Status)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		for _, payment := range payments.Items {
			require.NotEqual(t, billing.PaymentSucceeded, payment.Status)
		}
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		require.Empty(t, p.effects(t))
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment.settled", "invoice.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)

		// On NMI with a ZIP-only checkout: a plain decline is reported as one,
		// another price of the product is the one paid, and the declines raise
		// no operator finding.
		nmi, journal := newNMIWorld(t)
		pass, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-pass", DisplayName: "NMI pass", Entitlements: []string{"pass:owned"}})
		require.NoError(t, err)
		cheap, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pass.ID, Key: "nmi-pass-test", UnitAmount: 500_000, Currency: "USD"})
		require.NoError(t, err)
		dear, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pass.ID, Key: "nmi-pass", UnitAmount: 23_000_000, Currency: "USD"})
		require.NoError(t, err)
		// As BuyButton buys: the customer's invoice of the lines, then its payment.
		buy := func(price billing.PriceID, total int64, card nmimock.Card, key string) (int, billing.Invoice) {
			t.Helper()
			status, raw := nmi.request(t, http.MethodPost, "/v1/me/invoices", nmi.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}})
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
			var invoice billing.Invoice
			require.NoError(t, json.Unmarshal(raw, &invoice))
			pay := billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: journal.core.Tokenize(card), BillingDetails: billedTo("US", "10001")}, ExpectedTotal: total, Reusable: ptr(false)}
			status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", nmi.aliceToken, pay, map[string]string{"Idempotency-Key": key})
			require.Contains(t, []int{http.StatusOK, http.StatusPaymentRequired}, status, string(raw))
			current, err := nmi.alpha.GetInvoice(t.Context(), invoice.ID)
			require.NoError(t, err)
			return status, *current
		}
		declined := nmimock.Card{Brand: "visa", Last4: "1111", CVV: "M", Decline: "200"}
		declined.AVS = "Z" // the ZIP matched; no street was sent
		_, first := buy(cheap.ID, 500_000, declined, "nmi-decline-zip-matched")
		require.Equal(t, billing.DeclineGeneric, first.Payment.LastPaymentError.Reason, "a matched ZIP is no address fault")
		tried, err := nmi.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{InvoiceID: first.ID})
		require.NoError(t, err)
		require.Len(t, tried.Items, 1)
		require.NotNil(t, tried.Items[0].Card, "the declined new card is named from NMI's report")
		require.Equal(t, "visa", *tried.Items[0].Card.Brand)
		require.Equal(t, "1111", *tried.Items[0].Card.Last4)
		declined.AVS = "N"
		_, again := buy(cheap.ID, 500_000, declined, "nmi-decline-zip-unmatched")
		require.Equal(t, first.ID, again.ID, "the same price pays the same invoice")
		require.Equal(t, billing.DeclineIncorrectZip, again.Payment.LastPaymentError.Reason, "an unmatched ZIP is named")
		// The preview agrees: the customer's unpaid invoice is no conflict for
		// another price, but still one for the merchant, who replaces nothing.
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices/preview", nmi.aliceToken, billing.PreviewMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: dear.ID}}})
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.InvoicePreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.Nil(t, preview.Lines[0].Refusal, "the unpaid invoice of the other price is replaced")
		require.EqualValues(t, 23_000_000, preview.Total)
		require.NotEmpty(t, preview.PaymentOptions)
		aliceID := billing.CustomerID(uuid.MustParse(nmi.alice.ID))
		merchantPreview, err := nmi.alphaRemote.PreviewInvoice(t.Context(), billing.PreviewInvoiceParams{CustomerID: aliceID, Lines: []billing.InvoiceLineParams{{PriceID: dear.ID}}})
		require.NoError(t, err)
		require.NotNil(t, merchantPreview.Lines[0].Refusal)
		require.Equal(t, "already_owned", merchantPreview.Lines[0].Refusal.Code)
		// An invoice the merchant made is not the customer's to replace.
		extra, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-extra", DisplayName: "NMI extra", Entitlements: []string{"extra:owned"}})
		require.NoError(t, err)
		extraA, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: extra.ID, Key: "nmi-extra-a", UnitAmount: 500_000, Currency: "USD"})
		require.NoError(t, err)
		extraB, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: extra.ID, Key: "nmi-extra-b", UnitAmount: 900_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = nmi.alphaRemote.CreateInvoice(t.Context(), "nmi-merchant-extra", billing.CreateInvoiceParams{CustomerID: aliceID, Lines: []billing.InvoiceLineParams{{PriceID: extraA.ID}}})
		require.NoError(t, err)
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices/preview", nmi.aliceToken, billing.PreviewMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: extraB.ID}}})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.NotNil(t, preview.Lines[0].Refusal, "the merchant's invoice still holds it")
		require.Equal(t, "already_owned", preview.Lines[0].Refusal.Code)
		// Another price, by BuyButton's two calls, then by one: each pays the
		// price chosen and releases the unpaid invoice of the other.
		_, other := buy(dear.ID, 23_000_000, declined, "nmi-other-price")
		require.NotEqual(t, first.ID, other.ID)
		require.EqualValues(t, 23_000_000, other.Total)
		replaced, err := nmi.alpha.GetInvoice(t.Context(), first.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceCanceled, replaced.Status, "the unpaid invoice of the other price is released")
		in := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: cheap.ID}}, ExpectedTotal: ptr(int64(500_000)), Reusable: ptr(false),
			Payment: &billing.InvoicePaymentParams{Token: journal.core.Tokenize(nmimock.Card{Brand: "mastercard", Last4: "5454", AVS: "Z", CVV: "M"}), BillingDetails: billedTo("US", "10001")}}
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices", nmi.aliceToken, in, map[string]string{"Idempotency-Key": "nmi-one-call"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var paid billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.InvoiceComplete, paid.Status)
		require.EqualValues(t, 500_000, paid.Total, "the customer pays the price they chose")
		require.Equal(t, "0.50", journal.core.LastSale().Amount)
		replaced, err = nmi.alpha.GetInvoice(t.Context(), other.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceCanceled, replaced.Status)
		fleet := hostFleet(t, nmi.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, nmi.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		abandoned, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.provider_intent.abandoned"})
		require.NoError(t, err)
		require.Empty(t, abandoned.Items, "a customer leaving a declined card is no operator incident")

	})
	t.Run("OR-J09-S05/lost-sale-answer-settles-from-the-provider", func(t *testing.T) {
		// A sale whose answer is lost waits for NMI's own record: a decline
		// there reopens the invoice for another card, and a charge NMI can't yet
		// show stays unknown as an operator finding until its receipt appears.
		nmi, journal := newNMIWorld(t)
		fleet := hostFleet(t, nmi.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, nmi.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		lost, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-lost", DisplayName: "NMI lost", Entitlements: []string{"lost:owned"}})
		require.NoError(t, err)
		lostPrice, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: lost.ID, Key: "nmi-lost", UnitAmount: 700_000, Currency: "USD"})
		require.NoError(t, err)
		settle := func(invoice billing.InvoiceID, done func(billing.Invoice) bool) billing.Invoice {
			t.Helper()
			nmi.clock.Advance(6 * time.Minute)
			var current *billing.Invoice
			require.Eventually(t, func() bool {
				wakeJobs(t, fleet, "openrails.provider_operation")
				current, err = nmi.alpha.GetInvoice(t.Context(), invoice)
				return err == nil && done(*current)
			}, 20*time.Second, 50*time.Millisecond)
			return *current
		}
		unresolved := func(invoice billing.Invoice) []billing.Finding {
			t.Helper()
			findings, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.sale.unresolved"})
			require.NoError(t, err)
			var out []billing.Finding
			for _, f := range findings.Items {
				if f.Evidence["invoice_id"] == invoice.ID.String() {
					out = append(out, f)
				}
			}
			return out
		}
		buy := func(price billing.PriceID, total int64, card nmimock.Card, key string) (int, billing.Invoice) {
			t.Helper()
			status, raw := nmi.request(t, http.MethodPost, "/v1/me/invoices", nmi.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}})
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
			var invoice billing.Invoice
			require.NoError(t, json.Unmarshal(raw, &invoice))
			pay := billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: journal.core.Tokenize(card), BillingDetails: billedTo("US", "10001")}, ExpectedTotal: total, Reusable: ptr(false)}
			status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", nmi.aliceToken, pay, map[string]string{"Idempotency-Key": key})
			require.Equal(t, http.StatusOK, status, string(raw))
			current, err := nmi.alpha.GetInvoice(t.Context(), invoice.ID)
			require.NoError(t, err)
			return status, *current
		}
		declined := nmimock.Card{Brand: "visa", Last4: "1111", AVS: "Z", CVV: "M", Decline: "200"}
		journal.core.DropSaleResponses(1)
		status, waiting := buy(lostPrice.ID, 700_000, declined, "nmi-lost-decline")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, billing.InvoiceProcessing, waiting.Status)
		status, raw := nmi.request(t, http.MethodPost, "/v1/me/invoices", nmi.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: lostPrice.ID}}})
		require.Equal(t, http.StatusConflict, status, "the unknown sale still holds its invoice: %s", raw)
		reopened := settle(waiting.ID, func(o billing.Invoice) bool { return o.Status == billing.InvoiceOpen })
		require.NotNil(t, reopened.Payment.LastPaymentError)
		require.Equal(t, billing.DeclineGeneric, reopened.Payment.LastPaymentError.Reason)
		require.Empty(t, unresolved(waiting), "NMI's decline settles the sale; nobody has to")
		journal.core.DropSaleResponses(1)
		journal.core.HideSales(1)
		status, waiting = buy(lostPrice.ID, 700_000, nmimock.Card{Brand: "visa", Last4: "4242", AVS: "Z", CVV: "M"}, "nmi-lost-charge")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, waiting.ID, reopened.ID, "the declined invoice is paid again")
		require.Equal(t, billing.InvoiceProcessing, waiting.Status)
		settle(waiting.ID, func(o billing.Invoice) bool { return len(unresolved(o)) == 1 })
		raised := unresolved(waiting)[0]
		require.Equal(t, "high", raised.Severity)
		require.Equal(t, billing.FindingStatus("requires_review"), raised.Status)
		require.Contains(t, *raised.RecommendedAction, "openrails intents resolve")
		current, err := nmi.alpha.GetInvoice(t.Context(), waiting.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceProcessing, current.Status, "a charge NMI can't show is never assumed absent")
		journal.core.Reveal()
		settle(waiting.ID, func(o billing.Invoice) bool { return o.Status == billing.InvoiceComplete })
		require.Empty(t, unresolved(waiting), "the receipt closes the finding")
		closed, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.sale.unresolved", Status: "fixed"})
		require.NoError(t, err)
		require.Len(t, closed.Items, 1)
		require.Equal(t, raised.ID, closed.Items[0].ID)
		// An answered sale NMI has yet to index waits out the delay quietly.
		late, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-late", DisplayName: "NMI late", Entitlements: []string{"late:owned"}})
		require.NoError(t, err)
		latePrice, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: late.ID, Key: "nmi-late", UnitAmount: 700_000, Currency: "USD"})
		require.NoError(t, err)
		journal.core.HideSales(1)
		status, waiting = buy(latePrice.ID, 700_000, nmimock.Card{Brand: "visa", Last4: "4242", AVS: "Z", CVV: "M"}, "nmi-late-receipt")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, billing.InvoiceProcessing, waiting.Status)
		wakeJobs(t, fleet, "openrails.provider_operation")
		require.Never(t, func() bool { return len(unresolved(waiting)) > 0 }, time.Second, 100*time.Millisecond, "no finding inside the observation delay")
		journal.core.Reveal()
		settle(waiting.ID, func(o billing.Invoice) bool { return o.Status == billing.InvoiceComplete })
		require.Empty(t, unresolved(waiting))
		journal.money(t, 2, 140) // neither sale is ever resent
	})
	t.Run("OR-J09-S06/partial-approval-never-pays-the-invoice", func(t *testing.T) {
		// An issuer approving part of a sale is no payment: NMI is asked to
		// settle it only in full, the invoice stays unpaid, an operator finding
		// stands, and the sale is never resent.
		nmi, journal := newNMIWorld(t)
		fleet := hostFleet(t, nmi.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, nmi.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		product, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-partial", DisplayName: "NMI partial", Entitlements: []string{"partial:owned"}})
		require.NoError(t, err)
		price, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "nmi-partial", UnitAmount: 700_000, Currency: "USD"})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(nmi.alice.ID))
		journal.core.ApprovePartially("4242", "0.30")
		status, raw := nmi.request(t, http.MethodPost, "/v1/me/invoices", nmi.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		pay := billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", AVS: "Z", CVV: "M"}), BillingDetails: billedTo("US", "10001")}, ExpectedTotal: 700_000, Reusable: ptr(false)}
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", nmi.aliceToken, pay, map[string]string{"Idempotency-Key": "nmi-partial-approval"})
		require.Equal(t, http.StatusOK, status, string(raw))
		unpaid := func() {
			t.Helper()
			current, err := nmi.alpha.GetInvoice(t.Context(), invoice.ID)
			require.NoError(t, err)
			require.Equal(t, billing.InvoiceProcessing, current.Status, "a partial approval is neither a payment nor a decline")
			payments, err := nmi.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
			require.NoError(t, err)
			for _, payment := range payments.Items {
				require.NotEqual(t, billing.PaymentSucceeded, payment.Status)
			}
			access, err := nmi.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"partial:owned"}})
			require.NoError(t, err)
			require.False(t, access.Entitlements["partial:owned"])
		}
		unpaid()
		attempts := journal.core.Attempts()
		require.Len(t, attempts, 1)
		require.Equal(t, "payment_in_full", attempts[0].Get("partial_payments"), "a partial approval must never settle by itself")
		require.Equal(t, "0.40", journal.core.LastSale().PartialBalance, "the issuer approved part of the sale")
		nmi.clock.Advance(6 * time.Minute)
		var raised []billing.Finding
		require.Eventually(t, func() bool {
			wakeJobs(t, fleet, "openrails.provider_operation")
			findings, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.sale.unresolved"})
			if err != nil {
				return false
			}
			raised = raised[:0]
			for _, f := range findings.Items {
				if f.Evidence["invoice_id"] == invoice.ID.String() {
					raised = append(raised, f)
				}
			}
			return len(raised) == 1
		}, 20*time.Second, 50*time.Millisecond)
		require.Equal(t, "high", raised[0].Severity)
		require.Contains(t, raised[0].Evidence["reason"], "approved for part of its amount")
		unpaid()
		require.Len(t, journal.core.Attempts(), 1, "the partial sale is never resent")
		journal.money(t, 0, 0) // a sale held for payment in full moves no money
	})
	t.Run("OR-J10-S02/lost-client-response-replays-original-committed-result", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		origin := a.server
		wire := http.DefaultTransport.(*http.Transport).Clone()
		defer wire.CloseIdleConnections()
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forward, err := http.NewRequestWithContext(r.Context(), r.Method, origin.URL+r.URL.RequestURI(), r.Body)
			if err != nil {
				http.Error(w, "fixture forward", 500)
				return
			}
			forward.Header = r.Header.Clone()
			response, err := wire.RoundTrip(forward)
			if err != nil {
				http.Error(w, "fixture upstream", 502)
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}))
		defer proxy.Close()
		a.server = proxy
		_, _, err := a.do(t.Context(), http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "lost-client-answer"})
		a.server = origin
		require.Error(t, err, "the client must actually lose the committed answer")
		invoices, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, invoices.Items, 1)
		require.Equal(t, billing.InvoiceComplete, invoices.Items[0].Status)
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "lost-client-answer"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var replay billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, invoices.Items[0].ID, replay.ID)
		require.Equal(t, invoices.Items[0].Payment.PaymentID, replay.Payment.PaymentID)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
	})
}

type nmiCheckoutWire func(*http.Request) (*http.Response, error)

func (f nmiCheckoutWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nmiSaleRequest(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/transact.php") }

func nmiCheckoutInvoice(t *testing.T, a *authorityWorld, key string, recurring bool, ttl time.Duration) billing.Invoice {
	t.Helper()
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: catalog.OwnershipUnique, Entitlements: []string{key + ":owned"}})
	require.NoError(t, err)
	params := billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: 5_000_000, Currency: "USD"}
	if recurring {
		params.BillingIntervalMonths = ptr(1)
	}
	price, err := a.alpha.CreatePrice(t.Context(), params)
	require.NoError(t, err)
	expires := a.clock.Now().Add(ttl)
	invoice, err := a.alpha.CreateInvoice(t.Context(), key, billing.CreateInvoiceParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpiresAt: &expires})
	require.NoError(t, err)
	return *invoice
}

func payNMICheckout(t *testing.T, a *authorityWorld, invoice billing.Invoice, token string, save bool) billing.Invoice {
	t.Helper()
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, billing.PayInvoiceParams{ExpectedTotal: invoice.Total, Payment: billing.InvoicePaymentParams{Token: token, BillingDetails: billedTo("US", "10001")}, Reusable: ptr(save)}, map[string]string{"Idempotency-Key": invoice.ID.String()})
	require.Contains(t, []int{http.StatusOK, http.StatusPaymentRequired}, status, string(raw))
	current, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
	require.NoError(t, err)
	return *current
}

func nmiCheckoutOperation(t *testing.T, a *authorityWorld, invoice billing.Invoice, verify bool) gen.BillingProviderIntent {
	t.Helper()
	rt := embedded.Graph(a.engine).Runtime
	var row gen.BillingProviderIntent
	ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
	require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var id uuid.UUID
		err := a.pool.QueryRow(ctx, "SELECT id FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE payload->>'order_id'=$1", invoice.ID.UUID().String()).Scan(&id)
		if err != nil {
			return err
		}
		if verify {
			row, err = rt.IntentRunner().VerifyByID(ctx, id)
		} else {
			row, err = intents.NewStore(rt.DB).Get(ctx, id)
		}
		return err
	}))
	return row
}

func testNMISaleBoundaries(t *testing.T) {
	t.Run("OR-J09-S71/gateway-refusal-reopens-the-invoice", func(t *testing.T) {
		a, p := newNMIWorld(t)
		invoice := nmiCheckoutInvoice(t, a, "refusal", false, 48*time.Hour)
		invoice = payNMICheckout(t, a, invoice, "invalid-payment-token", true)
		require.Equal(t, billing.InvoiceOpen, invoice.Status)
		require.NotNil(t, invoice.Payment.LastPaymentError)
		require.Equal(t, billing.DeclineProcessingError, invoice.Payment.LastPaymentError.Reason)
		row := nmiCheckoutOperation(t, a, invoice, false)
		require.Equal(t, intents.StatusFailedTerminal, row.Status)
		var proof map[string]any
		require.NoError(t, json.Unmarshal(row.ResultEvidence, &proof))
		require.Equal(t, true, proof["declined"])
		require.EqualValues(t, 300, proof["response_code"])
		attempts, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, attempts.Items, 1)
		require.Empty(t, p.core.Ledger(""))
		p.money(t, 0, 0)
	})
	t.Run("OR-J09-S72/never-connected-sale-reopens-the-invoice", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("closed server received request") }))
		target := closed.URL
		closed.Close()
		a, p := newNMIWorld(t, func(w *world) {
			original := w.deps.NMITransport
			w.deps.NMITransport = nmiCheckoutWire(func(r *http.Request) (*http.Response, error) {
				if nmiSaleRequest(r) {
					copy := r.Clone(r.Context())
					copy.URL, _ = url.Parse(target)
					return http.DefaultTransport.RoundTrip(copy)
				}
				return original.RoundTrip(r)
			})
		})
		invoice := nmiCheckoutInvoice(t, a, "unsent", false, 48*time.Hour)
		invoice = payNMICheckout(t, a, invoice, p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), false)
		require.Equal(t, billing.InvoiceOpen, invoice.Status)
		row := nmiCheckoutOperation(t, a, invoice, false)
		require.Equal(t, intents.StatusFailedTerminal, row.Status)
		require.Contains(t, string(row.ResultEvidence), `"request_refused": true`)
		require.Empty(t, p.core.Attempts())
		p.money(t, 0, 0)
	})
	t.Run("OR-J09-S73/disconnected-client-keeps-the-sale-running", func(t *testing.T) {
		a, p := newNMIWorld(t)
		invoice := nmiCheckoutInvoice(t, a, "disconnected", false, 48*time.Hour)
		hold := p.core.Hold(nmiSaleRequest, nmimock.HoldResponse)
		defer hold.Release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, _, err := a.do(ctx, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, billing.PayInvoiceParams{ExpectedTotal: invoice.Total, Payment: billing.InvoicePaymentParams{Token: p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), BillingDetails: billedTo("US", "10001")}, Reusable: ptr(false)}, map[string]string{"Idempotency-Key": "disconnect"})
			done <- err
		}()
		select {
		case <-hold.Arrived():
		case <-time.After(5 * time.Second):
			t.Fatal("sale did not arrive")
		}
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		time.Sleep(100 * time.Millisecond)
		hold.Release()
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
			return err == nil && current.Status == billing.InvoiceComplete
		}, 5*time.Second, 20*time.Millisecond)
		require.Equal(t, intents.StatusSucceeded, nmiCheckoutOperation(t, a, invoice, false).Status)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1)
		require.Len(t, p.core.Attempts(), 1)
		p.money(t, 1, 500)
	})
	t.Run("OR-J09-S74/empty-order-releases-after-a-day-and-expires", func(t *testing.T) {
		a, p := newNMIWorld(t)
		first := nmiCheckoutInvoice(t, a, "released", false, 48*time.Hour)
		expiring := nmiCheckoutInvoice(t, a, "released-expiring", false, time.Hour)
		p.core.FailRequests(nmiSaleRequest, http.StatusBadGateway, 2)
		first = payNMICheckout(t, a, first, p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), false)
		expiring = payNMICheckout(t, a, expiring, p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), false)
		a.clock.Advance(23 * time.Hour)
		require.Equal(t, intents.StatusUnknownNeedsVerify, nmiCheckoutOperation(t, a, first, true).Status)
		require.Equal(t, intents.StatusUnknownNeedsVerify, nmiCheckoutOperation(t, a, expiring, true).Status)
		rt := embedded.Graph(a.engine).Runtime
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error { _, err := rt.Invoices.ExpireDue(ctx, 100); return err }))
		current, err := a.alpha.GetInvoice(t.Context(), expiring.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceProcessing, current.Status)
		a.clock.Advance(time.Hour)
		p.core.QueryUnavailable(true)
		require.Equal(t, intents.StatusUnknownNeedsVerify, nmiCheckoutOperation(t, a, first, true).Status)
		p.core.QueryUnavailable(false)
		for _, invoice := range []billing.Invoice{first, expiring} {
			row := nmiCheckoutOperation(t, a, invoice, true)
			require.Equal(t, intents.StatusFailedTerminal, row.Status)
			var evidence map[string]any
			require.NoError(t, json.Unmarshal(row.ResultEvidence, &evidence))
			require.Equal(t, true, evidence["released_unconfirmed"])
			require.NotEmpty(t, evidence["submitted_at"])
			require.NotContains(t, evidence, "not_executed")
		}
		current, err = a.alpha.GetInvoice(t.Context(), first.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceOpen, current.Status)
		require.Equal(t, billing.DeclineProcessingError, current.Payment.LastPaymentError.Reason)
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/"+first.ID.String()+"/cancel", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: *first.Lines[0].PriceID}}})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error { _, err := rt.Invoices.ExpireDue(ctx, 100); return err }))
		current, err = a.alpha.GetInvoice(t.Context(), expiring.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceExpired, current.Status)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: payment.SaleReleasedFinding})
		require.NoError(t, err)
		require.Len(t, findings.Items, 2)
		require.Equal(t, "high", findings.Items[0].Severity)
		unresolved, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: payment.SaleUnresolvedFinding, Status: "fixed"})
		require.NoError(t, err)
		require.Len(t, unresolved.Items, 2)
		p.money(t, 0, 0)
	})
	t.Run("OR-J09-S75/operator-attests-an-empty-order-release", func(t *testing.T) {
		a, p := newNMIWorld(t)
		invoice := nmiCheckoutInvoice(t, a, "attested", false, 48*time.Hour)
		p.core.HideSales(1)
		invoice = payNMICheckout(t, a, invoice, p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), false)
		row := nmiCheckoutOperation(t, a, invoice, false)
		rt := embedded.Graph(a.engine).Runtime
		resolve := func(resolution intents.Resolution) error {
			return rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
				resolved, err := rt.IntentRunner().Resolve(ctx, row.ID, resolution)
				if err == nil {
					row = resolved
				}
				return err
			})
		}
		resolution := intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Checked the empty order and accept the risk of late money"}
		p.core.Reveal()
		require.Error(t, resolve(resolution))
		p.core.QueryUnavailable(true)
		require.Error(t, resolve(resolution))
		p.core.QueryUnavailable(false)
		p.core.EditSale(p.core.LastSale().TransactionID, func(s *nmimock.Sale) { s.Hidden = true })
		require.Error(t, resolve(intents.Resolution{NotExecuted: true, Reason: "missing actor"}))
		require.NoError(t, resolve(resolution))
		require.Equal(t, intents.StatusFailedTerminal, row.Status)
		var evidence map[string]any
		require.NoError(t, json.Unmarshal(row.ResultEvidence, &evidence))
		require.Equal(t, true, evidence["released_unconfirmed"])
		require.Equal(t, "operator", evidence["operator_resolution"].(map[string]any)["actor"])
		current, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, billing.InvoiceOpen, current.Status)
		require.True(t, errors.Is(resolve(intents.Resolution{ProviderReference: p.core.LastSale().TransactionID, Actor: "operator", Reason: "late receipt"}), intents.ErrResolutionNotUnknown))
		p.money(t, 1, 500)
	})
	t.Run("OR-J09-S76/vault-read-failure-keeps-paid-access-and-retries-storage", func(t *testing.T) {
		for _, recurring := range []bool{false, true} {
			a, p := newNMIWorld(t)
			invoice := nmiCheckoutInvoice(t, a, "card-storage", recurring, 48*time.Hour)
			storageDown := true
			p.core.Intercept(func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/customers/")
			}, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
				if !storageDown {
					return serve(), nil
				}
				response := httptest.NewRecorder()
				response.WriteHeader(http.StatusServiceUnavailable)
				return response.Result(), nil
			})
			invoice = payNMICheckout(t, a, invoice, p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), true)
			require.Equal(t, billing.InvoiceComplete, invoice.Status)
			require.Nil(t, invoice.Payment.PaymentMethodID)
			require.Equal(t, intents.StatusUnknownNeedsVerify, nmiCheckoutOperation(t, a, invoice, false).Status)
			customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
			access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"card-storage:owned"}})
			require.NoError(t, err)
			require.True(t, access.Entitlements["card-storage:owned"])
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: payment.SaleCardNotSavedFinding})
			require.NoError(t, err)
			require.Len(t, findings.Items, 1)
			finding := findings.Items[0]
			require.Equal(t, customer.UUID().String(), finding.Evidence["customer_id"])
			if recurring {
				require.NotEmpty(t, finding.Evidence["subscription_id"])
			}
			a.clock.Advance(6 * time.Minute)
			require.Equal(t, intents.StatusUnknownNeedsVerify, nmiCheckoutOperation(t, a, invoice, true).Status)
			again, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: payment.SaleCardNotSavedFinding})
			require.NoError(t, err)
			require.Len(t, again.Items, 1)
			require.Equal(t, finding.ID, again.Items[0].ID)
			storageDown = false
			a.clock.Advance(6 * time.Minute)
			require.Equal(t, intents.StatusSucceeded, nmiCheckoutOperation(t, a, invoice, true).Status)
			require.Equal(t, intents.StatusSucceeded, nmiCheckoutOperation(t, a, invoice, true).Status)
			payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
			require.NoError(t, err)
			require.Len(t, payments.Items, 1)
			require.Len(t, p.core.Attempts(), 1)
			methods, err := a.alpha.ListPaymentMethods(t.Context(), customer, billing.PaymentMethodListParams{})
			require.NoError(t, err)
			require.Len(t, methods.Items, 1)
			current, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
			require.NoError(t, err)
			require.Equal(t, &methods.Items[0].ID, current.Payment.PaymentMethodID)
			fixed, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: payment.SaleCardNotSavedFinding, Status: "fixed"})
			require.NoError(t, err)
			require.Len(t, fixed.Items, 1)
			require.Equal(t, finding.ID, fixed.Items[0].ID)
			if recurring {
				subID, err := billing.ParseSubscriptionID(finding.Evidence["subscription_id"].(string))
				require.NoError(t, err)
				sub, err := a.alpha.GetSubscription(t.Context(), subID)
				require.NoError(t, err)
				require.NotNil(t, sub.PaymentMethodID)
			}
			p.money(t, 1, 500)
		}
	})
}
