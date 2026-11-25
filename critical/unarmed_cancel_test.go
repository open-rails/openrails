//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// The arming switch guards OpenRails' own destruction and staff cancels, never
// a customer's own cancel: only the customer budget refuses that.
func TestUnarmedCancels(t *testing.T) {
	customerCancel := func(t *testing.T, a *authorityWorld, sub billing.SubscriptionID) billing.Subscription {
		t.Helper()
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Moving on from it"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var canceled billing.Subscription
		require.NoError(t, json.Unmarshal(raw, &canceled))
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		require.Equal(t, "user", *canceled.CancelType)
		return canceled
	}
	accessAt := func(t *testing.T, a *authorityWorld, customer billing.CustomerID, entitlement string, at time.Time) bool {
		t.Helper()
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}, At: at})
		require.NoError(t, err)
		return access.Entitlements[entitlement]
	}
	noHeldCancel := func(t *testing.T, a *authorityWorld) {
		t.Helper()
		held, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: subscriptions.ProviderCancelHeldFindingType})
		require.NoError(t, err)
		require.Empty(t, held.Items)
	}

	t.Run("OR-J17-S13/a-customers-own-cancel-stops-the-provider-on-an-unarmed-merchant", func(t *testing.T) {
		t.Run("nmi", func(t *testing.T) {
			a, p, sub := nmiMembershipWithAccess(t, catalog.Value(168))
			schedule, due := *sub.RailSubscriptionID, *sub.CurrentPeriodEndsAt
			fleet := quietFleet(t, a.world)
			a.clock.Advance(time.Hour)
			canceled := customerCancel(t, a, sub.ID)
			require.True(t, canceled.Resumable, "the NMI delete waits inside the paid period")
			require.Equal(t, due, *canceled.CurrentPeriodEndsAt)
			deleteAt := due.Add(-subscriptions.NMIDeleteSafetyMargin)
			require.Equal(t, deleteAt, *canceled.DeletionScheduledAt)
			require.True(t, p.core.ScheduleLive(schedule))
			a.clock.Advance(deleteAt.Sub(a.clock.Now()) + time.Minute)
			require.Eventually(t, func() bool {
				wakeJobs(t, fleet, "openrails.provider_operation")
				return !p.core.ScheduleLive(schedule)
			}, 20*time.Second, 100*time.Millisecond, "the disarmed merchant's NMI schedule is deleted")
			require.Equal(t, 1, p.core.ScheduleDeletes(schedule))
			ended, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionCanceled, ended.Status)
			require.Equal(t, due, *ended.CurrentPeriodEndsAt)
			require.True(t, accessAt(t, a, sub.CustomerID, "nmi:member", due.Add(-time.Microsecond)), "access runs to the paid boundary")
			require.False(t, accessAt(t, a, sub.CustomerID, "nmi:member", due), "and ends there")
			noHeldCancel(t, a)
			p.money(t, 1, 1000)
		})
		t.Run("ccbill", func(t *testing.T) {
			dataLink := ccbillmock.New()
			t.Cleanup(dataLink.Close)
			t.Cleanup(func() { require.Empty(t, dataLink.Unexpected()) })
			a := newAuthorityWorld(t, func(w *world) { declareCCBill(w, dataLink.URL()); fullWrites(w) })
			price := ccbillMemberPrice(t, a.alpha)
			alice := customerOf(a.alice)
			now := a.clock.Now().UTC().Truncate(time.Second)
			paid := now.Add(5 * 24 * time.Hour)
			const ref, sale = "556677889900117788", "0812233445566770011"
			importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(alice, price, ref, sale, paid))
			dataLink.AddMember(ccbillmock.Member{SubscriptionID: ref, Username: "alice-ccbill", Email: "alice-ccbill@critical.invalid", Signup: paid.Add(-720 * time.Hour), Rebill: &paid})
			page, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: alice})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			sub := page.Items[0]
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			fleet := quietFleet(t, a.world)
			canceled := customerCancel(t, a, sub.ID)
			require.False(t, canceled.Resumable, "CCBill has no resume")
			require.Equal(t, paid, *canceled.CurrentPeriodEndsAt)
			require.Eventually(t, func() bool {
				wakeJobs(t, fleet, "openrails.provider_operation")
				return dataLink.Canceled(ref) == 1
			}, 20*time.Second, 100*time.Millisecond, "the disarmed merchant's CCBill rebilling is stopped")
			require.Len(t, dataLink.Mutations(), 1, "one cancel reaches CCBill")
			ended, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionCanceled, ended.Status)
			require.Equal(t, paid, *ended.CurrentPeriodEndsAt)
			require.True(t, accessAt(t, a, alice, "ccbill:member", a.clock.Now()), "access runs to the paid boundary")
			require.False(t, accessAt(t, a, alice, "ccbill:member", paid), "and ends there")
			noHeldCancel(t, a)
		})
	})

	// Five other customers' own cancels spend a small merchant's hour; each
	// has a token and a subscription of its own.
	others := func(t *testing.T, a *authorityWorld) ([]billing.CustomerID, []string) {
		t.Helper()
		var customers []billing.CustomerID
		var tokens []string
		for range 5 {
			user := authtest.NewUser(t, a.auth)
			customers, tokens = append(customers, customerOf(user)), append(tokens, signIn(t, a.auth, user).AccessToken)
		}
		return customers, tokens
	}
	overBudget := func(t *testing.T, a *authorityWorld, sub billing.SubscriptionID) {
		t.Helper()
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Moving on from it"})
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.Contains(t, string(raw), `"code":"rate_limit_exceeded"`)
		kept, err := a.alpha.GetSubscription(t.Context(), sub)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, kept.Status, "a refused cancel changes nothing")
		require.Nil(t, kept.DeletionScheduledAt)
		var operations int
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+
			" WHERE merchant_id = $1 AND subscription_id = $2", a.alpha.MerchantID().UUID(), sub.UUID()).Scan(&operations))
		require.Zero(t, operations, "a refused cancel queues nothing for the provider")
	}

	t.Run("OR-J17-S15/over-the-customer-budget-a-customers-own-cancel-reaches-no-provider", func(t *testing.T) {
		t.Run("nmi", func(t *testing.T) {
			a, p := newNMIWorld(t)
			customers, tokens := others(t, a)
			schedules := map[billing.CustomerID]string{}
			sub, _ := importNMISchedule(t, a, p, catalog.Field[int]{}, func(book *billing.DeclaredBilling) {
				started, due, price := book.AsOf, *book.Subscriptions[0].PaidThrough, book.Subscriptions[0].Price
				for i, customer := range customers {
					vaultID := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
					vault := p.core.Vault(vaultID)
					schedule := p.core.AddSchedule(nmimock.Schedule{Vault: vaultID, Plan: "weekly", Amount: "10.00", Days: 7, Created: started, NextBilling: due, Order: fmt.Sprintf("other-nmi-membership-%d", i)})
					initial := p.core.AddScheduleSale(schedule, started)
					schedules[customer] = schedule
					method := billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID}
					book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
					book.PaymentMethods = append(book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: customer, Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID, RecurringTransactionID: initial.TransactionID, CreatedAt: started, Card: &billing.CardDetails{Brand: ptr("visa"), Last4: ptr("4242")}})
					book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: fmt.Sprintf("other-nmi-membership-%d", i), Customer: customer, Price: price, Rail: "nmi", RailSubscriptionID: schedule, StartedAt: started, PaidThrough: &due, PaymentMethod: &method,
						AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: schedule, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: due}})
					book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: fmt.Sprintf("other-nmi-receipt-%d", i), RailSubscriptionID: schedule, TransactionID: initial.TransactionID, Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: started, NextBillingAt: &due})
				}
				require.NoError(t, p.persist(t.Context()))
			})
			for i, customer := range customers {
				page, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+page.Items[0].ID.String()+"/cancel", tokens[i], map[string]any{"reason": "Stop renewing"})
				require.Equal(t, http.StatusOK, status, string(raw))
			}
			overBudget(t, a, sub.ID)
			// The others' accepted deletes reach NMI; the refused one never does.
			fleet := quietFleet(t, a.world)
			a.clock.Advance(sub.CurrentPeriodEndsAt.Add(-subscriptions.NMIDeleteSafetyMargin).Sub(a.clock.Now()) + time.Minute)
			require.Eventually(t, func() bool {
				wakeJobs(t, fleet, "openrails.provider_operation")
				for _, schedule := range schedules {
					if p.core.ScheduleLive(schedule) {
						return false
					}
				}
				return true
			}, 20*time.Second, 100*time.Millisecond, "the accepted cancels' schedules are deleted")
			require.True(t, p.core.ScheduleLive(*sub.RailSubscriptionID))
			require.Zero(t, p.core.ScheduleDeletes(*sub.RailSubscriptionID), "NMI hears nothing of the refused cancel")
			kept, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionActive, kept.Status)
			p.money(t, 6, 6000)
		})
		t.Run("ccbill", func(t *testing.T) {
			dataLink := ccbillmock.New()
			t.Cleanup(dataLink.Close)
			t.Cleanup(func() { require.Empty(t, dataLink.Unexpected()) })
			a := newAuthorityWorld(t, func(w *world) { declareCCBill(w, dataLink.URL()); fullWrites(w) })
			price := ccbillMemberPrice(t, a.alpha)
			customers, tokens := others(t, a)
			now := a.clock.Now().UTC().Truncate(time.Second)
			paid := now.Add(5 * 24 * time.Hour)
			land := importLegacyCCBill(t, a.alpha, now)
			member := func(customer billing.CustomerID, i int) billing.SubscriptionID {
				t.Helper()
				ref, sale := fmt.Sprintf("5566778899001188%02d", i), fmt.Sprintf("08122334455667700%02d", i)
				land(legacyCCBillMember(customer, price, ref, sale, paid))
				dataLink.AddMember(ccbillmock.Member{SubscriptionID: ref, Username: fmt.Sprintf("ccbill-member-%d", i), Email: fmt.Sprintf("ccbill-member-%d@critical.invalid", i), Signup: paid.Add(-720 * time.Hour), Rebill: &paid})
				page, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				return page.Items[0].ID
			}
			alice := member(customerOf(a.alice), 0)
			for i, customer := range customers {
				sub := member(customer, i+1)
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.String()+"/cancel", tokens[i], map[string]any{"reason": "Stop renewing"})
				require.Equal(t, http.StatusOK, status, string(raw))
			}
			overBudget(t, a, alice)
			// The others' accepted cancels reach CCBill; the refused one never does.
			fleet := quietFleet(t, a.world)
			require.Eventually(t, func() bool {
				wakeJobs(t, fleet, "openrails.provider_operation")
				return len(dataLink.Mutations()) == len(customers)
			}, 20*time.Second, 100*time.Millisecond, "the accepted cancels reach CCBill")
			require.Zero(t, dataLink.Canceled("556677889900118800"), "CCBill hears nothing of the refused cancel")
			for i := range customers {
				require.Equal(t, 1, dataLink.Canceled(fmt.Sprintf("5566778899001188%02d", i+1)))
			}
			kept, err := a.alpha.GetSubscription(t.Context(), alice)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionActive, kept.Status)
		})
	})

	t.Run("OR-J17-S14/a-staff-cancel-on-an-unarmed-merchant-is-still-held", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		schedule := *sub.RailSubscriptionID
		_, err := a.alphaRemote.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "support request"})
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, http.StatusConflict, refused.Status)
		require.Equal(t, billing.CodeProviderCancelHeld, refused.Code)
		kept, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, kept.Status, "a held staff cancel changes nothing")
		require.Nil(t, kept.DeletionScheduledAt)
		require.True(t, p.core.ScheduleLive(schedule))
		require.Zero(t, p.core.ScheduleDeletes(schedule))
		held, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: subscriptions.ProviderCancelHeldFindingType})
		require.NoError(t, err)
		require.Len(t, held.Items, 1)
		require.Equal(t, sub.ID.String(), held.Items[0].Evidence["subscription_id"])
		require.Equal(t, false, held.Items[0].Evidence["account_deletion"])
		// The customer's own cancel of it is not held, and settles the finding.
		customerCancel(t, a, sub.ID)
		noHeldCancel(t, a)
	})
}
