package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestStripePullDoesNotBackfillNativeRenewalFromCustomer(t *testing.T) {
	sub, price, customer, method := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	local := &LocalState{Subscriptions: []LocalSubscription{{ID: sub, CustomerID: customer, PriceID: &price, Status: "active", CollectionPolicy: models.CollectionPolicyEngine, PaymentMethodID: &method}}, PaymentMethods: []LocalPaymentMethod{{ID: method, CustomerID: customer, RailCustomerRef: "cus_1"}}}
	transaction := RemoteTransaction{TransactionID: "ch_1", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", Raw: rawJSON(map[string]string{"payment_intent": "pi_1", "customer": "cus_1"})}
	snapshot := &RemoteSnapshot{Provider: ProviderStripe, Capabilities: Capabilities{Transactions: true}, Transactions: []RemoteTransaction{transaction}}
	findings := diffProvider(ProviderStripe, snapshot, local, nil, time.Now(), diffOptions{})
	require.Len(t, findings, 1)
	require.Equal(t, FindingStatusRequiresReview, findings[0].Status)
	require.Nil(t, findings[0].Apply, "native receipts belong to their accepted operation")
	known := LocalPayment{ID: uuid.New(), CustomerID: customer, SubscriptionID: &sub, TransactionID: "ch_1", AmountCents: 999, Currency: "USD", Status: "completed"}
	require.Empty(t, diffProvider(ProviderStripe, snapshot, local, []LocalPayment{known}, time.Now(), diffOptions{}))
}

// The active Stripe checkout.session.completed handler stores pi_* payment
// IDs (also asserted by ci/TestStripeWebhookReplayAndReorderingConverges). Bulk charge
// reports must recognize those same payments without re-allocating them.
func TestStripePullRecognizesCheckoutPaymentIntent(t *testing.T) {
	for _, name := range []string{"match", "wrong_amount", "wrong_currency", "two_allocations", "two_charges", "not_settled"} {
		t.Run(name, func(t *testing.T) {
			payment := LocalPayment{ID: uuid.New(), TransactionID: "pi_1", AmountCents: 999, Currency: "USD", Status: "completed"}
			payments := []LocalPayment{payment}
			transaction := RemoteTransaction{TransactionID: "ch_1", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", Raw: rawJSON(map[string]string{"payment_intent": "pi_1"})}
			snapshot := &RemoteSnapshot{Provider: ProviderStripe, Capabilities: Capabilities{Transactions: true}, Transactions: []RemoteTransaction{transaction}}
			switch name {
			case "wrong_amount":
				payments[0].AmountCents++
			case "wrong_currency":
				payments[0].Currency = "EUR"
			case "two_allocations":
				other := payment
				other.ID, other.TransactionID = uuid.New(), "ch_1"
				payments = append(payments, other)
			case "two_charges":
				other := transaction
				other.TransactionID = "ch_2"
				snapshot.Transactions = append(snapshot.Transactions, other)
			case "not_settled":
				payments[0].Status = "pending"
			}
			findings := diffProvider(ProviderStripe, snapshot, &LocalState{}, payments, time.Now(), diffOptions{})
			if name == "match" {
				require.Empty(t, findings)
				return
			}
			require.NotEmpty(t, findings)
			for _, finding := range findings {
				require.Equal(t, FindingStatusRequiresReview, finding.Status)
				require.Nil(t, finding.Apply)
			}
		})
	}
}

func TestStripeRefundLinksCheckoutPaymentOutsideChargeWindow(t *testing.T) {
	payment := LocalPayment{ID: uuid.New(), TransactionID: "pi_1", AmountCents: 999, Currency: "USD", Status: "completed"}
	snapshot := &RemoteSnapshot{Provider: ProviderStripe, Capabilities: Capabilities{Transactions: true, Refunds: true}, Transactions: []RemoteTransaction{{TransactionID: "re_1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, Currency: "USD", Raw: rawJSON(map[string]string{"charge": "ch_1", "payment_intent": "pi_1"})}}}
	findings := diffProvider(ProviderStripe, snapshot, &LocalState{}, []LocalPayment{payment}, time.Now(), diffOptions{})
	require.Len(t, findings, 1)
	require.NotNil(t, findings[0].Apply)
	require.Equal(t, payment.ID, *findings[0].Apply.RecordRefund.RefundedPaymentID)
	require.Contains(t, collectTxnLookupIDs(snapshot), "pi_1")
}
