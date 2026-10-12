//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A live NMI member whose book proves no accepted schedule still imports:
// the unproven schedule is a finding and no renewal is inferred.
func TestImportUnprovenScheduleIsAFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	monthly := w.bookTier("unproven", 999, 30)
	now := w.clock.Now()
	c, vault := w.newCustomer(), w.nmi.AddVault(visa)
	paid := now.Add(10 * day)
	schedule := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: monthly.plan, Amount: monthly.amount, Days: 30, NextBilling: paid})
	method := &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID}
	res, err := w.client[embedded].ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"},
		Customers:      []billing.DeclaredCustomer{{Customer: c.customerID()}},
		PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: c.customerID(), Rail: "nmi", RailCustomerRef: vault, RailMethodRef: method.RailMethodRef, Card: declaredCard(visa)}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "unproven", Customer: c.customerID(), Price: monthly.price.ID, Rail: "nmi", RailSubscriptionID: schedule,
			StartedAt: now.Add(-100 * day), PaidThrough: &paid, PaymentMethod: method}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"unproven"}, res.Imported, "%+v", res)
	subs, err := w.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	require.Equal(t, []string{"subscription:" + subs.Items[0].ID.UUID().String()}, w.openFindings("life.import.schedule_unresolved"))
}
