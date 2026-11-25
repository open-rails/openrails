//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/notify"
	notifypg "github.com/open-rails/authkit/notify/postgres"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func accountInbox(t *testing.T, destination **notifypg.Store) func(*world) {
	t.Helper()
	return func(w *world) {
		schema := w.schema + "_inbox"
		_, err := w.pool.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		})
		require.NoError(t, notifypg.Migrate(t.Context(), w.pool, schema))
		*destination, err = notifypg.New(w.pool, notifypg.Config{Schema: schema, Scope: "accounts"})
		require.NoError(t, err)
		w.deps.Notifications = &openrails.NotificationChannel{Sink: accountTestPublisher{store: *destination}, Recipient: func(_ context.Context, source openrails.NotificationRecipient) (string, bool, error) {
			return source.CustomerID.UUID().String(), source.Issuer == "" || source.Issuer == authtest.Issuer, nil
		}}
	}
}

// accountTestPublisher isolates billing producer behavior against canonical AuthKit
// persistence. Host/client authorization is covered by the real host inbox journeys.
// The test binds provenance from the trusted merchant transaction, never payloads.
type accountTestPublisher struct{ store *notifypg.Store }

func (p accountTestPublisher) Publish(ctx context.Context, values []notify.Publication, transaction ...pgx.Tx) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	changes := make([]notify.Mutation, len(values))
	for i, value := range values {
		changes[i] = notify.Mutation{Input: notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: value.RecipientID}, Origin: notify.Origin{System: "openrails", Scope: mid.String(), ID: value.ID}, Visibility: notify.VisibilityTenant, Kind: value.Kind, Data: value.Data, CreatedAt: value.CreatedAt, ActionRequested: value.ActionRequested, Severity: value.Severity}, Revision: value.Revision}
	}
	return p.apply(ctx, changes, transaction...)
}

func (p accountTestPublisher) Withdraw(ctx context.Context, values []notify.Withdrawal, transaction ...pgx.Tx) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	changes := make([]notify.Mutation, len(values))
	for i, value := range values {
		changes[i] = notify.Mutation{Input: notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: value.RecipientID}, Origin: notify.Origin{System: "openrails", Scope: mid.String(), ID: value.ID}, Visibility: notify.VisibilityTenant}, Revision: value.Revision, Withdrawn: true}
	}
	return p.apply(ctx, changes, transaction...)
}

func (p accountTestPublisher) apply(ctx context.Context, changes []notify.Mutation, transaction ...pgx.Tx) error {
	if len(transaction) == 0 {
		return p.store.Apply(ctx, changes)
	}
	if len(transaction) != 1 || transaction[0] == nil {
		return notify.ErrInvalid
	}
	return p.store.ApplyTx(ctx, transaction[0], changes)
}

func accountNotices(t *testing.T, store *notifypg.Store, user string) []notify.Item {
	t.Helper()
	page, err := store.List(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: user}, notify.ListOptions{})
	require.NoError(t, err)
	return page.Items
}

