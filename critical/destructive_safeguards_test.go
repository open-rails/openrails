//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/failpoint"
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
		t.Run("host", func(t *testing.T) {
			b := newNMIBook(t)
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
					sub := b.live(customer, schedule(i))
					if i < canceled {
						sub.AcceptedSchedule, sub.Cancel = nil, billing.CancelEvidence{Kind: "user_canceled", At: asOf.Add(-time.Minute)}
					}
					out.Customers = append(out.Customers, billing.DeclaredCustomer{Customer: customer})
					out.Subscriptions = append(out.Subscriptions, sub)
				}
				return out
			}
			b.land(b.a.alpha, book(b.started, 0))
			for i := range members {
				require.Equal(t, billing.SubscriptionActive, b.status(members[i]))
			}

			// With the switch off the declared cancel waits, access kept.
			b.land(b.a.alpha, book(b.started.Add(time.Hour), 1))
			require.Equal(t, billing.SubscriptionUnverified, b.status(members[0]), "the switch is off: the book cancels nothing")
			access, err := b.a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: members[0], Entitlements: []string{"nmi:member"}})
			require.NoError(t, err)
			require.True(t, access.Entitlements["nmi:member"])

			// Four of a book of six pass the PSP's cancellation cap: none lands.
			armDestructive(t, b.a, true)
			b.land(b.a.alpha, book(b.started.Add(2*time.Hour), 4))
			for i := range 4 {
				require.NotEqual(t, billing.SubscriptionCanceled, b.status(members[i]), "member %d", i)
			}
			capped, err := b.a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.cancellation.capped"})
			require.NoError(t, err)
			require.NotEmpty(t, capped.Items, "the capped book is an operator finding")

			// Three are within it: each is an admitted action of the importer.
			b.land(b.a.alpha, book(b.started.Add(3*time.Hour), 3))
			for i := range members {
				require.Equal(t, i < 3, b.status(members[i]) == billing.SubscriptionCanceled, "member %d", i)
			}
			require.Len(t, b.actions(t, ""), 3, "held cancels admit nothing")
			actions := b.actions(t, "succeeded")
			require.Len(t, actions, 3, "one admitted action per canceled membership")
			for i, got := range actions {
				require.True(t, strings.HasSuffix(got.target, "/nmi/"+schedule(i)), got.target)
				require.Equal(t, bookAction{got.target, "host", "openrails:host", "succeeded"}, got, "the in-process host's own work, recorded")
			}
		})

		t.Run("staff", func(t *testing.T) {
			// The merchant's budget is 50 an hour; an actor's stays 5.
			b := newNMIBook(t, func(w *world) {
				w.cfg.DestructiveActionLimits = openrails.DestructiveActionLimitsConfig{MerchantPerHour: 50, MerchantPerDay: 100}
			})
			armDestructive(t, b.a, true)
			asOf := b.started.Add(10 * 24 * time.Hour)

			// A declined charge that spends the dunning schedule is one admitted
			// cancel; its provider stop carries the action.
			dunned := billing.CustomerID(uuid.New())
			b.land(b.a.alphaRemote, b.dunnedBook(asOf, dunned, "staff-dunned"))
			require.Equal(t, billing.SubscriptionCanceled, b.status(dunned))
			require.Len(t, b.stops(t), 1, "the canceled schedule's provider stop is the book's action")

			// Forty owed provider stops spend the merchant's budget, not the
			// actor's.
			owed := func(n int, prefix string) billing.DeclaredBilling {
				out := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "nmi"}}
				for i := range n {
					customer := billing.CustomerID(uuid.New())
					out.Customers = append(out.Customers, billing.DeclaredCustomer{Customer: customer})
					out.Subscriptions = append(out.Subscriptions, billing.DeclaredSubscription{SourceID: fmt.Sprintf("%s-%02d", prefix, i), Customer: customer, Price: b.price, Rail: "nmi",
						RailSubscriptionID: fmt.Sprintf("%s-%02d", prefix, i), StartedAt: b.started, Cancel: billing.CancelEvidence{Kind: "user_canceled", At: asOf.Add(-time.Hour), ScheduleLive: true}})
				}
				return out
			}
			forty := owed(40, "owed")
			// A landing that fails after its admission leaves no action
			// spending the budget.
			lost := errors.New("landing lost before its commit")
			remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
				if site.Point == failpoint.BeforeComplete && site.Kind == "billing_import" {
					return lost
				}
				return nil
			})
			t.Cleanup(remove)
			_, err := b.a.alphaRemote.ImportBilling(t.Context(), forty)
			require.Error(t, err)
			remove()
			abandoned := b.actions(t, "no_effect")
			require.Len(t, abandoned, 40, "the abandoned landing's actions had no effect")
			b.land(b.a.alphaRemote, forty)
			require.Len(t, b.stops(t), 41)
			started := b.actions(t, "started")
			require.Len(t, started, 41, "each stop waits for its provider effect")
			for _, got := range started {
				require.Equal(t, "staff", got.origin)
				require.NotEqual(t, "openrails:host", got.issuer, "the application's own name")
			}

			// Ten more pass the merchant's hour: refused, nothing admitted.
			_, err = b.a.alphaRemote.ImportBilling(t.Context(), owed(10, "late"))
			status, code := refusalCode(t, err)
			require.Equal(t, http.StatusTooManyRequests, status)
			require.Equal(t, billing.CodeRateLimitExceeded, code)
			require.Len(t, b.actions(t, ""), 81, "a refused book reserves nothing")
			require.Len(t, b.stops(t), 41)
		})
	})

	t.Run("OR-J34-S35/a-declined-charge-that-spends-dunning-in-a-book-is-an-admitted-cancel", func(t *testing.T) {
		b := newNMIBook(t)
		asOf := b.started.Add(10 * 24 * time.Hour)
		// With the switch off the spent schedule is held, access kept.
		held := billing.CustomerID(uuid.New())
		b.land(b.a.alpha, b.dunnedBook(asOf, held, "held-dunned"))
		require.NotEqual(t, billing.SubscriptionCanceled, b.status(held), "the switch is off: the book cancels nothing")
		findings, err := b.a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.terminal_outcome.held"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Empty(t, b.stops(t))

		// With it on, the cancel is the book's action and so is its stop.
		armDestructive(t, b.a, true)
		ended := billing.CustomerID(uuid.New())
		b.land(b.a.alpha, b.dunnedBook(asOf, ended, "ended-dunned"))
		require.Equal(t, billing.SubscriptionCanceled, b.status(ended))
		require.Len(t, b.stops(t), 1)
		started := b.actions(t, "started")
		require.Len(t, started, 1)
		require.True(t, strings.HasSuffix(started[0].target, "/nmi/ended-dunned"), started[0].target)
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

// nmiBook is a merchant whose host or staff lands billing books of NMI
// schedules on one weekly provider price.
type nmiBook struct {
	t       *testing.T
	a       *authorityWorld
	price   billing.PriceID
	started time.Time
	due     time.Time
}

func newNMIBook(t *testing.T, configure ...func(*world)) *nmiBook {
	t.Helper()
	a, p := newNMIWorld(t, configure...)
	p.core.AddPlan(nmimock.Plan{ID: "weekly", Name: "Weekly", Amount: "10.00", Days: 7})
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
	require.NoError(t, err)
	price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-weekly", PSP: "nmi", PlanID: "weekly", Currency: "USD"})
	require.NoError(t, err)
	started := a.clock.Now()
	return &nmiBook{t: t, a: a, price: price.ID, started: started, due: started.Add(7 * 24 * time.Hour)}
}

// live is a schedule paid through the first week.
func (b *nmiBook) live(customer billing.CustomerID, ref string) billing.DeclaredSubscription {
	return billing.DeclaredSubscription{SourceID: ref, Customer: customer, Price: b.price, Rail: "nmi", RailSubscriptionID: ref, StartedAt: b.started, PaidThrough: &b.due,
		AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ref, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: b.due}}
}

// dunnedBook declares a schedule two declines into its three, whose renewal
// NMI declined again within the dunning window: that decline spends it.
func (b *nmiBook) dunnedBook(asOf time.Time, customer billing.CustomerID, ref string) billing.DeclaredBilling {
	paid := asOf.Add(-2 * time.Hour)
	retried := asOf.Add(-90 * time.Minute)
	sub := billing.DeclaredSubscription{SourceID: ref, Customer: customer, Price: b.price, Rail: "nmi", RailSubscriptionID: ref, StartedAt: paid.Add(-14 * 24 * time.Hour), PaidThrough: &paid,
		AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ref, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: paid},
		Dunning:          &billing.DunningEvidence{Retries: 2, LastRetryAt: &retried, ScheduleLive: true}}
	decline := billing.DeclaredTransaction{RailSubscriptionID: ref, TransactionID: fmt.Sprintf("9%09d", crc32.ChecksumIEEE([]byte(ref))%1_000_000_000), Type: "decline", Amount: 10_000_000, Currency: "USD", OccurredAt: asOf.Add(-time.Hour)}
	return billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "nmi"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []billing.DeclaredSubscription{sub}, Transactions: []billing.DeclaredTransaction{decline}}
}

