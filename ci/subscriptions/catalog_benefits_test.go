//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
)

const acceptedCatalogBenefit = "content:accepted"
const editedCatalogBenefit = "content:edited"

func requireSubscriptionBenefits(t *testing.T, w *world, id billing.SubscriptionID, expected map[string]*int) {
	t.Helper()
	var raw []byte
	require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT coalesce(entitlements_spec_snapshot, '{}'::jsonb) FROM billing.subscriptions WHERE id=$1`), id.UUID()).Scan(&raw))
	want, err := json.Marshal(expected)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(raw))
}

func importCatalogBenefitsCCBill(t *testing.T, w *world, benefits map[string]*int) *ccbillMember {
	t.Helper()
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "accepted-benefits", DisplayName: "Accepted benefits", EntitlementsSpec: benefits})
	require.NoError(t, err)
	hours := monthHours
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"ccbill": {"form_name": ccbillFormName, "flex_id": ccbillFlexID, "recurring_billing_option_id": ccbillRBO}}})
	require.NoError(t, err)
	start := w.clock.Now().Add(-10 * day)
	end := start.Add(monthHours * time.Hour)
	l := &legacy{w: w, rail: "ccbill", tp: embedded, c: w.newCustomer(), price: price, railSub: ccbillNumericID(), ent: acceptedCatalogBenefit}
	m := &ccbillMember{legacy: l, paidThrough: end, saleTxn: ccbillNumericID()}
	result, err := c.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"},
		Customers:     []billing.DeclaredCustomer{{Customer: l.c.cid()}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: l.c.cid(), Price: price.ID, Rail: "ccbill", RailSubscriptionID: l.railSub, StartedAt: start, PaidThrough: &end}},
		Transactions:  []billing.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: m.saleTxn, Success: true, Amount: 9_990_000, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(t, err)
	require.Len(t, result.Imported, 1)
	list, err := c.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: l.c.cid()})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	l.sub = list.Items[0].ID
	w.converge()
	return m
}

// CCBill's observed paid renewal reaches the unprepared lifecycle path. An
// empty agreement is just as durable as one containing named benefits.
func TestCatalogBenefitsRemainAcceptedOnRenewal(t *testing.T) {
	for _, empty := range []bool{true, false} {
		name := "nonempty"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			benefits := map[string]*int{}
			if !empty {
				benefits[acceptedCatalogBenefit] = nil
			}
			m := importCatalogBenefitsCCBill(t, w, benefits)
			requireSubscriptionBenefits(t, w, m.sub, benefits)
			_, err := w.client[embedded].UpdateProduct(t.Context(), m.price.ProductID, billing.UpdateProductParams{EntitlementsSpec: catalog.Value(map[string]*int{editedCatalogBenefit: nil})})
			require.NoError(t, err)
			w.advance(20*day + time.Hour)
			txn := ccbillNumericID()
			next := m.paidThrough.Add(monthHours * time.Hour)
			w.deliverCCBill("RenewalSuccess", m.renewal(txn, next))
			require.True(t, w.subscription(embedded, m.sub).CurrentPeriodEndsAt.After(m.paidThrough))
			requireSubscriptionBenefits(t, w, m.sub, benefits)
			require.Equal(t, !empty, m.c.entitled(acceptedCatalogBenefit))
			require.False(t, m.c.entitled(editedCatalogBenefit), "a product edit is not an accepted subscription change")
			var paymentSnapshot []byte
			require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT coalesce(entitlements_spec_snapshot, '{}'::jsonb) FROM billing.payments WHERE transaction_id=$1`), txn).Scan(&paymentSnapshot))
			want, err := json.Marshal(benefits)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(paymentSnapshot), "the paid record preserves the same accepted benefits")
			w.deliverCCBill("RenewalSuccess", m.renewal(txn, next))
			requireSubscriptionBenefits(t, w, m.sub, benefits)
		})
	}
}