func TestAccountNotificationChannel(t *testing.T) {
	t.Run("declined-renewal-publishes-once-and-paid-recovery-withdraws-unread", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		require.Empty(t, accountNotices(t, inbox, a.alice.ID), "routine enrollment is not an inbox message")
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		var item notify.Item
		require.Eventually(t, func() bool {
			items := accountNotices(t, inbox, a.alice.ID)
			if len(items) == 1 {
				item = items[0]
				return true
			}
			return false
		}, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, "subscription:"+sub.ID.UUID().String()+":payment_method_failed", item.Origin.ID)
		require.Equal(t, a.alpha.MerchantID().String(), item.Origin.Scope)
		require.Equal(t, "payment_method_failed", item.Kind)
		require.Nil(t, item.ReadAt)
		require.Empty(t, accountNotices(t, inbox, a.bob.ID))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		items := accountNotices(t, inbox, a.alice.ID)
		require.Len(t, items, 1)
		require.Equal(t, item.ID, items[0].ID, "retry before the due time must not duplicate the receipt")
		adoption, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Len(t, adoption.Adoptions, 1)
		require.Equal(t, item.Origin, adoption.Adoptions[0].Current.Origin)
		require.False(t, adoption.Adoptions[0].Current.Withdrawn)
		assertAdoptionPublisherReplay(t, a, adoption)
		require.Len(t, adoption.Adoptions[0].Previous, 1)
		repeated, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Equal(t, adoption, repeated, "stopped source export must not manufacture new revisions")
		p.core.SetDecline("4242", "")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 0 }, 20*time.Second, 20*time.Millisecond)
		_, err = inbox.Get(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, item.ID)
		require.ErrorIs(t, err, notify.ErrNotFound)
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment_method_failed"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, facts.Events, 1, "withdrawing the inbox item must preserve the general billing fact")
		require.Equal(t, facts.Events[0].ID, adoption.Adoptions[0].Previous[0].ID)
		resolved, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Len(t, resolved.Adoptions, 1)
		require.True(t, resolved.Adoptions[0].Current.Withdrawn)
		require.Greater(t, resolved.Adoptions[0].Current.Revision, adoption.Adoptions[0].Current.Revision)
		require.NoError(t, a.local.RestoreNotificationOrdering(t.Context(), resolved))
		require.NoError(t, a.local.RestoreNotificationOrdering(t.Context(), resolved), "identical source snapshot replay is safe")
		require.Error(t, a.local.RestoreNotificationOrdering(t.Context(), adoption), "old ordering cannot replace a newer resolution")
		afterRestore, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Equal(t, resolved, afterRestore)
	})

	t.Run("replacement-method-withdraws-the-method-request-before-payment", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, "payment_method_update_required", accountNotices(t, inbox, a.alice.ID)[0].Kind)
		method := saveMethod(t, a, p, a.aliceToken, "5556")
		status, raw := a.request(t, http.MethodPut, "/v1/me/subscriptions/"+sub.ID.String()+"/payment-method", a.aliceToken, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &method.ID})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Empty(t, accountNotices(t, inbox, a.alice.ID))
	})

	for _, tc := range []struct {
		name, decline, state, kind string
		expire                     bool
	}{
		{"failed-renewal", "insufficient_funds", "past_due", "payment_method_failed", false},
		{"invalid-method", "lost_card", "awaiting_method", "payment_method_update_required", false},
		{"expired-method-deadline", "lost_card", "awaiting_method", "payment_method_update_required", true},
	} {
		t.Run("cancellation-withdraws-obsolete-payment-request/"+tc.name, func(t *testing.T) {
			var inbox *notifypg.Store
			a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
			p.core.SetDecline("4242", tc.decline)
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.billing_pass", 2)
			waiting := subscriptionState(t, a, sub.ID, tc.state)
			items := accountNotices(t, inbox, a.alice.ID)
			require.Len(t, items, 1)
			require.Equal(t, tc.kind, items[0].Kind)
			require.Nil(t, items[0].ReadAt)
			if tc.expire {
				enableLifecycleMaintenance(t, a)
				require.NotNil(t, waiting.Dunning)
				require.NotNil(t, waiting.Dunning.FinalRetryAt)
				a.clock.Advance(waiting.Dunning.FinalRetryAt.Sub(a.clock.Now()) + time.Microsecond)
				runRegistered(t, fleet, "openrails.billing_pass", 2)
			} else {
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop renewal after the failed charge"})
				require.Equal(t, http.StatusOK, status, string(raw))
			}
			subscriptionState(t, a, sub.ID, "canceled")
			_, err := inbox.Get(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, items[0].ID)
			require.ErrorIs(t, err, notify.ErrNotFound, "an ended renewal must not keep asking the customer to repair its payment")
			facts, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{tc.kind}, Limit: 100})
			require.NoError(t, err)
			require.Len(t, facts.Events, 1)
			adoption, err := a.local.ExportNotificationAdoption(t.Context())
			require.NoError(t, err)
			found := false
			for _, adopted := range adoption.Adoptions {
				if adopted.Current.Kind == tc.kind {
					found = true
					require.True(t, adopted.Current.Withdrawn, "migration must not revive a canceled request for an unrepaired method")
				}
			}
			require.True(t, found, "source export must account for the retained payment fact")
		})
	}

	t.Run("held-collection-does-not-resolve-an-existing-payment-request", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		// The operator has not armed terminal lifecycle actions. The next
		// nonrecoverable decline is held for verification, not paid or canceled.
		p.core.SetDecline("4242", "stolen_card")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool {
			findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.terminal_outcome.held"})
			return err == nil && len(findings.Items) == 1
		}, 20*time.Second, 20*time.Millisecond, "the new provider outcome must commit before adoption")
		subscriptionState(t, a, sub.ID, "past_due")
		adoption, err := a.local.ExportNotificationAdoption(t.Context())
		require.NoError(t, err)
		require.Len(t, adoption.Adoptions, 1)
		require.False(t, adoption.Adoptions[0].Current.Withdrawn, "uncertain provider state is not recovery evidence")
		require.Len(t, accountNotices(t, inbox, a.alice.ID), 1)
	})
}

