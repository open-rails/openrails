package reconcile

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestNMIPullKeepsNativePaymentsWithTheirOperation(t *testing.T) {
	now := time.Now().UTC()
	subID, priceID, customerID, methodID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, order, source string
		policy              models.CollectionPolicy
		wantApply           bool
	}{
		{"invoice on sole provider subscription", uuid.NewString(), "api", models.CollectionPolicyNMISchedule, false},
		{"native renewal on sole engine subscription", subscriptions.ObligationOrderReference(subID, now), "api", models.CollectionPolicyEngine, false},
		{"unknown API sale cannot use email", "external-order", "api", models.CollectionPolicyNMISchedule, false},
		{"engine ID cannot authorize mirror writer", subID.String(), "api", models.CollectionPolicyEngine, false},
		{"provider schedule exact order", subID.String(), "recurring", models.CollectionPolicyNMISchedule, true},
		{"historical exact rebill", fmt.Sprintf("rebill-%s-%d", subID, now.Unix()), "api", models.CollectionPolicyNMISchedule, true},
		{"provider recurring vault", "provider-schedule-order", "recurring", models.CollectionPolicyNMISchedule, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			until := now.Add(time.Hour)
			local := &LocalState{Subscriptions: []LocalSubscription{{ID: subID, CustomerID: customerID, PriceID: &priceID, Rail: "nmi", CollectionPolicy: tc.policy, Status: "active", RailSubscriptionID: "provider-sub", PaymentMethodID: &methodID, CurrentPeriodEndsAt: &until, CustomerEmail: "buyer@example.test"}}, PaymentMethods: []LocalPaymentMethod{{ID: methodID, CustomerID: customerID, Rail: "nmi", RailCustomerRef: "vault"}}}
			tx := RemoteTransaction{TransactionID: "sale-1", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now, Raw: rawJSON(map[string]any{"order_id": tc.order, "customer_vault_id": "vault", "email": "buyer@example.test", "action": map[string]string{"source": tc.source}})}
			findings := diffProvider(ProviderNMI, &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Transactions: true}, Subscriptions: []RemoteSubscription{{RailSubscriptionID: "provider-sub", CustomerID: "vault"}}, Transactions: []RemoteTransaction{tx}}, local, nil, now, diffOptions{})
			require.Len(t, findings, 1)
			if tc.wantApply {
				require.NotNil(t, findings[0].Apply)
			} else {
				require.Nil(t, findings[0].Apply, "a native/unknown charge must not create generic payment or access")
				require.Equal(t, FindingStatusRequiresReview, findings[0].Status)
			}
		})
	}
}

func TestNMINewScheduleCannotBorrowAnotherChargeOnItsVault(t *testing.T) {
	schedule := RemoteSubscription{RailSubscriptionID: "nmi-sub", CustomerID: "vault"}
	tx := RemoteTransaction{TransactionID: "invoice-sale", Type: TransactionTypeSale, Success: true, AmountCents: 999, Raw: rawJSON(map[string]any{"customer_vault_id": "vault", "action": map[string]string{"source": "api"}})}
	snapshot := &RemoteSnapshot{Provider: ProviderNMI, Subscriptions: []RemoteSubscription{schedule}, Transactions: []RemoteTransaction{tx}}
	require.Nil(t, latestChargeForRemoteSub(snapshot, &schedule), "materialization must not grant a schedule from an invoice sale")
	snapshot.Transactions[0].Raw = rawJSON(map[string]any{"customer_vault_id": "vault", "action": map[string]string{"source": "recurring"}})
	require.NotNil(t, latestChargeForRemoteSub(snapshot, &schedule))
	snapshot.Subscriptions = append(snapshot.Subscriptions, RemoteSubscription{RailSubscriptionID: "other", CustomerID: "vault"})
	require.Nil(t, latestChargeForRemoteSub(snapshot, &schedule), "recurring source alone cannot choose between schedules sharing a vault")
	snapshot.Transactions[0].SubscriptionID = schedule.RailSubscriptionID
	require.NotNil(t, latestChargeForRemoteSub(snapshot, &schedule), "exact schedule identity resolves that ambiguity")
}

func TestNMIInvoiceReceiptCannotBecomeSubscriptionRefund(t *testing.T) {
	invoice := uuid.New()
	known := LocalPayment{ID: uuid.New(), CustomerID: uuid.New(), TransactionID: "invoice-sale", AmountCents: 999, Currency: "USD", InvoiceID: &invoice, Status: "settled"}
	tx := RemoteTransaction{TransactionID: "invoice-sale", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD"}
	snapshot := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Transactions: true, Refunds: true}, Transactions: []RemoteTransaction{tx}}
	require.Empty(t, diffProvider(ProviderNMI, snapshot, &LocalState{}, []LocalPayment{known}, time.Now(), diffOptions{}))
	snapshot.Transactions[0].AmountCents++
	findings := diffProvider(ProviderNMI, snapshot, &LocalState{}, []LocalPayment{known}, time.Now(), diffOptions{})
	require.Len(t, findings, 1)
	require.Nil(t, findings[0].Apply)
	require.Equal(t, FindingStatusRequiresReview, findings[0].Status)
	snapshot.Transactions[0].Type = TransactionTypeRefund
	findings = diffProvider(ProviderNMI, snapshot, &LocalState{}, []LocalPayment{known}, time.Now(), diffOptions{})
	require.Len(t, findings, 1)
	require.Nil(t, findings[0].Apply)
	require.Equal(t, FindingStatusRequiresReview, findings[0].Status)
}

func TestNMIRecurringVaultDoesNotHideAnUnmappedSchedule(t *testing.T) {
	subID, methodID, customerID, priceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	local := &LocalState{Subscriptions: []LocalSubscription{{ID: subID, CustomerID: customerID, PriceID: &priceID, Rail: "nmi", CollectionPolicy: models.CollectionPolicyNMISchedule, RailSubscriptionID: "known", PaymentMethodID: &methodID, Status: "active"}}, PaymentMethods: []LocalPaymentMethod{{ID: methodID, CustomerID: customerID, RailCustomerRef: "vault"}}}
	tx := RemoteTransaction{TransactionID: "sale", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", Raw: rawJSON(map[string]any{"customer_vault_id": "vault", "action": map[string]string{"source": "recurring"}})}
	snapshot := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Transactions: true}, Subscriptions: []RemoteSubscription{{RailSubscriptionID: "known", CustomerID: "vault"}, {RailSubscriptionID: "missing-locally", CustomerID: "vault"}}, Transactions: []RemoteTransaction{tx}}
	findings := diffProvider(ProviderNMI, snapshot, local, nil, time.Now(), diffOptions{})
	require.Len(t, findings, 1)
	require.Nil(t, findings[0].Apply)
	require.Equal(t, FindingStatusRequiresReview, findings[0].Status)
}
