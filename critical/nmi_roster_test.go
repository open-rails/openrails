//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
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
		a, old, fleet, book, now := lapsedNMIBook(t, members)
		// Every roster read is cut short until the test lifts the fault, so
		// the pull lane and the bulk read both meet it whichever runs first.
		const truncated = 1 << 20
		old.TruncateRoster(truncated)
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
		require.Len(t, imported.Imported, members)
		require.Eventually(t, func() bool { return old.RosterTruncations() < truncated }, 30*time.Second, 50*time.Millisecond, "the bulk read meets the cut-short page")
		paid := *book.Subscriptions[0].PaidThrough
		for _, i := range []int{0, members - 1} {
			customer := book.Customers[i].Customer
			require.Never(t, func() bool {
				subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				return err != nil || len(subs.Items) != 1 || subs.Items[0].Status != unverified || !subs.Items[0].CurrentPeriodEndsAt.Equal(paid)
			}, 3*time.Second, 100*time.Millisecond, "a cut-short roster is no evidence: nothing is adopted or cancelled")
		}
		old.TruncateRoster(0)
		refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
		for _, i := range []int{0, members - 1} {
			customer := book.Customers[i].Customer
			require.Eventually(t, func() bool {
				subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				return err == nil && len(subs.Items) == 1 && subs.Items[0].Status == billing.SubscriptionActive && subs.Items[0].CurrentPeriodEndsAt.After(now)
			}, 30*time.Second, 50*time.Millisecond, "the next pass reads the roster whole and resolves the rows")
		}
	})
}

// lapsedNMIBook boots the archived-account world of OR-J15-S07 and declares a
// lapsed book of members schedules the archived account has since charged,
// so every imported row is unverified and only the verifier's bulk read
// resolves it.
func lapsedNMIBook(t *testing.T, members int) (*authorityWorld, *nmimock.Mock, *river.Client[pgx.Tx], billing.DeclaredBilling, time.Time) {
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
	a.alpha, err = a.engine.MerchantBySlug("alpha")
	require.NoError(t, err)
	armDestructive(t, a, true)
	fleet := hostFleet(t, a.world, 2)
	require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))

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
	return a, old, fleet, book, now
}
