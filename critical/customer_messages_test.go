//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/helpers/smtp"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

const receiptEvent = "one_off_purchase_completed"

func TestCustomerMessages(t *testing.T) {
	t.Run("OR-J39-S01/a-paid-order-sends-one-receipt-across-retries", func(t *testing.T) {
		mail := &alertMailbox{refuse: 1}
		a, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		order, err := a.alpha.CreateOrder(t.Context(), "receipt-order", billing.CreateOrderParams{CustomerID: alice, Lines: messageSale(t, a)})
		require.NoError(t, err)
		paid := billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 10_000_000, TransactionID: "receipt-bank-transfer"}
		first, err := a.alpha.RecordExternalPayment(t.Context(), paid)
		require.NoError(t, err)
		replay, err := a.alpha.RecordExternalPayment(t.Context(), paid)
		require.NoError(t, err)
		require.Equal(t, first.ID, replay.ID, "a replayed settlement is the same payment and the same fact")
		fleet := startFleet(t, a.world)
		require.Eventually(t, func() bool { return len(mail.to(a.alice.Email)) == 1 }, 30*time.Second, 20*time.Millisecond,
			"the job retries the send the mailbox refused")
		require.Equal(t, 1, mail.refusals())
		runRegistered(t, fleet, "openrails.customer_messages_dispatch", 2)
		messagesSettled(t, fleet)
		sent := mail.to(a.alice.Email)
		require.Len(t, sent, 1, "one receipt, however often its fact and job repeat")
		require.Equal(t, "Your receipt from Alpha", sent[0].Subject)
		require.Equal(t, "Alpha", sent[0].From.Name)
		require.Contains(t, sent[0].Text, "Amount paid: 10.00 USD")
		log := settledMessages(t, a.world, alice, 1)
		require.Equal(t, receiptEvent, log[0].EventType)
		require.Equal(t, billing.MessageSent, log[0].Status)
		require.Equal(t, 2, log[0].Attempts)
		require.Equal(t, a.alice.Email, deref(log[0].Recipient))
		require.Nil(t, log[0].LastError)
	})
	t.Run("OR-J39-S02/a-billing-restore-sends-nothing", func(t *testing.T) {
		mail := &alertMailbox{}
		source, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		alice := billing.CustomerID(uuid.MustParse(source.alice.ID))
		paidExternally(t, source, alice, messageSale(t, source), "archived-receipt")
		fleet := startFleet(t, source.world)
		require.Eventually(t, func() bool { return len(mail.to(source.alice.Email)) == 1 }, 30*time.Second, 20*time.Millisecond)
		messagesSettled(t, fleet)
		archive := archiveBytes(t, source.alpha)
		restoredMail := &alertMailbox{}
		target, client := archiveTarget(t, source.world, func(w *world) { w.deps.Email = restoredMail })
		_, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		payments, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: alice})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "the paid order moved with the book")
		targetFleet := startFleet(t, target)
		runRegistered(t, targetFleet, "openrails.customer_messages_dispatch", 1)
		messagesSettled(t, targetFleet)
		require.Empty(t, restoredMail.all(), "a restore sends nothing again")
		log, err := customerMessages(t, client, alice)
		require.NoError(t, err)
		require.Empty(t, log, "a restore queues no message")
	})
	t.Run("OR-J39-S03/a-customer-without-an-address-is-no-contact-and-the-fact-is-emitted", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		stranger := billing.CustomerID(uuid.New())
		paidExternally(t, a, stranger, messageSale(t, a), "stranger-receipt")
		startFleet(t, a.world)
		log := settledMessages(t, a.world, stranger, 1)
		require.Equal(t, billing.MessageNoContact, log[0].Status)
		require.Nil(t, log[0].Recipient)
		require.Zero(t, log[0].Attempts)
		require.Empty(t, mail.all())
		require.True(t, factEmitted(t, a.alpha, receiptEvent, stranger), "the event is emitted whether or not a message is sent")
	})
	t.Run("OR-J39-S04/a-fact-on-a-test-psp-is-test-environment-and-the-fact-is-emitted", func(t *testing.T) {
		mail := &alertMailbox{}
		a, p := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, saleInput(t, a, p), map[string]string{"Idempotency-Key": "test-psp-receipt"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Len(t, p.effects(t), 1, "the test PSP took the payment")
		startFleet(t, a.world)
		log := settledMessages(t, a.world, alice, 1)
		require.Equal(t, receiptEvent, log[0].EventType)
		require.Equal(t, billing.MessageTestEnvironment, log[0].Status)
		require.Nil(t, log[0].Recipient)
		require.Empty(t, mail.all(), "a test PSP's facts send no automatic email")
		require.True(t, factEmitted(t, a.alpha, receiptEvent, alice))
	})
	t.Run("OR-J39-S05/messages-past-the-hourly-cap-are-rate-limited-with-a-finding", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		lines := messageSale(t, a)
		for i := range 11 {
			paidExternally(t, a, alice, lines, fmt.Sprintf("capped-%02d", i))
		}
		startFleet(t, a.world)
		log := settledMessages(t, a.world, alice, 11)
		statuses := map[billing.MessageStatus]int{}
		for _, m := range log {
			statuses[m.Status]++
		}
		require.Equal(t, map[billing.MessageStatus]int{billing.MessageSent: 10, billing.MessageRateLimited: 1}, statuses)
		require.Len(t, mail.to(a.alice.Email), 10)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.customer.messages_rate_limited"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, alice.String(), findings.Items[0].SubjectKey)
		require.Equal(t, billing.FindingStatus("requires_review"), findings.Items[0].Status)
	})
	t.Run("OR-J39-S06/erasing-a-customer-erases-its-message-log", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			w.deps.Email = mail
		})
		provisioner, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "owned directory"})
		require.NoError(t, err)
		id := uuid.NewString()
		user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "erased-customer", "displayName": "Erased Customer",
			"emails": []any{map[string]any{"value": "erased@directory.invalid", "primary": true}}, "active": true}
		status, raw := a.request(t, http.MethodPost, "/v1/app/scim/v2/Users", provisioner.Token, user)
		require.Equal(t, http.StatusCreated, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(id))
		paidExternally(t, a, customer, messageSale(t, a), "erased-receipt")
		startFleet(t, a.world)
		log := settledMessages(t, a.world, customer, 1)
		require.Equal(t, billing.MessageSent, log[0].Status)
		require.Len(t, mail.to("erased@directory.invalid"), 1)
		status, raw = a.request(t, http.MethodDelete, "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		log, err = customerMessages(t, a.alpha, customer)
		require.NoError(t, err)
		require.Empty(t, log, "erasure deletes the customer's message log")
	})
	t.Run("OR-J39-S07/a-merchant-template-replaces-the-built-in-message", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.Email = mail
			declareMessages(w, billing.MessageSettings{Events: map[string]billing.MessageEventSettings{receiptEvent: {Templates: map[string]billing.MessageTemplate{"en": {
				Subject: "Thanks for shopping at {{.Merchant.Name}}",
				Text:    "You paid {{.Event.Amount}} for {{.Event.ProductName}}.",
				HTML:    "<p>You paid <b>{{.Event.Amount}}</b> for {{.Event.ProductName}}.</p>",
			}}}}})
		})
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		paidExternally(t, a, alice, messageSale(t, a), "custom-receipt")
		startFleet(t, a.world)
		require.Equal(t, billing.MessageSent, settledMessages(t, a.world, alice, 1)[0].Status)
		sent := mail.to(a.alice.Email)
		require.Len(t, sent, 1)
		require.Equal(t, "Thanks for shopping at Alpha", sent[0].Subject)
		require.Contains(t, sent[0].Text, "You paid 10.00 USD for receipt-item.")
		require.NotContains(t, sent[0].Text, "Amount paid", "the merchant's body replaces the built-in one")
		require.True(t, strings.HasSuffix(sent[0].Text, "\nAlpha\n"), "the built-in layout still wraps it:\n%s", sent[0].Text)
		require.Contains(t, sent[0].HTML, "<p>You paid <b>10.00 USD</b> for receipt-item.</p>")
	})
	t.Run("OR-J39-S08/a-messages-document-whose-template-does-not-render-is-refused-on-load", func(t *testing.T) {
		mail := &alertMailbox{}
		off := false
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.Email = mail
			declareMessages(w, billing.MessageSettings{Events: map[string]billing.MessageEventSettings{
				receiptEvent:       {Templates: map[string]billing.MessageTemplate{"en": {Subject: "Receipt", Text: "Total: {{.Event.Total}}"}}},
				"refund_completed": {Enabled: &off},
			}})
		})
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.merchant_config.not_served"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, "messages", findings.Items[0].SubjectKey)
		reason := fmt.Sprint(findings.Items[0].Evidence["reason"])
		require.Contains(t, reason, "events."+receiptEvent+": text:", "the refusal names the event type")
		require.Contains(t, reason, "can't evaluate field Total", "and the error")
		// Nothing of the refused document is served: the receipt is the
		// built-in one.
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		paidExternally(t, a, alice, messageSale(t, a), "refused-document-receipt")
		startFleet(t, a.world)
		require.Equal(t, billing.MessageSent, settledMessages(t, a.world, alice, 1)[0].Status)
		sent := mail.to(a.alice.Email)
		require.Len(t, sent, 1)
		require.Equal(t, "Your receipt from Alpha", sent[0].Subject)
		require.Contains(t, sent[0].Text, "Amount paid: 10.00 USD")
	})
	t.Run("OR-J39-S09/each-cancellation-cause-sends-one-confirmation-in-its-words", func(t *testing.T) {
		t.Run("the-customer-canceled", func(t *testing.T) {
			mail := &alertMailbox{}
			a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
			status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Moving on"})
			require.Equal(t, http.StatusOK, status, string(raw))
			liveMoney(t, a.world)
			startFleet(t, a.world)
			confirmation := sentAbout(t, a, mail, "subscription_canceled", "You canceled your subscription to Alpha")
			require.Contains(t, confirmation.Text, "This confirms that you canceled your subscription to Membership.")
			require.Contains(t, confirmation.Text, "You keep your access until ")
			require.Contains(t, confirmation.Text, "You won't be charged again.")
			require.Contains(t, confirmation.Text, "Changed your mind? You can resume it before it ends: https://alpha.example/billing")
		})
		t.Run("staff-canceled", func(t *testing.T) {
			mail := &alertMailbox{}
			a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
			_, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends future service"})
			require.NoError(t, err)
			liveMoney(t, a.world)
			startFleet(t, a.world)
			confirmation := sentAbout(t, a, mail, "subscription_canceled", "Your subscription to Alpha was canceled")
			require.Contains(t, confirmation.Text, "Your subscription to Membership was canceled by Alpha.")
			require.Contains(t, confirmation.Text, "You won't be charged again.")
			require.Contains(t, confirmation.Text, "If you didn't expect this, please contact us: https://alpha.example/help")
			require.NotContains(t, confirmation.Text, "resume")
		})
		t.Run("payments-failed", func(t *testing.T) {
			mail := &alertMailbox{}
			a, p, customer := usageBillingWorld(t, true, false, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
			p.core.SetDecline("4242", "252")
			oneUsage(t, a.alpha, usageInput(customer, "stolen-card-usage", 3))
			a.clock.Advance(time.Hour)
			// The collection's message waits for the dispatch below, once
			// the PSP is live.
			fleet := undispatchedFleet(t, a.world)
			runUsageDue(t, fleet)
			require.Eventually(t, func() bool {
				log, err := customerMessages(t, a.alpha, customer)
				return err == nil && len(ofType(log, "order_collection_stopped")) == 1
			}, 30*time.Second, 20*time.Millisecond, "the stopped collection queues its message")
			liveMoney(t, a.world)
			dispatchMessages(t, fleet)
			confirmation := sentAbout(t, a, mail, "order_collection_stopped", "We stopped charging your card for Alpha")
			require.Contains(t, confirmation.Text, "Your bank told us your card can no longer be charged, so we have stopped trying to collect the 30.00 USD you owe.")
			require.Contains(t, confirmation.Text, "Pay it or add another payment method: https://alpha.example/billing")
		})
	})
	t.Run("OR-J39-S10/a-scheduled-price-change-sends-its-notice", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
		target, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: "membership-week-repriced", UnitAmount: 12_000_000, Currency: "USD", BillingIntervalHours: ptr(7 * 24), AccessDurationHours: ptr(7 * 24)})
		require.NoError(t, err)
		migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, EffectiveAt: *sub.CurrentPeriodEndsAt, AcknowledgeShortNotice: true})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Scheduled)
		liveMoney(t, a.world)
		startFleet(t, a.world)
		notice := sentAbout(t, a, mail, "subscription_reprice_scheduled", "The price of your subscription to Alpha is changing")
		require.Contains(t, notice.Text, "The price of your Membership subscription is changing from 10.00 USD to 12.00 USD, starting "+sub.CurrentPeriodEndsAt.UTC().Format("January 2, 2006")+".")
		require.Contains(t, notice.Text, "If you'd rather not continue, you can cancel before then: https://alpha.example/billing")
		require.Contains(t, notice.HTML, `href="https://alpha.example/billing"`)
	})
	t.Run("OR-J39-S11/a-refund-sends-one-refund-receipt", func(t *testing.T) {
		t.Run("staff-refund", func(t *testing.T) {
			mail := &alertMailbox{}
			a, p := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
			status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, saleInput(t, a, p), map[string]string{"Idempotency-Key": "refunded-purchase"})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			require.NotNil(t, order.Payment.PaymentID)
			params := billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Customer asked"}
			first, err := a.alpha.RefundPayment(t.Context(), "receipt-refund", *order.Payment.PaymentID, params)
			require.NoError(t, err)
			require.Equal(t, billing.PaymentSucceeded, first.Status)
			replay, err := a.alpha.RefundPayment(t.Context(), "receipt-refund", *order.Payment.PaymentID, params)
			require.NoError(t, err)
			require.Equal(t, first.ID, replay.ID)
			liveMoney(t, a.world)
			startFleet(t, a.world)
			receipt := sentAbout(t, a, mail, "refund_completed", "Your refund from Alpha")
			require.Contains(t, receipt.Text, "We refunded 10.00 USD for Sale.")
			require.True(t, factEmitted(t, a.alpha, "refund_completed", customerOf(a.alice)))
		})
		t.Run("provider-reported-refund", func(t *testing.T) {
			mail := &alertMailbox{}
			a := ccbillWebhookWorld(t, func(w *world) { w.deps.Email = mail })
			price := ccbillMemberPrice(t, a.alpha)
			now := a.clock.Now().UTC().Truncate(time.Second)
			const ref, sale = "556677889900112244", "0812233445566778822"
			importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(customerOf(a.alice), price, ref, sale, now.Add(5*24*time.Hour)))
			for range 2 {
				status, body := ccbillNotice(t, a, "Refund", map[string]any{"subscriptionId": ref, "transactionId": sale,
					"amount": "3.00", "currencyCode": "840", "reason": "customer request", "timestamp": now.Add(-time.Hour).Format(time.DateTime)})
				require.Equal(t, http.StatusOK, status, body)
			}
			liveMoney(t, a.world)
			startFleet(t, a.world)
			receipt := sentAbout(t, a, mail, "refund_completed", "Your refund from Alpha")
			require.Contains(t, receipt.Text, "We refunded 3.00 USD")
		})
	})
	t.Run("OR-J39-S12/the-host-renderer-takes-precedence-for-the-messages-it-renders", func(t *testing.T) {
		mail := &alertMailbox{}
		var mu sync.Mutex
		var rendered []billing.MessageData
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.Email = mail
			declareMessages(w, billing.MessageSettings{Events: map[string]billing.MessageEventSettings{receiptEvent: {Templates: map[string]billing.MessageTemplate{"en": {
				Subject: "Merchant receipt", Text: "From the merchant's template.",
			}}}}})
			w.deps.MessageRenderer = func(_ context.Context, data billing.MessageData) (billing.MessageContent, bool, error) {
				mu.Lock()
				rendered = append(rendered, data)
				mu.Unlock()
				if data.Event.ProductName != "host-item" {
					return billing.MessageContent{}, false, nil
				}
				return billing.MessageContent{Subject: "Host receipt from " + data.Merchant.Name, Text: "The host rendered " + data.Event.Amount + "."}, true, nil
			}
		})
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		paidExternally(t, a, alice, []billing.OrderLineParams{{PriceID: hostProduct(t, a.alpha, "host-item").ID}}, "host-rendered")
		paidExternally(t, a, alice, messageSale(t, a), "template-rendered")
		startFleet(t, a.world)
		for _, m := range settledMessages(t, a.world, alice, 2) {
			require.Equal(t, billing.MessageSent, m.Status)
		}
		subjects := map[string]string{}
		for _, email := range mail.to(a.alice.Email) {
			subjects[email.Subject] = email.Text
		}
		require.Len(t, subjects, 2)
		require.Equal(t, "The host rendered 10.00 USD.", subjects["Host receipt from Alpha"])
		require.Contains(t, subjects["Merchant receipt"], "From the merchant's template.", "a message the host declines is the merchant's template")
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, rendered, 2)
		for _, data := range rendered {
			require.Equal(t, billing.MessageDataVersion, data.Version)
			require.Equal(t, receiptEvent, data.EventType)
			require.Equal(t, "en", data.Language)
			require.Equal(t, alice.String(), data.Customer.ID)
			require.Equal(t, "Alpha", data.Merchant.Name)
			require.Equal(t, "10.00 USD", data.Event.Amount)
		}
	})
	t.Run("OR-J39-S13/a-subscription-ended-by-failed-payments-sends-one-confirmation", func(t *testing.T) {
		mail := &alertMailbox{}
		a, p, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
		enableLifecycleMaintenance(t, a)
		// The purchase's messages end on the test PSP; the decline's wait for
		// the dispatch after it, once the PSP is live.
		fleet := undispatchedFleet(t, a.world)
		dispatchMessages(t, fleet)
		p.core.SetDecline("4242", "stolen_card")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "canceled")
		liveMoney(t, a.world)
		dispatchMessages(t, fleet)
		confirmation := sentAbout(t, a, mail, "premium_ended", "Your subscription to Alpha has ended")
		require.Contains(t, confirmation.Text, "Your bank told us your card can no longer be charged, so your subscription to Membership has ended.")
		require.Contains(t, confirmation.Text, "You won't be charged again.")
		require.Contains(t, confirmation.Text, "Subscribe again any time: https://alpha.example/billing")
		require.Len(t, mail.to(a.alice.Email), 1, "the ending is the one message")
		log, err := customerMessages(t, a.alpha, customerOf(a.alice))
		require.NoError(t, err)
		failed := ofType(log, "payment_method_failed")
		require.Len(t, failed, 1)
		require.Equal(t, billing.MessageDisabled, failed[0].Status, "the failure that ended it is confirmed by the ending")
	})
	t.Run("OR-J39-S14/a-staff-cancel-that-ends-access-now-sends-one-confirmation", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
		_, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "Terms violation", RevokeAccess: true})
		require.NoError(t, err)
		liveMoney(t, a.world)
		startFleet(t, a.world)
		confirmation := sentAbout(t, a, mail, "premium_ended", "Your subscription to Alpha was canceled")
		require.Contains(t, confirmation.Text, "Your subscription to Membership was canceled by Alpha.")
		require.Contains(t, confirmation.Text, "Your access ended on ")
		require.Contains(t, confirmation.Text, "You won't be charged again.")
		require.Contains(t, confirmation.Text, "If you didn't expect this, please contact us: https://alpha.example/help")
		log, err := customerMessages(t, a.alpha, customerOf(a.alice))
		require.NoError(t, err)
		require.Empty(t, ofType(log, "subscription_canceled"), "access ended with the cancel: one fact, one confirmation")
	})
	t.Run("OR-J39-S15/an-access-end-a-refund-caused-sends-no-confirmation-by-default", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
		a.clock.Advance(time.Hour)
		paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Len(t, paid.Items, 1)
		_, err = a.alpha.RefundPayment(t.Context(), "refund-and-end", paid.Items[0].ID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "Refund and end"})
		require.NoError(t, err)
		subscriptionState(t, a, sub.ID, "canceled")
		liveMoney(t, a.world)
		startFleet(t, a.world)
		receipt := sentAbout(t, a, mail, "refund_completed", "Your refund from Alpha")
		require.Contains(t, receipt.Text, "We refunded 10.00 USD")
		log, err := customerMessages(t, a.alpha, customerOf(a.alice))
		require.NoError(t, err)
		ends := ofType(log, "premium_ended")
		require.Len(t, ends, 1)
		require.Equal(t, billing.MessageDisabled, ends[0].Status, "a refund's access end sends nothing by default")
		require.Empty(t, ofType(log, "subscription_canceled"))
		for _, email := range mail.to(a.alice.Email) {
			require.NotContains(t, []string{"Your subscription to Alpha has ended", "Your subscription to Alpha was canceled", "Your subscription to Alpha is canceled"}, email.Subject)
		}
	})
	t.Run("OR-J39-S16/the-log-route-lists-a-customers-messages-with-their-statuses", func(t *testing.T) {
		mail := &alertMailbox{}
		a, p := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		alice := customerOf(a.alice)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, saleInput(t, a, p), map[string]string{"Idempotency-Key": "logged-on-test-psp"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		paidExternally(t, a, alice, messageSale(t, a), "logged-receipt")
		startFleet(t, a.world)
		settledMessages(t, a.world, alice, 2)
		first, err := a.alphaRemote.ListCustomerMessages(t.Context(), alice, billing.CustomerMessageListParams{PageRequest: billing.PageRequest{Limit: 1}})
		require.NoError(t, err)
		require.Len(t, first.Items, 1)
		require.NotEmpty(t, first.Next)
		second, err := a.alphaRemote.ListCustomerMessages(t.Context(), alice, billing.CustomerMessageListParams{PageRequest: billing.PageRequest{Limit: 1, Cursor: first.Next}})
		require.NoError(t, err)
		require.Len(t, second.Items, 1)
		require.Empty(t, second.Next)
		sent, test := first.Items[0], second.Items[0]
		require.Equal(t, receiptEvent, sent.EventType)
		require.Equal(t, billing.MessageEmail, sent.Channel)
		require.Equal(t, billing.MessageSent, sent.Status, "newest first")
		require.Equal(t, 1, sent.Attempts)
		require.Equal(t, a.alice.Email, deref(sent.Recipient))
		require.NotNil(t, sent.SentAt)
		require.Nil(t, sent.LastError)
		require.Equal(t, alice, sent.CustomerID)
		require.Equal(t, billing.MessageTestEnvironment, test.Status)
		require.Zero(t, test.Attempts)
		require.Nil(t, test.Recipient)
		require.Nil(t, test.SentAt)
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{receiptEvent}, Limit: 100})
		require.NoError(t, err)
		var facts []string
		for _, e := range events.Events {
			facts = append(facts, e.ID)
		}
		require.ElementsMatch(t, []string{sent.EventID.String(), test.EventID.String()}, facts, "each names its fact in the event log")
		inProcess, err := a.alpha.ListCustomerMessages(t.Context(), alice, billing.CustomerMessageListParams{})
		require.NoError(t, err)
		require.Equal(t, []billing.CustomerMessage{sent, test}, inProcess.Items, "the in-process client answers alike")
		other, err := a.betaRemote.ListCustomerMessages(t.Context(), alice, billing.CustomerMessageListParams{})
		require.NoError(t, err)
		require.Empty(t, other.Items, "another merchant's credential reads its own log")
	})
	t.Run("OR-J39-S17/a-preview-renders-a-candidate-template-and-refuses-an-invalid-one", func(t *testing.T) {
		a, _ := newPaymentWorld(t, func(w *world) {
			alphaProfile(w)
			declareMessages(w, billing.MessageSettings{Events: map[string]billing.MessageEventSettings{receiptEvent: {Templates: map[string]billing.MessageTemplate{"en": {
				Subject: "Saved receipt", Text: "The saved template.",
			}}}}})
		})
		candidate := &billing.MessageTemplate{
			Subject: "Receipt for {{.Customer.Name}} from {{.Merchant.Name}}",
			Text:    "You paid {{.Event.Amount}} for {{.Event.ProductName}}.\nManage it: {{.Links.ManageBilling}}",
		}
		preview, err := a.alphaRemote.PreviewMessage(t.Context(), billing.PreviewMessageParams{EventType: receiptEvent, Template: candidate})
		require.NoError(t, err)
		require.Equal(t, receiptEvent, preview.EventType)
		require.Equal(t, "en", preview.Language)
		require.Equal(t, "Receipt for Ada Lovelace from Alpha", preview.Subject)
		require.NotNil(t, preview.Text)
		require.Contains(t, *preview.Text, "You paid 12.50 USD for Field guide.\nManage it: https://alpha.example/billing")
		require.Nil(t, preview.HTML, "the candidate has no html body")
		saved, err := a.alpha.PreviewMessage(t.Context(), billing.PreviewMessageParams{EventType: receiptEvent})
		require.NoError(t, err)
		require.Equal(t, "Saved receipt", saved.Subject, "a preview saves nothing")
		_, err = a.alphaRemote.PreviewMessage(t.Context(), billing.PreviewMessageParams{EventType: receiptEvent, Template: &billing.MessageTemplate{Subject: "Receipt", Text: "Total: {{.Event.Total}}"}})
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, http.StatusBadRequest, refusal.Status)
		require.Equal(t, billing.CodeMessageTemplateInvalid, refusal.Code)
		require.Equal(t, "template", deref(refusal.Param))
		require.Contains(t, refusal.Message, "can't evaluate field Total")
		_, err = a.alpha.PreviewMessage(t.Context(), billing.PreviewMessageParams{EventType: "order.completed"})
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, billing.CodeInvalidParam, refusal.Code)
		require.Equal(t, "event_type", deref(refusal.Param))
	})
	t.Run("OR-J39-S18/a-test-message-goes-only-to-the-alert-address", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.Email = mail
			alphaProfile(w)
			m := cfgMerchant(w.cfg, "alpha")
			m.AlertEmail = ptr("ops@alpha.example")
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		sent, err := a.alphaRemote.CreateTestMessage(t.Context(), billing.CreateTestMessageParams{EventType: "refund_completed"})
		require.NoError(t, err)
		require.Equal(t, "ops@alpha.example", sent.Recipient)
		require.Equal(t, "Your refund from Alpha", sent.Message.Subject)
		emails := mail.all()
		require.Len(t, emails, 1)
		require.Equal(t, "ops@alpha.example", emails[0].To)
		require.Equal(t, "Your refund from Alpha", emails[0].Subject)
		require.Contains(t, emails[0].Text, "We refunded 12.50 USD for Field guide.")
		require.Equal(t, "Alpha", emails[0].From.Name)
		// The request has no address to name: another field is refused.
		status, raw := a.request(t, http.MethodPost, "/v1/merchant/messages/test", a.alphaKey.Secret, map[string]any{"event_type": "refund_completed", "to": a.alice.Email})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), string(billing.CodeUnknownField))
		require.Len(t, mail.all(), 1)
		require.Empty(t, mail.to(a.alice.Email))
		log, err := customerMessages(t, a.alpha, customerOf(a.alice))
		require.NoError(t, err)
		require.Empty(t, log, "a test is not a customer message")
		// A merchant with no alert address is refused.
		_, err = a.betaRemote.CreateTestMessage(t.Context(), billing.CreateTestMessageParams{EventType: "refund_completed"})
		var refusal *billing.StatusError
		require.True(t, errors.As(err, &refusal), "%v", err)
		require.Equal(t, http.StatusConflict, refusal.Status)
		require.Equal(t, billing.CodeAlertEmailRequired, refusal.Code)
		require.Len(t, mail.all(), 1)
		// A merchant sends at most ten tests an hour.
		for range 9 {
			_, err := a.alphaRemote.CreateTestMessage(t.Context(), billing.CreateTestMessageParams{EventType: receiptEvent})
			require.NoError(t, err)
		}
		_, err = a.alphaRemote.CreateTestMessage(t.Context(), billing.CreateTestMessageParams{EventType: receiptEvent})
		require.True(t, errors.As(err, &refusal), "%v", err)
		require.Equal(t, http.StatusTooManyRequests, refusal.Status)
		require.Equal(t, billing.CodeRateLimitExceeded, refusal.Code)
		require.Len(t, mail.all(), 10, "the eleventh test is not sent")
	})
	t.Run("OR-J39-S19/only-a-refused-recipient-is-final-a-refused-sender-retries-with-a-finding", func(t *testing.T) {
		t.Run("recipient-refused", func(t *testing.T) {
			server := smtptest.Start(t, smtptest.Options{RefuseRecipient: func(string) string { return "550 5.1.1 no such user" }})
			a, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = smtpSender(t, server, "") })
			alice := customerOf(a.alice)
			paidExternally(t, a, alice, messageSale(t, a), "refused-recipient")
			fleet := startFleet(t, a.world)
			log := settledMessages(t, a.world, alice, 1)
			messagesSettled(t, fleet)
			require.Equal(t, billing.MessageFailed, log[0].Status)
			require.Equal(t, billing.MessageRefused, lastError(log[0]))
			require.Equal(t, 1, log[0].Attempts, "a recipient refused for good is not retried")
			require.Empty(t, server.Messages())
		})
		t.Run("sender-refused", func(t *testing.T) {
			server := smtptest.Start(t, smtptest.Options{Username: "openrails", Password: "the-right-one"})
			a, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = smtpSender(t, server, "a-wrong-one") })
			alice := customerOf(a.alice)
			paidExternally(t, a, alice, messageSale(t, a), "refused-sender")
			startFleet(t, a.world)
			require.Eventually(t, func() bool {
				log, err := customerMessages(t, a.alpha, alice)
				return err == nil && len(log) == 1 && log[0].Status == billing.MessageQueued && log[0].Attempts >= 1 &&
					lastError(log[0]) == billing.MessageSendFailed
			}, 30*time.Second, 20*time.Millisecond, "a refused sender is retried")
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.customer.messages_sender_refused"})
			require.NoError(t, err)
			require.Len(t, findings.Items, 1)
			require.Equal(t, "deployment_sender", findings.Items[0].SubjectKey)
			require.Equal(t, "AUTH", findings.Items[0].Evidence["step"])
			require.EqualValues(t, 535, findings.Items[0].Evidence["reply_code"])
			require.Empty(t, server.Messages())
		})
	})
	t.Run("OR-J39-S20/a-last-attempt-whose-job-ends-mid-send-settles-failed", func(t *testing.T) {
		mail := &stalledMailbox{refuse: messageSendAttempts - 1, stalled: make(chan struct{})}
		a, _ := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
		alice := customerOf(a.alice)
		paidExternally(t, a, alice, messageSale(t, a), "stalled-receipt")
		fleet := promptFleet(t, a.world)
		_, err := fleet.Insert(t.Context(), registeredJob{KindName: "openrails.customer_messages_dispatch"}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		select {
		case <-mail.stalled:
		case <-time.After(30 * time.Second):
			t.Fatal("the last attempt never reached the sender")
		}
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, fleet.StopAndCancel(stop), "the job's context ends while the sender holds its last attempt")
		log, err := customerMessages(t, a.alpha, alice)
		require.NoError(t, err)
		require.Len(t, log, 1)
		require.Equal(t, billing.MessageFailed, log[0].Status, "not left sending")
		require.Equal(t, billing.MessageSendFailed, lastError(log[0]))
		require.Equal(t, messageSendAttempts, log[0].Attempts)
	})
	t.Run("OR-J39-S21/a-declined-checkout-sends-nothing-by-default-and-names-its-psp", func(t *testing.T) {
		decline := func(t *testing.T, a *authorityWorld, transaction string) {
			t.Helper()
			status, body := ccbillNotice(t, a, "NewSaleFailure", map[string]any{"transactionId": transaction, "email": a.alice.Email, "username": a.alice.Username,
				"flexId": "critical-flex", "billedCurrencyCode": "840", "failureCode": "15", "failureReason": "Insufficient funds"})
			require.Equal(t, http.StatusOK, status, body)
		}
		t.Run("off-by-default", func(t *testing.T) {
			mail := &alertMailbox{}
			a := ccbillWebhookWorld(t, func(w *world) { w.deps.Email = mail })
			decline(t, a, "0912233445566778801")
			liveMoney(t, a.world)
			startFleet(t, a.world)
			log := settledMessages(t, a.world, customerOf(a.alice), 1)
			require.Equal(t, "payment_method_failed", log[0].EventType)
			require.Equal(t, billing.MessageDisabled, log[0].Status, "the checkout page told the customer")
			require.Empty(t, mail.all())
		})
		t.Run("turned-on-on-a-sandbox-psp", func(t *testing.T) {
			mail := &alertMailbox{}
			on := true
			a := ccbillWebhookWorld(t, func(w *world) {
				w.deps.Email = mail
				declareMessages(w, billing.MessageSettings{Events: map[string]billing.MessageEventSettings{"payment_method_failed": {Enabled: &on}}})
			})
			decline(t, a, "0912233445566778802")
			startFleet(t, a.world)
			log := settledMessages(t, a.world, customerOf(a.alice), 1)
			require.Equal(t, billing.MessageTestEnvironment, log[0].Status, "the decline names its sandbox PSP")
			require.Empty(t, mail.all())
		})
	})
	t.Run("OR-J39-S22/a-refund-found-more-than-a-week-late-sends-no-receipt", func(t *testing.T) {
		// refundFound is alice's sale refunded at the provider and found by a
		// refresh late after it, with its messages sent.
		refundFound := func(t *testing.T, late time.Duration) (*authorityWorld, *alertMailbox) {
			t.Helper()
			mail := &alertMailbox{}
			a, p := newPaymentWorld(t, func(w *world) { w.deps.Email = mail })
			status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, saleInput(t, a, p), map[string]string{"Idempotency-Key": "refunded-at-the-provider"})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			var charge string
			for _, effect := range p.effects(t) {
				if effect.Kind == "charge" {
					charge = effect.ID
				}
			}
			require.NotEmpty(t, charge)
			_, err := p.core.Refund(charge, 1000)
			require.NoError(t, err)
			a.clock.Advance(late)
			fleet := undispatchedFleet(t, a.world)
			refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
			read, err := a.alpha.GetPayment(t.Context(), *order.Payment.PaymentID)
			require.NoError(t, err)
			require.Len(t, read.Refunds, 1, "the refresh records the refund")
			liveMoney(t, a.world)
			dispatchMessages(t, fleet)
			return a, mail
		}
		t.Run("minutes-late", func(t *testing.T) {
			a, mail := refundFound(t, 10*time.Minute)
			receipt := sentAbout(t, a, mail, "refund_completed", "Your refund from Alpha")
			require.Contains(t, receipt.Text, "We refunded 10.00 USD")
		})
		t.Run("eight-days-late", func(t *testing.T) {
			a, mail := refundFound(t, 8*24*time.Hour)
			alice := customerOf(a.alice)
			log, err := customerMessages(t, a.alpha, alice)
			require.NoError(t, err)
			require.True(t, ended(log))
			require.Empty(t, ofType(log, "refund_completed"), "old news records no receipt")
			require.False(t, factEmitted(t, a.alpha, "refund_completed", alice))
			for _, email := range mail.to(a.alice.Email) {
				require.NotEqual(t, "Your refund from Alpha", email.Subject)
			}
		})
	})
	t.Run("OR-J39-S23/a-void-is-confirmed-as-a-canceled-charge", func(t *testing.T) {
		mail := &alertMailbox{}
		a := ccbillWebhookWorld(t, func(w *world) { w.deps.Email = mail })
		price := ccbillMemberPrice(t, a.alpha)
		now := a.clock.Now().UTC().Truncate(time.Second)
		const ref, sale = "556677889900112255", "0812233445566778833"
		importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(customerOf(a.alice), price, ref, sale, now.Add(5*24*time.Hour)))
		for range 2 {
			status, body := ccbillNotice(t, a, "Void", map[string]any{"subscriptionId": ref, "transactionId": sale, "amount": "10.00", "currencyCode": "840", "reason": "voided"})
			require.Equal(t, http.StatusOK, status, body)
		}
		liveMoney(t, a.world)
		startFleet(t, a.world)
		receipt := sentAbout(t, a, mail, "refund_completed", "Your payment to Alpha was canceled")
		require.Contains(t, receipt.Text, "Your payment of 10.00 USD")
		require.Contains(t, receipt.Text, "was canceled; you weren't charged.")
		require.NotContains(t, receipt.Text, "refund")
	})
	t.Run("OR-J39-S24/a-subscriptions-first-payment-sends-its-receipt", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _, sub := purchasedMembershipWith(t, 7*24, func(w *world) { w.deps.Email = mail; alphaProfile(w) })
		liveMoney(t, a.world)
		startFleet(t, a.world)
		receipt := sentAbout(t, a, mail, "premium_started", "Your subscription to Alpha has started")
		day := func(at *time.Time) string { return at.UTC().Format("January 2, 2006") }
		require.Contains(t, receipt.Text, "Thank you for subscribing to Membership.")
		require.Contains(t, receipt.Text, "Amount paid: 10.00 USD\nBilling period: "+day(sub.CurrentPeriodStartsAt)+" to "+day(sub.CurrentPeriodEndsAt))
		require.Contains(t, receipt.HTML, "<strong>10.00 USD</strong>")
	})
}