// recipientGate parks the next host recipient lookup until opened.
type recipientGate struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *recipientGate) wrap(w *world) {
	next := w.deps.Notifications.Recipient
	w.deps.Notifications.Recipient = func(ctx context.Context, source openrails.NotificationRecipient) (string, bool, error) {
		if g.armed.CompareAndSwap(true, false) {
			close(g.entered)
			<-g.release
		}
		return next(ctx, source)
	}
}

func (g *recipientGate) open() { g.once.Do(func() { close(g.release) }) }

func TestNotificationAdoptionExport(t *testing.T) {
	t.Run("OR-J31-S40/export-holds-back-no-other-merchant-and-invoices-its-own-producers", func(t *testing.T) {
		var inbox *notifypg.Store
		lookup := &recipientGate{entered: make(chan struct{}), release: make(chan struct{})}
		defer lookup.open()
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox), lookup.wrap)
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		subscriptionState(t, a, sub.ID, "past_due")
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
		request := accountNotices(t, inbox, a.alice.ID)[0]
		require.Equal(t, "payment_method_failed", request.Kind)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		sale := func(client *openrails.Client) billing.PriceID {
			product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "sale", DisplayName: "Sale", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"sale:owned"}})
			require.NoError(t, err)
			price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "sale", UnitAmount: 30_000_000, Currency: "USD"})
			require.NoError(t, err)
			return price.ID
		}
		alphaSale, betaSale := sale(a.alpha), sale(a.beta)
		paid := func(client *openrails.Client, customer string, price billing.PriceID, key string) func() error {
			return func() error {
				invoice, err := client.CreateInvoice(ctx, key, billing.CreateInvoiceParams{CustomerID: billing.CustomerID(uuid.MustParse(customer)), Lines: []billing.InvoiceLineParams{{PriceID: price}}})
				if err != nil {
					return err
				}
				_, err = client.RecordExternalPayment(ctx, billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 30_000_000, TransactionID: key})
				return err
			}
		}
		// committed reports whether a write lands while the export is held; a
		// write still waiting at the deadline lands after release.
		committed := func(write func() error, release func()) bool {
			done := make(chan error, 1)
			go func() { done <- write() }()
			select {
			case err := <-done:
				require.NoError(t, err)
				return true
			case <-time.After(30 * time.Second):
				release()
				require.NoError(t, <-done)
				return false
			}
		}
		type exportResult struct {
			adoption billing.NotificationAdoption
			err      error
		}
		exported := make(chan exportResult, 1)
		export := func() {
			adoption, err := a.local.ExportNotificationAdoption(ctx)
			exported <- exportResult{adoption, err}
		}

		// Host recipient lookups hold nothing: the merchant's own invoice lands.
		lookup.armed.Store(true)
		go export()
		select {
		case <-lookup.entered:
		case <-time.After(30 * time.Second):
			t.Fatal("the export never looked up its recipient")
		}
		if !committed(paid(a.alpha, a.alice.ID, alphaSale, "alpha-during-lookups"), lookup.open) {
			t.Error("the merchant's own invoice waited behind the export's host recipient lookups")
		}
		lookup.open()
		first := <-exported
		require.NoError(t, first.err)
		require.Len(t, first.adoption.Adoptions, 1)
		require.False(t, first.adoption.Adoptions[0].Current.Withdrawn)
		revision := first.adoption.Adoptions[0].Current.Revision

		// Hold the export inside its snapshot, at its revision write.
		hold, err := a.pool.Begin(ctx)
		require.NoError(t, err)
		release := func() { _ = hold.Rollback(context.Background()) }
		defer release()
		var holder, exporter int
		require.NoError(t, hold.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holder))
		_, err = hold.Exec(ctx, "SELECT 1 FROM "+pgx.Identifier{a.schema, "notification_issue_revisions"}.Sanitize()+" WHERE merchant_id=$1 FOR UPDATE", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		go export()
		require.Eventually(t, func() bool {
			return a.pool.QueryRow(ctx, "SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", holder).Scan(&exporter) == nil
		}, 30*time.Second, 20*time.Millisecond, "the export must reach its revision write")
		if !committed(paid(a.beta, a.bob.ID, betaSale, "beta-during-export"), release) {
			t.Error("another merchant's invoice waited behind this merchant's export")
			require.NoError(t, (<-exported).err)
			return
		}

		// The merchant's own producer waits and lands after the snapshot.
		canceled := make(chan error, 1)
		go func() {
			status, raw, err := a.do(ctx, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop renewal after the failed charge"})
			if err == nil && status != http.StatusOK {
				err = fmt.Errorf("cancel: %d %s", status, raw)
			}
			canceled <- err
		}()
		require.Eventually(t, func() bool {
			var waiting bool
			return a.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", exporter).Scan(&waiting) == nil && waiting
		}, 30*time.Second, 20*time.Millisecond, "the merchant's own cancellation must wait for its export")
		select {
		case err := <-canceled:
			t.Fatalf("the merchant's own cancellation committed inside its export: %v", err)
		default:
		}
		require.NoError(t, hold.Rollback(ctx))
		held := <-exported
		require.NoError(t, held.err)
		require.Len(t, held.adoption.Adoptions, 1)
		require.False(t, held.adoption.Adoptions[0].Current.Withdrawn, "the snapshot predates the waiting cancellation")
		require.Equal(t, revision, held.adoption.Adoptions[0].Current.Revision)
		require.NoError(t, <-canceled)
		subscriptionState(t, a, sub.ID, "canceled")
		// The cancellation ends access, so the inbox now holds the ended notice instead.
		_, err = inbox.Get(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, request.ID)
		require.ErrorIs(t, err, notify.ErrNotFound, "the cancellation withdraws the request after the export")
		after, err := a.local.ExportNotificationAdoption(ctx)
		require.NoError(t, err)
		var withdrawn *notify.Adoption
		for i := range after.Adoptions {
			if after.Adoptions[i].Current.Kind == "payment_method_failed" {
				require.Nil(t, withdrawn, "one adoption per issue")
				withdrawn = &after.Adoptions[i]
			}
		}
		require.NotNil(t, withdrawn)
		require.True(t, withdrawn.Current.Withdrawn)
		require.Greater(t, withdrawn.Current.Revision, revision, "the later withdrawal invoices after the exported revision")
	})
}