// The member's NMI cancel/resume path calls ReactivateMembership. It restores
// the accepted benefits, including an intentionally empty set.
func TestCatalogBenefitsRemainAcceptedOnReactivation(t *testing.T) {
	for _, empty := range []bool{true, false} {
		name := "nonempty"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.armDestructive()
			benefits := map[string]*int{}
			if !empty {
				benefits[acceptedCatalogBenefit] = nil
			}
			l := importLegacy(t, w, "nmi", embedded, func(book *billing.DeclaredBilling) {
				price, err := w.client[embedded].GetPrice(t.Context(), book.Subscriptions[0].Price, billing.GetPriceParams{})
				require.NoError(t, err)
				_, err = w.client[embedded].UpdateProduct(t.Context(), price.ProductID, billing.UpdateProductParams{EntitlementsSpec: catalog.Value(benefits)})
				require.NoError(t, err)
			})
			w.converge()
			requireSubscriptionBenefits(t, w, l.sub, benefits)
			status, body := l.meCancel(l.sub)
			require.Equal(t, http.StatusOK, status, "%v", body)
			w.settle()
			require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, l.sub).Status)
			_, err := w.client[embedded].UpdateProduct(t.Context(), l.price.ProductID, billing.UpdateProductParams{EntitlementsSpec: catalog.Value(map[string]*int{editedCatalogBenefit: nil})})
			require.NoError(t, err)
			before := l.engineCharges()
			status, body = l.c.call(http.MethodPost, "/subscriptions/"+l.sub.String()+"/resume", "", map[string]any{})
			require.Equal(t, http.StatusOK, status, "%v", body)
			w.settle()
			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
			require.Equal(t, before, l.engineCharges())
			requireSubscriptionBenefits(t, w, l.sub, benefits)
			require.Equal(t, !empty, l.c.entitled(acceptedCatalogBenefit))
			require.False(t, l.c.entitled(editedCatalogBenefit))
		})
	}
}

// A pending native schedule has already accepted its benefits before the first
// provider payment arrives. The legacy completion path has no Prepared payload;
// it must use the persisted pending snapshot, including an empty one.
func TestCatalogPendingBenefitsRemainAcceptedOnFirstPayment(t *testing.T) {
	for _, empty := range []bool{true, false} {
		name := "nonempty"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			c := w.client[embedded]
			benefits := map[string]*int{}
			if !empty {
				benefits[acceptedCatalogBenefit] = nil
			}
			product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "pending-benefits", DisplayName: "Pending benefits", EntitlementsSpec: benefits})
			require.NoError(t, err)
			hours := monthHours
			price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
			require.NoError(t, err)
			buyer := w.newCustomer()
			method := pmid(buyer.saveCard("nmi", visa))
			var vault string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT rail_customer_ref FROM billing.payment_methods WHERE id=$1`), method.UUID()).Scan(&vault))
			start := w.clock.Now().Add(day)
			end := start.Add(monthHours * time.Hour)
			railSub := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Amount: "9.99", NextBilling: start})
			ctx := db.WithPSPID(merchant.WithID(t.Context(), c.MerchantID()), w.psp["nmi"].UUID())
			lifecycle := engine.Graph(w.rt).Runtime.SubscriptionLifecycleService
			accepted := &subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyNMISchedule, SubscriptionID: uuid.New(), CustomerID: buyer.cid().UUID(), PSPID: w.psp["nmi"].UUID(),
				ProductID: product.ID.UUID(), PriceID: price.ID.UUID(), PaymentMethodID: method.UUID(), ProductName: product.DisplayName, RecurringAmount: price.UnitAmount, Currency: "USD",
				AcceptedAt: w.clock.Now(), PeriodStart: start, PeriodEnd: end, Pending: true, Entitlements: benefits}
			pending, err := lifecycle.CreateMembership(ctx, &subscriptions.CreateMembershipParams{Prepared: accepted, UserID: buyer.id, PriceID: price.ID.UUID(), Rail: models.RailNMI, RailSubscriptionID: &railSub})
			require.NoError(t, err)
			require.Equal(t, models.StatusPending, pending.Status)
			requireSubscriptionBenefits(t, w, billing.SubscriptionID(pending.ID), benefits)
			_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{EntitlementsSpec: catalog.Value(map[string]*int{editedCatalogBenefit: nil})})
			require.NoError(t, err)
			w.advance(day)
			sale := w.nmi.RenewSchedule(railSub, true)
			active, err := lifecycle.CreateMembership(ctx, &subscriptions.CreateMembershipParams{UserID: buyer.id, PriceID: price.ID.UUID(), Rail: models.RailNMI, RailSubscriptionID: &railSub,
				CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &end, TransactionID: sale.TransactionID, Amount: price.UnitAmount, AmountProvided: true, Currency: "USD", PurchasedAt: &start})
			require.NoError(t, err)
			require.Equal(t, pending.ID, active.ID)
			require.Equal(t, models.StatusActive, active.Status)
			w.settle()
			requireSubscriptionBenefits(t, w, billing.SubscriptionID(active.ID), benefits)
			require.Equal(t, !empty, buyer.entitled(acceptedCatalogBenefit))
			require.False(t, buyer.entitled(editedCatalogBenefit))
		})
	}
}