// declareMessages gives alpha the messages document s.
func declareMessages(w *world, s billing.MessageSettings) {
	declaration := cfgMerchant(w.cfg, "alpha")
	declaration.Messages = &s
	setCfgMerchant(&w.cfg, "alpha", declaration)
}

// alphaProfile gives alpha a profile with its support and billing pages.
func alphaProfile(w *world) {
	declaration := cfgMerchant(w.cfg, "alpha")
	declaration.Profile = &billing.MerchantProfile{SupportURL: "https://alpha.example/help", BillingURL: "https://alpha.example/billing"}
	setCfgMerchant(&w.cfg, "alpha", declaration)
}

// liveMoney declares alpha's PSPs live, as a live merchant's are, so the
// messages of facts on them send: a world with test seams has test PSPs
// only. A scenario calls it once its business is done and before its
// messages are dispatched, so only the send job reads it.
func liveMoney(t *testing.T, w *world) {
	t.Helper()
	_, err := w.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{w.schema, "psps"}.Sanitize()+" SET environment = 'live' WHERE merchant_id = $1", w.alpha.MerchantID().UUID())
	require.NoError(t, err)
}

// sentAbout waits until every one of alice's messages has ended, then
// requires one sent message about eventType and one email with subject,
// and returns the email.
func sentAbout(t *testing.T, a *authorityWorld, mail *alertMailbox, eventType, subject string) openrails.Email {
	t.Helper()
	alice := customerOf(a.alice)
	var log []billing.CustomerMessage
	require.Eventually(t, func() bool {
		rows, err := customerMessages(t, a.alpha, alice)
		if err != nil || len(ofType(rows, eventType)) == 0 || !ended(rows) {
			return false
		}
		log = rows
		return true
	}, 30*time.Second, 20*time.Millisecond, "every message ends")
	about := ofType(log, eventType)
	require.Len(t, about, 1, "one message per fact")
	require.Equal(t, billing.MessageSent, about[0].Status)
	var emails []openrails.Email
	for _, email := range mail.to(a.alice.Email) {
		if email.Subject == subject {
			emails = append(emails, email)
		}
	}
	require.Len(t, emails, 1, "one %q among %d emails", subject, len(mail.to(a.alice.Email)))
	return emails[0]
}