func TestInvoiceNotificationRecovery(t *testing.T) {
	for _, disposition := range []string{"partial-then-paid", "waived", "uncollectible", "funded-credit"} {
		t.Run(disposition, func(t *testing.T) {
			var inbox *notifypg.Store
			a, p, customer := usageBillingWorld(t, false, accountInbox(t, &inbox))
			p.core.SetDecline("4242", "202")
			oneUsage(t, a.alpha, usageInput(customer, "declined-usage", 3))
			// The day's usage posts as its threshold invoice is billed: the
			// invoice covers one instant and still duns on the shortest cycle.
			a.clock.Advance(time.Hour)
			fleet := startFleet(t, a.world)
			runUsageDue(t, fleet)
			require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
			item := accountNotices(t, inbox, a.alice.ID)[0]
			invoice := oneUsageInvoice(t, a, customer)
			require.Equal(t, "order:"+invoice.ID.UUID().String()+":"+item.Kind, item.Origin.ID)
			require.Nil(t, item.ReadAt)
			var err error
			switch disposition {
			case "partial-then-paid":
				_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 10_000_000, TransactionID: "bank-part-one"})
				require.NoError(t, err)
				require.Len(t, accountNotices(t, inbox, a.alice.ID), 1, "partial payment leaves an unresolved amount")
				_, err = a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{InvoiceID: invoice.ID, Amount: 20_000_000, TransactionID: "bank-part-two"})
			case "waived":
				_, err = a.alpha.WaiveInvoiceBalance(t.Context(), invoice.ID)
			case "uncollectible":
				_, err = a.alpha.MarkInvoiceUncollectible(t.Context(), invoice.ID)
			case "funded-credit":
				_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 30_000_000, Source: "notification-test", SourceID: "repay-usage"}})
			}
			require.NoError(t, err)
			after := oneUsageInvoice(t, a, customer)
			if disposition == "uncollectible" {
				require.Equal(t, billing.InvoiceUncollectible, after.Status)
				require.EqualValues(t, 30_000_000, after.Usage.AmountDue)
				require.Len(t, accountNotices(t, inbox, a.alice.ID), 1, "stopped collection does not erase debt")
			} else {
				expected := billing.InvoiceComplete
				if disposition == "waived" {
					expected = billing.InvoiceVoid
				}
				require.Equal(t, expected, after.Status)
				require.Zero(t, after.Usage.AmountDue)
				_, err = inbox.Get(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: a.alice.ID}, item.ID)
				require.ErrorIs(t, err, notify.ErrNotFound, "settled or waived debt must retire the request")
			}
			facts, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{item.Kind}, Limit: 100})
			require.NoError(t, err)
			require.Len(t, facts.Events, 1, "resolution preserves the declined collection fact")
			p.money(t, 0, 0)
		})
	}
	t.Run("renewal-authentication-request-is-withdrawn-after-confirmation", func(t *testing.T) {
		var inbox *notifypg.Store
		a, p, sub := purchasedMembershipWith(t, 7*24, accountInbox(t, &inbox))
		p.core.SetDecline("4242", "auth")
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool { return len(accountNotices(t, inbox, a.alice.ID)) == 1 }, 20*time.Second, 20*time.Millisecond)
		item := accountNotices(t, inbox, a.alice.ID)[0]
		require.Equal(t, "payment_authentication_required", item.Kind)
		var data billing.NotificationData
		require.NoError(t, json.Unmarshal(item.Data, &data))
		status, raw := a.request(t, http.MethodGet, "/v1/me/invoices/"+data.InvoiceID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var waiting billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.NotNil(t, waiting.Payment.NextAction)
		intent, _, ok := strings.Cut(waiting.Payment.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok)
		require.True(t, p.core.Authenticate(intent))
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices/"+waiting.ID.String()+"/confirm", a.aliceToken, map[string]any{})
		require.Equal(t, http.StatusOK, status, string(raw))
		var paid billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.InvoiceComplete, paid.Status)
		require.Empty(t, accountNotices(t, inbox, a.alice.ID))
		require.Len(t, p.effects(t), 2, "only enrollment and confirmed renewal charge money")
	})
}