func (b *nmiBook) land(client *openrails.Client, book billing.DeclaredBilling) {
	b.t.Helper()
	res, err := client.ImportBilling(b.t.Context(), book)
	require.NoError(b.t, err)
	require.Empty(b.t, res.Blocked, "%v", res.Reasons)
}

func (b *nmiBook) status(customer billing.CustomerID) billing.SubscriptionStatus {
	b.t.Helper()
	subs, err := b.a.alpha.ListSubscriptions(b.t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(b.t, err)
	require.Len(b.t, subs.Items, 1)
	return subs.Items[0].Status
}

type bookAction struct{ target, origin, issuer, state string }

// actions are the merchant's cancel_subscription actions in state ("" for
// every state), by target.
func (b *nmiBook) actions(t *testing.T, state string) []bookAction {
	t.Helper()
	rows, err := b.a.pool.Query(t.Context(), "SELECT target, origin, actor_issuer, state FROM "+pgx.Identifier{b.a.schema, "destructive_actions"}.Sanitize()+
		" WHERE merchant_id = $1 AND kind = 'cancel_subscription' AND ($2 = '' OR state = $2) ORDER BY target", b.a.alpha.MerchantID().UUID(), state)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (bookAction, error) {
		var got bookAction
		return got, row.Scan(&got.target, &got.origin, &got.issuer, &got.state)
	})
	require.NoError(t, err)
	return out
}

// stops are the queued NMI schedule deletes, each bound to its action.
func (b *nmiBook) stops(t *testing.T) []string {
	t.Helper()
	rows, err := b.a.pool.Query(t.Context(), "SELECT id::text FROM "+pgx.Identifier{b.a.schema, "provider_intents"}.Sanitize()+
		" WHERE merchant_id = $1 AND intent_type = 'nmi_delete_subscription' AND destructive_action_id IS NOT NULL", b.a.alpha.MerchantID().UUID())
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	var unbound int
	require.NoError(t, b.a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{b.a.schema, "provider_intents"}.Sanitize()+
		" WHERE merchant_id = $1 AND intent_type = 'nmi_delete_subscription' AND destructive_action_id IS NULL", b.a.alpha.MerchantID().UUID()).Scan(&unbound))
	require.Zero(t, unbound, "every provider stop names its admitted action")
	return out
}