// ofType is the log's messages about eventType.
func ofType(log []billing.CustomerMessage, eventType string) []billing.CustomerMessage {
	var out []billing.CustomerMessage
	for _, m := range log {
		if m.EventType == eventType {
			out = append(out, m)
		}
	}
	return out
}

// ended reports whether every message of the log has ended.
func ended(log []billing.CustomerMessage) bool {
	for _, m := range log {
		if m.Status == billing.MessagePending || m.Status == billing.MessageQueued || m.Status == billing.MessageSending {
			return false
		}
	}
	return true
}

// messageSale is a one-off price alpha sells, as order lines.
func messageSale(t *testing.T, a *authorityWorld) []billing.OrderLineParams {
	t.Helper()
	return []billing.OrderLineParams{{PriceID: hostProduct(t, a.alpha, "receipt-item").ID}}
}

// paidExternally creates an order of lines for customer and records its
// payment outside any PSP.
func paidExternally(t *testing.T, a *authorityWorld, customer billing.CustomerID, lines []billing.OrderLineParams, key string) {
	t.Helper()
	order, err := a.alpha.CreateOrder(t.Context(), key, billing.CreateOrderParams{CustomerID: customer, Lines: lines})
	require.NoError(t, err)
	_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 10_000_000, TransactionID: "bank-" + key})
	require.NoError(t, err)
}