// A legacy receipt may carry source-subject and workflow fields that are not
// producer inputs in the scoped API. Explicit adoption must canonicalize them
// before an unchanged source revision is retried through the real publisher.
func assertAdoptionPublisherReplay(t *testing.T, a *authorityWorld, snapshot billing.NotificationAdoption) {
	t.Helper()
	schema := a.schema + "_adoption"
	t.Cleanup(func() {
		_, err := a.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	accounts := authtest.Replica(t, a.auth, authtest.WithConfig(func(c *authkit.Config) {
		c.Notifications = &authkit.NotificationsConfig{Schema: schema, Scopes: map[string]string{a.tenant.String(): "accounts"}, Producers: []authkit.NotificationProducer{{Name: "openrails", TenantID: a.tenant, Origin: notify.OriginScope{System: "openrails", Scope: a.alpha.MerchantID().String()}, Visibility: notify.VisibilityTenant}}}
	}))
	raw, err := notifypg.New(a.pool, notifypg.Config{Schema: schema, Scope: "accounts"})
	require.NoError(t, err)
	adoption := snapshot.Adoptions[0]
	legacy := adoption.Current.Input
	legacy.Origin = adoption.Previous[0]
	legacy.Origin.Subject = a.alice.ID
	legacy.Status = notify.StatusOpen
	prior, err := raw.Publish(t.Context(), legacy)
	require.NoError(t, err)
	_, err = raw.SetRead(t.Context(), legacy.Recipient, prior.ID, true)
	require.NoError(t, err)
	require.NoError(t, accounts.AdoptNotifications(t.Context(), "openrails", []notify.Adoption{adoption}))
	before, err := accounts.GetNotification(t.Context(), a.alice.ID, prior.ID)
	require.NoError(t, err)
	require.NotNil(t, before.ReadAt)
	publisher, err := accounts.Notifications("openrails")
	require.NoError(t, err)
	m := adoption.Current
	require.NoError(t, publisher.Publish(t.Context(), []notify.Publication{{RecipientID: m.Recipient.ID, ID: m.Origin.ID, Revision: m.Revision, Kind: m.Kind, Data: m.Data, CreatedAt: m.CreatedAt, ActionRequested: m.ActionRequested, Severity: m.Severity}}))
	after, err := accounts.GetNotification(t.Context(), a.alice.ID, prior.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "same-revision scoped publisher replay preserves adopted receipt ID/read state and hash")
}
