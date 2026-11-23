//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// The bulk roster read resolves unverified NMI rows only from a whole roster:
// a page NMI cuts short is refused.
func TestNMIRoster(t *testing.T) {
	const members = 201 // one past the bulk threshold
	unverified := billing.SubscriptionStatus("unverified")
	t.Run("OR-J15-S71/a-truncated-roster-page-resolves-nothing", func(t *testing.T) {
		a, old, book, now := lapsedNMIBook(t, members)
		// Import before the listener starts. With periodic work paused in the
		// fixture, only the operator's job can read the outstanding rows.
		const truncated = 1 << 20
		old.TruncateRoster(truncated)
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
		require.Len(t, imported.Imported, members)
		fleet := quietFleet(t, a.world)
		job, err := fleet.Insert(t.Context(), refreshMerchantJob{MerchantID: a.alpha.MerchantID().UUID()}, &river.InsertOpts{Queue: "provider_refresh", MaxAttempts: 5})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			current, err := fleet.JobGet(t.Context(), job.Job.ID)
			return err == nil && current.State == rivertype.JobStateRetryable
		}, 30*time.Second, 20*time.Millisecond, "the truncated roster leaves the manual refresh retryable")
		require.Less(t, old.RosterTruncations(), truncated, "a roster read meets the cut-short page")
		paid := *book.Subscriptions[0].PaidThrough
		for _, i := range []int{0, members - 1} {
			customer := book.Customers[i].Customer
			require.Never(t, func() bool {
				subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				return err != nil || len(subs.Items) != 1 || subs.Items[0].Status != unverified || !subs.Items[0].CurrentPeriodEndsAt.Equal(paid)
			}, 3*time.Second, 100*time.Millisecond, "a cut-short roster is no evidence: nothing is adopted or cancelled")
		}
		old.TruncateRoster(0)
		_, err = fleet.JobRetry(t.Context(), job.Job.ID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			current, err := fleet.JobGet(t.Context(), job.Job.ID)
			return err == nil && current.State == rivertype.JobStateCompleted
		}, 2*time.Minute, 50*time.Millisecond, "the same refresh completes and backfills the account once the roster is readable")
		for _, i := range []int{0, members - 1} {
			customer := book.Customers[i].Customer
			require.Eventually(t, func() bool {
				subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				return err == nil && len(subs.Items) == 1 && subs.Items[0].Status == billing.SubscriptionActive && subs.Items[0].CurrentPeriodEndsAt.After(now)
			}, 30*time.Second, 50*time.Millisecond, "the next pass reads the roster whole and resolves the rows")
		}
	})
}

// lapsedNMIBook prepares the archived account and a book whose members need
// verification. It leaves the fleet and listener off until the caller starts
// them, so the test controls what reads the book.
func lapsedNMIBook(t *testing.T, members int) (*authorityWorld, *nmimock.Mock, billing.DeclaredBilling, time.Time) {
	t.Helper()
	a, accounts := nmiAccountsWorld(t)
	old := accounts.cores["nmi-old-key"]
	old.AddPlan(nmimock.Plan{ID: "weekly", Name: "Weekly", Amount: "10.00", Days: 7})
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
	require.NoError(t, err)
	price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-weekly", PSP: "old", PlanID: "weekly", Currency: "USD"})
	require.NoError(t, err)
	require.NoError(t, a.engine.Close(context.Background()))
	m := a.cfg.Merchants["alpha"]
	m.PSPs = openrails.PSPs{"old": nmiPSP("old", true), "new": nmiPSP("new", false)}
	a.cfg.Merchants["alpha"] = m
	// A refresh pass verifies unverified rows only for a merchant whose
	// provider writes are not held, so the next whole read can happen.
	a.cfg.ProviderWriteMode = openrails.ProviderWritesFull
	a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
	require.NoError(t, err)
	a.alpha, err = declaredMerchant(a.engine, "alpha")
	require.NoError(t, err)
	armDestructive(t, a, true)

	now := a.clock.Now()
	started, paid := now.Add(-10*24*time.Hour), now.Add(-3*24*time.Hour)
	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "old"}}
	for i := range members {
		customer := billing.CustomerID(uuid.New())
		vault := old.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		id := old.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "weekly", Amount: "10.00", Days: 7, Created: started, NextBilling: paid.Add(7 * 24 * time.Hour), Order: fmt.Sprintf("lapsed-%d", i)})
		old.AddScheduleSale(id, paid)
		ref := billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: old.Vault(vault).BillingID}
		book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
		book.PaymentMethods = append(book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: customer, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: ref.RailMethodRef, CreatedAt: started})
		book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: fmt.Sprintf("lapsed-%d", i), Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: id,
			StartedAt: started, PaidThrough: &paid, PaymentMethod: &ref,
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: id, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: paid}})
	}
	return a, old, book, now
}