// customerMessages is the customer's message log as client's route lists
// it, every page, oldest first.
func customerMessages(t *testing.T, client *openrails.Client, customer billing.CustomerID) ([]billing.CustomerMessage, error) {
	var out []billing.CustomerMessage
	var params billing.CustomerMessageListParams
	for {
		page, err := client.ListCustomerMessages(t.Context(), customer, params)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Next == "" {
			break
		}
		params.Cursor = page.Next
	}
	slices.Reverse(out)
	return out, nil
}

// settledMessages waits for n of alpha's messages to the customer and for
// each to end.
func settledMessages(t *testing.T, w *world, customer billing.CustomerID, n int) []billing.CustomerMessage {
	t.Helper()
	var log []billing.CustomerMessage
	require.Eventually(t, func() bool {
		rows, err := customerMessages(t, w.alpha, customer)
		if err != nil || len(rows) != n || !ended(rows) {
			return false
		}
		log = rows
		return true
	}, 30*time.Second, 20*time.Millisecond, "every message ends")
	return log
}

// undispatchedFleet starts a fleet with no dispatcher, listening or
// periodic, and no periodic pass: a scenario's messages wait for the
// dispatch it runs (dispatchMessages).
func undispatchedFleet(t *testing.T, w *world) *river.Client[pgx.Tx] {
	t.Helper()
	fleet := hostFleet(t, w)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, fleet.Start(t.Context()))
	return fleet
}

