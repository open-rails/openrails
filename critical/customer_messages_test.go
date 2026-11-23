//go:build integration

package critical_test

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
		require.Equal(t, "sent", log[0].Status)
		require.Equal(t, 2, log[0].Attempts)
		require.Equal(t, a.alice.Email, deref(log[0].Recipient))
		require.Nil(t, log[0].ErrorCode)
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
		log, err := customerMessages(t, target, client.MerchantID(), alice)
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
		require.Equal(t, "no_contact", log[0].Status)
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
		require.Equal(t, "test_environment", log[0].Status)
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
		statuses := map[string]int{}
		for _, m := range log {
			statuses[m.Status]++
		}
		require.Equal(t, map[string]int{"sent": 10, "rate_limited": 1}, statuses)
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
		require.Equal(t, "sent", log[0].Status)
		require.Len(t, mail.to("erased@directory.invalid"), 1)
		status, raw = a.request(t, http.MethodDelete, "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		log, err = customerMessages(t, a.world, a.alpha.MerchantID(), customer)
		require.NoError(t, err)
		require.Empty(t, log, "erasure deletes the customer's message log")
	})
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

// loggedMessage is one row of a customer's message log.
type loggedMessage struct {
	EventType string
	Status    string
	Attempts  int
	Recipient *string
	ErrorCode *string
}

// customerMessages is the customer's message log, oldest first.
func customerMessages(t *testing.T, w *world, merchant billing.MerchantID, customer billing.CustomerID) ([]loggedMessage, error) {
	rows, err := w.pool.Query(t.Context(), "SELECT event_type, status, attempts, recipient, last_error_code FROM "+pgx.Identifier{w.schema, "customer_messages"}.Sanitize()+
		" WHERE merchant_id = $1 AND customer_id = $2 ORDER BY created_at, id", merchant.UUID(), customer.UUID())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (loggedMessage, error) {
		var m loggedMessage
		err := row.Scan(&m.EventType, &m.Status, &m.Attempts, &m.Recipient, &m.ErrorCode)
		return m, err
	})
}

// settledMessages waits for n of alpha's messages to the customer and for
// each to end.
func settledMessages(t *testing.T, w *world, customer billing.CustomerID, n int) []loggedMessage {
	t.Helper()
	var log []loggedMessage
	require.Eventually(t, func() bool {
		rows, err := customerMessages(t, w, w.alpha.MerchantID(), customer)
		if err != nil || len(rows) != n {
			return false
		}
		for _, m := range rows {
			if m.Status == "pending" || m.Status == "queued" || m.Status == "sending" {
				return false
			}
		}
		log = rows
		return true
	}, 30*time.Second, 20*time.Millisecond, "every message ends")
	return log
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
