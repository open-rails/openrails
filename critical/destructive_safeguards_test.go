//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/stretchr/testify/require"
)

func TestDestructiveSafeguards(t *testing.T) {
	t.Run("OR-J34-S39/a-spent-solana-schedule-waits-past-due-while-the-switch-is-off", func(t *testing.T) {
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

	t.Run("OR-J34-S40/a-billing-import-cancels-only-through-the-switch-the-cap-and-an-attributed-admission", func(t *testing.T) {
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
			forty := b.owed(asOf, 40, "owed")
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

			// Ten more pass the merchant's hour: refused for later, nothing
			// admitted, and no incident freezes the merchant.
			_, err = b.a.alphaRemote.ImportBilling(t.Context(), b.owed(asOf, 10, "late"))
			status, code := refusalCode(t, err)
			require.Equal(t, http.StatusTooManyRequests, status)
			require.Equal(t, billing.CodeRateLimitExceeded, code)
			var refused *billing.StatusError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "3600", refused.RetryAfter, "it may be tried again within the hour")
			require.Len(t, b.actions(t, ""), 81, "a refused book reserves nothing")
			require.Len(t, b.stops(t), 41)
			holds, err := b.a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
			require.NoError(t, err)
			require.Empty(t, holds.Items, "a book too large for the budget is no anomaly")
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
		require.NotNil(t, findings.Items[0].RecommendedAction)
		require.Contains(t, *findings.Items[0].RecommendedAction, "Turn the destructive-action switch on", "the action names what lets it end")
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

	t.Run("OR-J34-S36/a-lost-admission-answer-leaves-no-action-spending-the-budget", func(t *testing.T) {
		b := newNMIBook(t)
		book := b.owed(b.started.Add(10*24*time.Hour), 3, "lost")
		// The admission commits; its answer never reaches the import.
		lost := errors.New("admission answer lost")
		remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
			if site.Point == failpoint.Committed && site.Kind == "destructive_admission" {
				return lost
			}
			return nil
		})
		t.Cleanup(remove)
		_, err := b.a.alphaRemote.ImportBilling(t.Context(), book)
		require.Error(t, err)
		remove()
		require.Empty(t, b.actions(t, "started"), "an action whose admission answer was lost stays counted for nothing")
		require.Len(t, b.actions(t, "no_effect"), 3)
		require.Empty(t, b.stops(t))
		b.land(b.a.alphaRemote, book)
		require.Len(t, b.actions(t, "started"), 3)
		require.Len(t, b.stops(t), 3)
	})

	t.Run("OR-J34-S37/a-merchants-books-land-one-at-a-time", func(t *testing.T) {
		b := newNMIBook(t)
		book := b.owed(b.started.Add(10*24*time.Hour), 3, "race")
		// A pauses between its admission and its landing; B lands the same
		// book and loses its landing. The in-process handle carries no
		// context value to the engine, so the attempts are told apart by
		// order: the first admission is A's, a landing while A waits is B's,
		// and after A resumes its own landing comes first.
		paused, resume := make(chan struct{}), make(chan struct{})
		var pause sync.Once
		var mu sync.Mutex
		resumed, aLanded := false, false
		lostB := errors.New("B's landing lost before its commit")
		remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
			switch {
			case site.Point == failpoint.Committed && site.Kind == "destructive_admission":
				pause.Do(func() {
					close(paused)
					<-resume
				})
			case site.Point == failpoint.BeforeComplete && site.Kind == "billing_import":
				mu.Lock()
				defer mu.Unlock()
				if resumed && !aLanded {
					aLanded = true
					return nil
				}
				return lostB
			}
			return nil
		})
		t.Cleanup(remove)
		var once sync.Once
		release := func() {
			once.Do(func() {
				mu.Lock()
				resumed = true
				mu.Unlock()
				close(resume)
			})
		}
		t.Cleanup(release)
		errA, errB := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := b.a.alpha.ImportBilling(t.Context(), book)
			errA <- err
		}()
		select {
		case <-paused:
		case <-time.After(30 * time.Second):
			t.Fatal("A never reached its landing")
		}
		go func() {
			_, err := b.a.alpha.ImportBilling(t.Context(), book)
			errB <- err
		}()
		// B waits for A's book, or (unserialized) runs to its end first.
		var bErr error
		bDone := false
		require.Eventually(t, func() bool {
			select {
			case bErr = <-errB:
				bDone = true
				return true
			default:
				return b.waitingOn("name: LockBillingImport")
			}
		}, 30*time.Second, 20*time.Millisecond)
		release()
		require.NoError(t, <-errA, "no other attempt's abandon finishes A's actions")
		if !bDone {
			bErr = <-errB
		}
		require.Error(t, bErr)
		remove()
		require.Len(t, b.actions(t, "started"), 3, "A's stops each wait for their provider effect")
		require.Len(t, b.stops(t), 3)
	})

	t.Run("OR-J34-S38/an-abandon-waiting-on-its-actions-stalls-no-other-admission", func(t *testing.T) {
		b := newNMIBook(t)
		method := b.saveCard(t)
		book := b.owed(b.started.Add(10*24*time.Hour), 1, "stalled")
		// While the landing holds its action, another transaction queues for
		// it; the landing is lost, and that transaction keeps the action from
		// the import's abandon.
		locked := make(chan pgx.Tx, 1)
		var once sync.Once
		remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
			if site.Point != failpoint.BeforeComplete || site.Kind != "billing_import" {
				return nil
			}
			var hookErr error
			once.Do(func() {
				tx, err := b.a.pool.Begin(context.WithoutCancel(ctx))
				if err != nil {
					hookErr = err
					return
				}
				go func() {
					_, err := tx.Exec(context.WithoutCancel(ctx), "SELECT /* or-j34-s38 */ id FROM "+pgx.Identifier{b.a.schema, "destructive_actions"}.Sanitize()+
						" WHERE merchant_id = $1 AND kind = 'cancel_subscription' AND state = 'started' FOR UPDATE", b.a.alpha.MerchantID().UUID())
					if err != nil {
						_ = tx.Rollback(context.Background())
						tx = nil
					}
					locked <- tx
				}()
				deadline := time.Now().Add(20 * time.Second)
				for !b.waitingOn("or-j34-s38") && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				hookErr = errors.New("landing lost before its commit")
			})
			return hookErr
		})
		t.Cleanup(remove)
		errA := make(chan error, 1)
		go func() {
			_, err := b.a.alpha.ImportBilling(t.Context(), book)
			errA <- err
		}()
		var holder pgx.Tx
		select {
		case holder = <-locked:
		case <-time.After(30 * time.Second):
			t.Fatal("the landing never reached its commit")
		}
		require.NotNil(t, holder, "the action is held")
		var stalled atomic.Bool
		timer := time.AfterFunc(15*time.Second, func() {
			stalled.Store(true)
			_ = holder.Rollback(context.Background())
		})
		t.Cleanup(func() { timer.Stop(); _ = holder.Rollback(context.Background()) })
		require.Eventually(t, func() bool { return b.waitingOn("name: LockDestructiveAction :one") }, 20*time.Second, 20*time.Millisecond,
			"the abandon waits for its action")

		// A staff card removal admits meanwhile.
		status, raw := b.a.request(t, http.MethodDelete, "/v1/admin/customers/"+method.CustomerID.String()+"/payment-methods/"+method.ID.String(), b.a.alphaKey.Secret, nil)
		require.False(t, stalled.Load(), "an admission waited for the abandon's row lock")
		require.Equal(t, http.StatusNoContent, status, string(raw))
		timer.Stop()
		require.NoError(t, holder.Rollback(t.Context()))
		require.Error(t, <-errA)
		remove()
		require.Len(t, b.actions(t, "no_effect"), 1, "the abandon finishes once its action is free")
	})

	t.Run("OR-J34-S41/an-archive-exports-right-after-a-catalog-edit", func(t *testing.T) {
		w := newWorld(t)
		usageMeter(t, w.alpha, 100)
		var edits int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "destructive_actions"}.Sanitize()+
			" WHERE merchant_id = $1 AND kind IN ('catalog_change', 'configuration_change') AND state = 'succeeded'", w.alpha.MerchantID().UUID()).Scan(&edits))
		require.Positive(t, edits, "the meter's rate card is an admitted catalog edit")
		archiveBytes(t, w.alpha)
	})

	t.Run("OR-J34-S42/an-archive-export-beside-a-pending-destructive-action-is-refused", func(t *testing.T) {
		b := newNMIBook(t)
		b.land(b.a.alpha, b.owed(b.started.Add(10*24*time.Hour), 1, "pending"))
		require.Len(t, b.actions(t, "started"), 1, "the owed stop waits for its provider effect")
		var out bytes.Buffer
		err := b.a.alpha.ExportBillingArchive(t.Context(), &out)
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, billing.CodeBillingArchiveUnsupportedState, refused.Code)
		require.Equal(t, "destructive_actions", refused.Metadata["table"], "its effect may still land")
	})

	t.Run("OR-J34-S43/a-restore-waits-a-day-after-a-prune-that-can-be-undone", func(t *testing.T) {
		source := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		_, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "pruned-target"}})
		require.NoError(t, err)
		archive := archiveBytes(t, source.alpha)
		target, client := archiveTarget(t, source)
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)

		// A prune of the restored book ends now, by the target's clock: the
		// record its undo reads.
		database, err := db.NewWithPGXPool(slog.Default(), target.pool, target.schema)
		require.NoError(t, err)
		mid := client.MerchantID().UUID()
		require.NoError(t, database.MerchantTx(merchant.WithID(t.Context(), client.MerchantID()), func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			run := uuid.New()
			if _, err := q.CreateDestructiveRun(ctx, gen.CreateDestructiveRunParams{ID: run, MerchantID: mid, Kind: "prune", Actor: "critical operator", Coverage: []byte(`{}`)}); err != nil {
				return err
			}
			_, err := q.FinishDestructiveRun(ctx, gen.FinishDestructiveRunParams{MerchantID: mid, ID: run, Status: "completed", Now: target.clock.Now().UTC(), Affected: []byte(`{}`)})
			return err
		}))

		// The book's restore, replayed, waits while the prune can be undone.
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, billing.CodeBillingArchiveUnsupportedState, refused.Code)
		require.Equal(t, "maintenance_runs", refused.Metadata["table"])
		target.clock.Advance(25 * time.Hour)
		replay, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err, "a day on, by the deployment's clock")
		require.True(t, replay.AlreadyImported)
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
	nmi     *nmiJournal
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
	return &nmiBook{t: t, a: a, nmi: p, price: price.ID, started: started, due: started.Add(7 * 24 * time.Hour)}
}