// dispatchMessages runs one dispatch pass and waits for the send jobs it
// queued.
func dispatchMessages(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	runRegistered(t, fleet, "openrails.customer_messages_dispatch", 1)
	messagesSettled(t, fleet)
}

// messagesSettled waits until no customer message job is outstanding.
func messagesSettled(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	require.Eventually(t, func() bool {
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.customer_message").States(rivertype.JobStateAvailable,
			rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled))
		return err == nil && len(jobs.Jobs) == 0
	}, 30*time.Second, 20*time.Millisecond, "every customer message job settles")
}

// factEmitted reports whether the event log holds the customer's fact.
func factEmitted(t *testing.T, client *openrails.Client, eventType string, customer billing.CustomerID) bool {
	t.Helper()
	page, err := client.ListEvents(t.Context(), billing.EventQuery{Types: []string{eventType}, Limit: 100})
	require.NoError(t, err)
	for _, e := range page.Events {
		if e.Subject.ID == customer.String() {
			return true
		}
	}
	return false
}

// to is what the mailbox delivered to address.
func (m *alertMailbox) to(address string) []openrails.Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []openrails.Email
	for _, email := range m.sent {
		if email.To == address {
			out = append(out, email)
		}
	}
	return out
}

// all is everything the mailbox delivered.
func (m *alertMailbox) all() []openrails.Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]openrails.Email(nil), m.sent...)
}

