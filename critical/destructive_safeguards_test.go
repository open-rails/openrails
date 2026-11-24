//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/stretchr/testify/require"
)

func TestDestructiveSafeguards(t *testing.T) {
	t.Run("OR-J34-S32/a-spent-solana-schedule-waits-past-due-while-the-switch-is-off", func(t *testing.T) {
		// This policy retries no weekly renewal: the first refused pull spends it.
		s := newSolanaWorld(t, true, solanaFixtureOptions{dunning: &billing.DunningPolicy{Tiers: []billing.DunningTier{{}}}})
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		// The wallet can no longer pay the renewal.
		s.node.Fund(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint), 1)
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		sends := s.wire.sends()
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.sends() > sends }, 20*time.Second, 20*time.Millisecond, "the due pull is sent and the chain refuses it")
		held := func() []billing.Finding {
			page, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.terminal_outcome.held"})
			require.NoError(t, err)
			return page.Items
		}
		require.Eventually(t, func() bool { return len(held()) == 1 }, 20*time.Second, 20*time.Millisecond, "the spent schedule's cancellation is refused")
		settleOperations(t, fleet)
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionPastDue, current.Status, "with the switch off a spent schedule cancels nothing")
		require.True(t, s.entitled(t, sub.CustomerID, "chain:paid"), "and the member keeps access")
	})

	t.Run("OR-J34-S33/a-billing-import-cancels-only-through-the-switch-the-cap-and-an-attributed-admission", func(t *testing.T) {
		a, p := newNMIWorld(t)
		p.core.AddPlan(nmimock.Plan{ID: "weekly", Name: "Weekly", Amount: "10.00", Days: 7})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-weekly", PSP: "nmi", PlanID: "weekly", Currency: "USD"})
		require.NoError(t, err)
		started, due := a.clock.Now(), a.clock.Now().Add(7*24*time.Hour)
		members := make([]billing.CustomerID, 6)
		for i := range members {
			members[i] = billing.CustomerID(uuid.New())
		}
		schedule := func(i int) string { return fmt.Sprintf("book-schedule-%d", i) }
		// The host's book: its first canceled members declared canceled at
		// asOf, the rest live.
		book := func(asOf time.Time, canceled int) billing.DeclaredBilling {
			out := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "nmi"}}
			for i, customer := range members {
				sub := billing.DeclaredSubscription{SourceID: schedule(i), Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: schedule(i), StartedAt: started, PaidThrough: &due}
				if i < canceled {
					sub.Cancel = billing.CancelEvidence{Kind: "user_canceled", At: asOf.Add(-time.Minute)}
				} else {
					sub.AcceptedSchedule = &billing.DeclaredProviderSchedule{SourceReference: schedule(i), Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: due}
				}
				out.Customers = append(out.Customers, billing.DeclaredCustomer{Customer: customer})
				out.Subscriptions = append(out.Subscriptions, sub)
			}
			return out
		}
		land := func(b billing.DeclaredBilling) {
			t.Helper()
			res, err := a.alpha.ImportBilling(t.Context(), b)
			require.NoError(t, err)
			require.Empty(t, res.Blocked, "%v", res.Reasons)
		}
		status := func(i int) billing.SubscriptionStatus {
			t.Helper()
			subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: members[i]})
			require.NoError(t, err)
			require.Len(t, subs.Items, 1)
			return subs.Items[0].Status
		}
		land(book(started, 0))
		for i := range members {
			require.Equal(t, billing.SubscriptionActive, status(i))
		}

		// With the switch off the declared cancel waits, access kept.
		land(book(started.Add(time.Hour), 1))
		require.Equal(t, billing.SubscriptionUnverified, status(0), "the switch is off: the book cancels nothing")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: members[0], Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["nmi:member"])

		// Four of a book of six pass the PSP's cancellation cap: none lands.
		armDestructive(t, a, true)
		land(book(started.Add(2*time.Hour), 4))
		for i := range 4 {
			require.NotEqual(t, billing.SubscriptionCanceled, status(i), "member %d", i)
		}
		capped, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.cancellation.capped"})
		require.NoError(t, err)
		require.NotEmpty(t, capped.Items, "the capped book is an operator finding")

		// Three are within it: each is an admitted action of the importer.
		land(book(started.Add(3*time.Hour), 3))
		for i := range members {
			require.Equal(t, i < 3, status(i) == billing.SubscriptionCanceled, "member %d", i)
		}
		rows, err := a.pool.Query(t.Context(), "SELECT target, origin, actor_issuer, state FROM "+pgx.Identifier{a.schema, "destructive_actions"}.Sanitize()+
			" WHERE merchant_id = $1 AND kind = 'cancel_subscription' ORDER BY target", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		type action struct{ target, origin, issuer, state string }
		actions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (action, error) {
			var out action
			return out, row.Scan(&out.target, &out.origin, &out.issuer, &out.state)
		})
		require.NoError(t, err)
		require.Len(t, actions, 3, "one admitted action per canceled membership")
		for i, got := range actions {
			require.True(t, strings.HasSuffix(got.target, "/nmi/"+schedule(i)), got.target)
			require.Equal(t, action{got.target, "host", "openrails:host", "succeeded"}, got, "the in-process host's own work, recorded")
		}
	})

	t.Run("OR-J34-S34/a-staff-card-removal-spends-no-customer-action-budget", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		save := func() billing.PaymentMethod {
			t.Helper()
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var method billing.PaymentMethod
			require.NoError(t, json.Unmarshal(raw, &method))
			return method
		}
		count := func(table, where string) int {
			t.Helper()
			var n int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, table}.Sanitize()+" WHERE merchant_id = $1 AND "+where, a.alpha.MerchantID().UUID()).Scan(&n))
			return n
		}
		method := save()
		status, raw := a.request(t, http.MethodDelete, "/v1/admin/customers/"+method.CustomerID.String()+"/payment-methods/"+method.ID.String(), a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.Zero(t, count("user_destructive_reservations", "true"), "a staff removal is no customer action")
		require.Equal(t, 1, count("destructive_actions", "kind = 'delete_payment_method'"), "it is the staff member's admitted action")

		own := save()
		status, raw = a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+own.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.Equal(t, 1, count("user_destructive_reservations", "kind = 'delete_payment_method'"), "the customer's own removal spends the customer budget")
		require.Equal(t, 1, count("destructive_actions", "kind = 'delete_payment_method'"))
	})
}