// owed declares n schedules canceled an hour before asOf whose NMI
// schedules are still live: each owes a provider stop.
func (b *nmiBook) owed(asOf time.Time, n int, prefix string) billing.DeclaredBilling {
	out := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	for i := range n {
		customer := billing.CustomerID(uuid.New())
		ref := fmt.Sprintf("%s-%02d", prefix, i)
		out.Customers = append(out.Customers, billing.DeclaredCustomer{Customer: customer})
		out.Subscriptions = append(out.Subscriptions, billing.DeclaredSubscription{SourceID: ref, Customer: customer, Price: b.price, Rail: "nmi",
			RailSubscriptionID: ref, StartedAt: b.started, Cancel: billing.CancelEvidence{Kind: "user_canceled", At: asOf.Add(-time.Hour), ScheduleLive: true}})
	}
	return out
}

// saveCard saves a card for alice.
func (b *nmiBook) saveCard(t *testing.T) billing.PaymentMethod {
	t.Helper()
	psps, err := b.a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	status, raw := b.a.request(t, http.MethodPost, "/v1/me/payment-methods", b.a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID,
		Token: b.nmi.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var method billing.PaymentMethod
	require.NoError(t, json.Unmarshal(raw, &method))
	return method
}

// waitingOn reports a session of the test database waiting on a lock in a
// statement containing marker. It runs off the test goroutine too.
func (b *nmiBook) waitingOn(marker string) bool {
	var n int
	err := b.a.pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%' AND pid <> pg_backend_pid()", marker).Scan(&n)
	return err == nil && n > 0
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