// messageSendAttempts is a message's tries (messages.SendAttempts).
const messageSendAttempts = 10

// smtpSender is helpers/smtp to server as the engine's EmailSender, as the
// server builds it from email_smtp, signing in with password when the server
// asks for one.
func smtpSender(t *testing.T, server *smtptest.Server, password string) openrails.EmailSender {
	t.Helper()
	cfg := smtp.Config{Host: server.Host, Port: server.Port, From: "billing@alpha.example", Timeout: 5 * time.Second}
	if password != "" {
		cfg.Username, cfg.Password = "openrails", password
	}
	sender, err := smtp.New(cfg)
	require.NoError(t, err)
	return helpersSMTP{sender}
}

type helpersSMTP struct{ *smtp.Sender }

func (s helpersSMTP) Send(ctx context.Context, m openrails.Email) error {
	from := s.From()
	if m.From.Address != "" {
		from.Address = m.From.Address
	}
	from.Name = m.From.Name
	return s.Sender.Send(ctx, smtp.Message{From: from, To: m.To, Subject: m.Subject, Text: m.Text, HTML: m.HTML})
}

// stalledMailbox refuses its first refuse sends, then holds the next until
// its context ends.
type stalledMailbox struct {
	mu      sync.Mutex
	refuse  int
	held    bool
	stalled chan struct{}
}

