//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A declared card is its PSP-scoped instrument key, an absent method ref
// included: re-importing a card finds the stored one instead of inserting it
// again, and a later book's subscription resolves to it without declaring it.
func TestImportPaymentMethodIdempotent(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			client := w.client[tp]
			c := w.newCustomer()
			customerID := c.customerID()
			suffix := uuid.NewString()[:8]
			bareCust, fullCust, fullMethod := "cus_bare"+suffix, "cus_full"+suffix, "pm_full"+suffix
			bare := billing.DeclaredPaymentMethod{Customer: customerID, Rail: "stripe", RailCustomerRef: bareCust, Card: declaredCard(visa)}
			full := billing.DeclaredPaymentMethod{Customer: customerID, Rail: "stripe", RailCustomerRef: fullCust, RailMethodRef: fullMethod, Card: declaredCard(visa)}
			book := func(methods ...billing.DeclaredPaymentMethod) billing.DeclaredBilling {
				return billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "stripe"},
					Customers: []billing.DeclaredCustomer{{Customer: customerID}}, PaymentMethods: methods}
			}
			stored := func() []billing.PaymentMethodID {
				page, err := client.ListPaymentMethods(t.Context(), customerID, billing.PaymentMethodListParams{})
				require.NoError(t, err)
				var ids []billing.PaymentMethodID
				for _, m := range page.Items {
					ids = append(ids, m.ID)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
				return ids
			}

			_, err := client.ImportBilling(t.Context(), book(bare))
			require.NoError(t, err)
			first := stored()
			require.Len(t, first, 1)
			bareID := first[0]

			_, err = client.ImportBilling(t.Context(), book(bare, full))
			require.NoError(t, err, "a card without a method ref is found again")
			both := stored()
			require.Len(t, both, 2)
			require.Contains(t, both, bareID)
			fullID := both[0]
			if fullID == bareID {
				fullID = both[1]
			}

			_, err = client.ImportBilling(t.Context(), book(bare, full))
			require.NoError(t, err, "a card with both refs is found again")
			require.Equal(t, both, stored())

			hours := monthHours
			start := w.clock.Now().Add(-10 * day)
			end := start.Add(monthHours * time.Hour)
			subs := book()
			want := map[string]billing.PaymentMethodID{}
			for i, ref := range []billing.PaymentMethodRef{
				{Rail: "stripe", RailCustomerRef: bareCust},
				{Rail: "stripe", RailCustomerRef: fullCust, RailMethodRef: fullMethod},
			} {
				key := fmt.Sprintf("pm-import-%s-%d", suffix, i)
				product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Imported membership", Entitlements: []string{"content:" + key}})
				require.NoError(t, err)
				link := "price_legacy_" + key
				price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key + "-usd", UnitAmount: 9_990_000, Currency: "USD",
					BillingIntervalHours: &hours, AccessDurationHours: &hours, PSPLinks: map[string]map[string]string{"stripe": {"price_id": link}}})
				require.NoError(t, err)
				method := ref.RailMethodRef
				if method == "" {
					method = "pm_unrecorded" + suffix
				}
				railSub := w.stripe.AddSubscription(ref.RailCustomerRef, method, link, 999, start, end)
				subs.Subscriptions = append(subs.Subscriptions, billing.DeclaredSubscription{SourceID: "legacy-" + railSub, Customer: customerID, Price: price.ID, Rail: "stripe",
					RailSubscriptionID: railSub, StartedAt: start, PaidThrough: &end, PaymentMethod: &ref})
				subs.Transactions = append(subs.Transactions, billing.DeclaredTransaction{RailSubscriptionID: railSub, TransactionID: w.stripe.LatestCharge(railSub), Success: true,
					Amount: 9_990_000, Currency: "USD", OccurredAt: start})
				want[railSub] = bareID
				if ref.RailMethodRef != "" {
					want[railSub] = fullID
				}
			}
			result, err := client.ImportBilling(t.Context(), subs)
			require.NoError(t, err)
			require.Len(t, result.Imported, 2, "%+v", result)
			w.settle()
			list, err := client.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customerID})
			require.NoError(t, err)
			got := map[string]billing.PaymentMethodID{}
			for _, s := range list.Items {
				require.NotNil(t, s.PaymentMethodID, str(s.RailSubscriptionID))
				got[str(s.RailSubscriptionID)] = *s.PaymentMethodID
			}
			require.Equal(t, want, got, "each subscription resolves to the stored card")
			require.Equal(t, both, stored())
		})
	}
}
