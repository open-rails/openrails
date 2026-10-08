package reconcile

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
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