func (m *stalledMailbox) Send(ctx context.Context, _ openrails.Email) error {
	m.mu.Lock()
	refuse := m.refuse > 0
	if refuse {
		m.refuse--
	} else if !m.held {
		m.held = true
		close(m.stalled)
	}
	m.mu.Unlock()
	if refuse {
		return errors.New("critical mailbox is unavailable")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *stalledMailbox) CheckHealth(context.Context) error { return nil }

// promptFleet is a fleet that retries a failed job at once and dispatches
// nothing by itself, so a message reaches its last attempt within a
// scenario.
func promptFleet(t *testing.T, w *world) *river.Client[pgx.Tx] {
	t.Helper()
	fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
		RetryPolicy: retryAtOnce{}, FetchCooldown: 5 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond}, w.engine.RiverJobs())
	require.NoError(t, err)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, fleet.Start(t.Context()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = fleet.StopAndCancel(ctx)
	})
	return fleet
}

type retryAtOnce struct{}

func (retryAtOnce) NextRetry(*rivertype.JobRow) time.Time {
	return time.Now().Add(10 * time.Millisecond)
}

// lastError is m's last error; empty after a send.
func lastError(m billing.CustomerMessage) billing.MessageError {
	if m.LastError == nil {
		return ""
	}
	return *m.LastError
}
