//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/destructive"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// scheduledNotices are the customer's price-change notices of one type.
func scheduledNotices(t *testing.T, client *openrails.Client, eventType string, customer billing.CustomerID) []billing.NotificationData {
	t.Helper()
	page, err := client.ListEvents(t.Context(), billing.EventListParams{Types: []string{eventType}, Limit: 100})
	require.NoError(t, err)
	var out []billing.NotificationData
	for _, e := range page.Events {
		if e.Subject.ID != customer.String() {
			continue
		}
		var data billing.NotificationData
		require.NoError(t, json.Unmarshal(e.Data, &data))
		out = append(out, data)
	}
	return out
}

// migrationUnits is the destructive units the migration's admission holds.
func migrationUnits(t *testing.T, w *world, id billing.PriceMigrationID) int64 {
	t.Helper()
	var units int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT a.units FROM "+pgx.Identifier{w.schema, "destructive_actions"}.Sanitize()+" a JOIN "+
		pgx.Identifier{w.schema, "price_migrations"}.Sanitize()+" m ON m.merchant_id = a.merchant_id AND m.destructive_action_id = a.id WHERE m.id = $1", id.UUID()).Scan(&units))
	return units
}

// ccbillMembers imports n CCBill members of price, paid five days ahead.
func ccbillMembers(t *testing.T, a *authorityWorld, price billing.PriceID, n int) []billing.CustomerID {
	t.Helper()
	now := a.clock.Now().UTC().Truncate(time.Second)
	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "ccbill"}}
	var customers []billing.CustomerID
	for i := range n {
		customer := billing.CustomerID(uuid.New())
		sub, sale := legacyCCBillMember(customer, price, fmt.Sprintf("55667788%010d", i), fmt.Sprintf("081223344%010d", i), now.Add(5*24*time.Hour))
		book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
		book.Subscriptions = append(book.Subscriptions, sub)
		book.Transactions = append(book.Transactions, sale)
		customers = append(customers, customer)
	}
	imported, err := a.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Len(t, imported.Imported, n, "%v", imported.Reasons)
	return customers
}

func TestPriceMigrationScale(t *testing.T) {
	t.Run("OR-J19-S09/a-migrations-source-archive-is-a-catalog-edit-a-document-does-not-undo", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		document := func(name string) *catalog.Application {
			doc, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"products":{"plans":{"display_name":"` + name + `","entitlements":["plans:member"],` +
				`"prices":{"basic":{"amount":"10 USD","billing_interval_hours":24,"access_duration_hours":24,"archived":false}}}}}`))
			require.NoError(t, err)
			return doc
		}
		_, err := a.alpha.ApplyCatalog(t.Context(), document("Plans"), billing.ApplyCatalogParams{})
		require.NoError(t, err)
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"plans"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		require.Len(t, products.Items[0].Prices, 1)
		basic := products.Items[0].Prices[0]
		sub := membershipAtPrice(t, a, p, basic.ID, a.aliceToken, "documented-membership")
		target, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: products.Items[0].ID, Key: "basic-v2", UnitAmount: 12_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Scheduled)
		source, err := a.alpha.GetPrice(t.Context(), basic.ID)
		require.NoError(t, err)
		require.True(t, source.Archived)
		// The document still declares the source on sale. The archive is an
		// edit that owns the field: the document reports it, and does not sell
		// the source again beneath the migration.
		receipt, err := a.alpha.ApplyCatalog(t.Context(), document("Plans renamed"), billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Len(t, receipt.Conflicts, 1, "%+v", receipt.Conflicts)
		conflict := receipt.Conflicts[0]
		require.Equal(t, billing.CatalogObjectPrice, conflict.Object)
		require.Equal(t, "basic", conflict.Key)
		require.Len(t, conflict.Fields, 1)
		require.Equal(t, "archived", conflict.Fields[0].Field)
		source, err = a.alpha.GetPrice(t.Context(), basic.ID)
		require.NoError(t, err)
		require.True(t, source.Archived, "the migration's archive stands")
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.NotNil(t, current.ScheduledChange)
		require.Equal(t, target.ID, current.ScheduledChange.PriceID)
	})

	t.Run("OR-J19-S10/a-migration-admits-every-subscriber-before-reading-any-and-reaches-them-a-page-at-a-time", func(t *testing.T) {
		const members = 101
		a := ccbillWebhookWorld(t, func(w *world) {
			w.limits = func(l *destructive.Limits) {
				*l = destructive.Limits{ActorPerHour: 150, ActorPerDay: 300, MerchantPerHour: 150, MerchantPerDay: 300, DeploymentPerHour: 300, DeploymentPerDay: 600, ErasurePerHour: l.ErasurePerHour, ErasurePerDay: l.ErasurePerDay}
			}
		})
		source := ccbillMemberPrice(t, a.alpha)
		price, err := a.alpha.GetPrice(t.Context(), source)
		require.NoError(t, err)
		target, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: price.ProductID, Key: "ccbill-monthly-v2", PSP: "ccbill", FlexID: "critical-flex-v2",
			Currency: "USD", UnitAmount: ptr(int64(12_000_000)), BillingIntervalHours: ptr(720)})
		require.NoError(t, err)
		customers := ccbillMembers(t, a, source, members)
		params := billing.CreatePriceMigrationParams{FromPriceID: source, ToPriceID: target.ID, AcknowledgeShortNotice: true,
			FallbackPolicy: billing.MigrationCancelAtPeriodEnd, ArchiveSource: ptr(false)}
		// Staff, whose destructive ceilings count it (the in-process host's own
		// work is never counted).
		staff := a.alphaRemote
		preview, err := staff.PreviewPriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, members, preview.Matched)
		require.Equal(t, members, preview.Classified)
		require.Equal(t, members, preview.Blocked)
		require.Equal(t, members, preview.ByRail["ccbill"].RequiresAction)

		// Creation reaches the first page with the migration itself.
		migration, err := staff.CreatePriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, members, migration.Matched)
		require.True(t, migration.Preparing, "the rest are reached after the call")
		require.Equal(t, 100, migration.Blocked, "one page, in one transaction")
		require.EqualValues(t, members, migrationUnits(t, a.world, migration.ID), "every subscriber counted is admitted before any is read")
		manualFleet(t, a.world)
		var result *billing.PriceMigration
		require.Eventually(t, func() bool {
			result, err = a.alpha.GetPriceMigration(t.Context(), migration.ID)
			return err == nil && !result.Preparing
		}, 30*time.Second, 50*time.Millisecond, "the next page is reached by its job")
		require.Equal(t, members, result.Blocked)
		require.Zero(t, result.Skipped)
		require.Zero(t, result.Scheduled)
		require.Equal(t, billing.MigrationCancelAtPeriodEnd, result.FallbackPolicy)
		require.True(t, migrationSettled(t, a.world, migration.ID))
		// CCBill bills on its own: each block records the move OpenRails cannot
		// make, and cancel_at_period_end records intent; nothing is canceled.
		for _, customer := range customers[:3] {
			subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
			require.NoError(t, err)
			require.Len(t, subs.Items, 1)
			require.Equal(t, source, subs.Items[0].PriceID)
			require.Equal(t, billing.SubscriptionActive, subs.Items[0].Status)
			require.Nil(t, subs.Items[0].CanceledAt)
			require.Nil(t, subs.Items[0].ScheduledChange)
		}

		// Another migration of the same cohort is refused whole past the
		// actor's ceiling: nothing is recorded.
		params.FallbackPolicy = billing.MigrationKeepGrandfathered
		_, err = staff.CreatePriceMigration(t.Context(), params)
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, http.StatusTooManyRequests, refused.Status)
		migrations, err := a.alpha.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{})
		require.NoError(t, err)
		require.Len(t, migrations.Items, 1)
	})

	t.Run("OR-J19-S15", func(t *testing.T) {
		// Staff whose hourly ceiling is or is one short of what the migration
		// spends: two subscribers and the source it archives.
		for _, c := range []struct {
			name    string
			ceiling int64
		}{{"one-short", 2}, {"exactly", 3}} {
			t.Run(c.name, func(t *testing.T) {
				a, p, alice := purchasedMembershipWith(t, 24, func(w *world) {
					w.limits = func(l *destructive.Limits) { l.ActorPerHour = c.ceiling }
				})
				bob := membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "ceiling-bob")
				target := migrationPrice(t, a, alice, "ceiling-target", 30_000_000)
				migration, err := a.alphaRemote.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
				source, readErr := a.alpha.GetPrice(t.Context(), alice.PriceID)
				require.NoError(t, readErr)
				migrations, listErr := a.alpha.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{})
				require.NoError(t, listErr)
				if c.name == "one-short" {
					var refused *billing.StatusError
					require.ErrorAs(t, err, &refused)
					require.Equal(t, http.StatusTooManyRequests, refused.Status)
					require.Empty(t, migrations.Items, "refused whole: nothing is recorded")
					require.False(t, source.Archived, "and nothing is archived")
					for _, sub := range []*billing.Subscription{alice, bob} {
						current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
						require.NoError(t, err)
						require.Nil(t, current.ScheduledChange)
					}
					return
				}
				require.NoError(t, err)
				require.Equal(t, 2, migration.Scheduled)
				require.Empty(t, migration.Warnings)
				require.True(t, source.Archived, "the source stops selling under the same command")
				require.Len(t, migrations.Items, 1)
				require.EqualValues(t, 3, migrationUnits(t, a.world, migration.ID), "one command: two subscribers and the source")
				holds, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
				require.NoError(t, err)
				require.Empty(t, holds.Items)
			})
		}
	})

	t.Run("OR-J19-S16/a-cancel-ends-a-walk-whose-next-page-never-ran", func(t *testing.T) {
		const members = 101
		a := ccbillWebhookWorld(t)
		source := ccbillMemberPrice(t, a.alpha)
		price, err := a.alpha.GetPrice(t.Context(), source)
		require.NoError(t, err)
		target, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: price.ProductID, Key: "ccbill-monthly-v2", PSP: "ccbill", FlexID: "critical-flex-v2",
			Currency: "USD", UnitAmount: ptr(int64(12_000_000)), BillingIntervalHours: ptr(720)})
		require.NoError(t, err)
		ccbillMembers(t, a, source, members)
		migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: source, ToPriceID: target.ID, AcknowledgeShortNotice: true, ArchiveSource: ptr(false)})
		require.NoError(t, err)
		require.True(t, migration.Preparing)
		// No worker runs: the next page's job waits. The cancel ends the walk.
		canceled, err := a.alpha.CancelPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.False(t, canceled.PriceMigration.Preparing)
		require.Equal(t, 100, canceled.PriceMigration.Blocked)
		require.Equal(t, 1, canceled.PriceMigration.Skipped, "the one it never reached")
		require.True(t, migrationSettled(t, a.world, migration.ID))
		// The page's job, run later, changes nothing.
		fleet := manualFleet(t, a.world)
		require.Eventually(t, func() bool {
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.price_migration").States(rivertype.JobStateCompleted))
			return err == nil && len(jobs.Jobs) == 1
		}, 30*time.Second, 50*time.Millisecond)
		result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.False(t, result.Preparing)
		require.Equal(t, 100, result.Blocked)
		require.Equal(t, 1, result.Skipped)
	})

	t.Run("OR-J19-S17/concurrent-identical-creates-answer-one-migration", func(t *testing.T) {
		a, p, alice := purchasedMembership(t)
		membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "concurrent-bob")
		target := migrationPrice(t, a, alice, "concurrent-target", 30_000_000)
		// Another catalog writer holds the catalog lock: both requests are
		// admitted and wait to archive the source, then record at once.
		holder, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = holder.Rollback(context.Background()) }()
		var revision int64
		require.NoError(t, holder.QueryRow(t.Context(), "SELECT catalog_revision FROM "+pgx.Identifier{a.schema, "merchants"}.Sanitize()+" WHERE id = $1 FOR NO KEY UPDATE",
			a.alpha.MerchantID().UUID()).Scan(&revision))
		var holderPID int
		require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPID))
		params := billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true}
		type created struct {
			migration *billing.PriceMigration
			err       error
		}
		done := make(chan created, 2)
		// A second waiter on the row queues behind the first, not the holder.
		blocked := func(n int) {
			t.Helper()
			require.Eventually(t, func() bool {
				var waiting int
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity w WHERE $1 = ANY(pg_blocking_pids(w.pid))"+
					" OR EXISTS (SELECT 1 FROM unnest(pg_blocking_pids(w.pid)) b WHERE $1 = ANY(pg_blocking_pids(b)))", holderPID).Scan(&waiting))
				return waiting == n
			}, 20*time.Second, 20*time.Millisecond, "waiting on the catalog lock")
		}
		// The second arrives once the first, admitted, waits to archive.
		for i := range 2 {
			go func() {
				m, err := a.alpha.CreatePriceMigration(context.Background(), params)
				done <- created{m, err}
			}()
			blocked(i + 1)
		}
		require.NoError(t, holder.Rollback(t.Context()))
		var ids []billing.PriceMigrationID
		for range 2 {
			select {
			case r := <-done:
				require.NoError(t, r.err)
				ids = append(ids, r.migration.ID)
			case <-time.After(30 * time.Second):
				t.Fatal("a create never answered")
			}
		}
		require.Equal(t, ids[0], ids[1], "both answer the one migration")
		migrations, err := a.alpha.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{})
		require.NoError(t, err)
		require.Len(t, migrations.Items, 1)
		require.Equal(t, 2, migrations.Items[0].Scheduled)
	})

	t.Run("OR-J19-S18/a-create-whose-caller-leaves-after-its-archive-is-recorded-under-one-command", func(t *testing.T) {
		a, p, alice := purchasedMembership(t)
		bob := membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "outlived-bob")
		target := migrationPrice(t, a, alice, "outlived-target", 30_000_000)
		holder, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = holder.Rollback(context.Background()) }()
		_, err = holder.Exec(t.Context(), "SELECT 1 FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1 FOR UPDATE", bob.ID.UUID())
		require.NoError(t, err)
		var holderPID int
		require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPID))
		params := billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = a.alpha.CreatePriceMigration(ctx, params)
			close(done)
		}()
		require.Eventually(t, func() bool {
			var waiting int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))", holderPID).Scan(&waiting))
			return waiting > 0
		}, 20*time.Second, 20*time.Millisecond, "the source is archived and recording waits on bob")
		source, err := a.alpha.GetPrice(t.Context(), alice.PriceID)
		require.NoError(t, err)
		require.True(t, source.Archived)
		// The caller leaves after the archive: the migration is still recorded.
		cancel()
		require.NoError(t, holder.Rollback(t.Context()))
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the creation never ended")
		}
		var migrations *billing.ListPage[billing.PriceMigration]
		require.Eventually(t, func() bool {
			migrations, err = a.alpha.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{})
			return err == nil && len(migrations.Items) == 1
		}, 20*time.Second, 50*time.Millisecond, "recorded though its caller left")
		require.Equal(t, 2, migrations.Items[0].Scheduled)
		// The retry answers it under the one command: two subscribers and
		// the source, admitted once.
		again, err := a.alpha.CreatePriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, migrations.Items[0].ID, again.ID)
		var commands int
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "destructive_actions"}.Sanitize()+
			" WHERE merchant_id = $1 AND target = $2", a.alpha.MerchantID().UUID(), "price-migration/"+target.ID.String()).Scan(&commands))
		require.Equal(t, 1, commands)
		require.EqualValues(t, 3, migrationUnits(t, a.world, again.ID))
	})

	t.Run("OR-J19-S19/an-identical-create-while-the-first-records-is-refused-at-once-and-stalls-no-admission", func(t *testing.T) {
		a, p, alice := purchasedMembership(t)
		bob := membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "in-progress-bob")
		target := migrationPrice(t, a, alice, "in-progress-target", 30_000_000)
		other := migrationPrice(t, a, alice, "unrelated-price", 40_000_000)
		holder, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = holder.Rollback(context.Background()) }()
		_, err = holder.Exec(t.Context(), "SELECT 1 FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1 FOR UPDATE", bob.ID.UUID())
		require.NoError(t, err)
		var holderPID int
		require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPID))
		params := billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true, ArchiveSource: ptr(false)}
		first := make(chan error, 1)
		go func() {
			_, err := a.alpha.CreatePriceMigration(context.Background(), params)
			first <- err
		}()
		require.Eventually(t, func() bool {
			var waiting int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))", holderPID).Scan(&waiting))
			return waiting > 0
		}, 20*time.Second, 20*time.Millisecond, "the first records and waits on bob")
		// The identical request is answered at once, not queued behind it.
		quick, stop := context.WithTimeout(t.Context(), 10*time.Second)
		defer stop()
		_, err = a.alpha.CreatePriceMigration(quick, params)
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, http.StatusConflict, refused.Status)
		require.Equal(t, billing.CodePriceMigrationInProgress, refused.Code)
		// Another destructive command of the merchant is admitted meanwhile.
		archived, err := a.alpha.UpdatePrice(quick, other.ID, billing.UpdatePriceParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		require.True(t, archived.Archived)
		require.NoError(t, holder.Rollback(t.Context()))
		select {
		case err := <-first:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("the first creation never answered")
		}
		recorded, err := a.alpha.CreatePriceMigration(t.Context(), params)
		require.NoError(t, err, "once recorded, the identical request answers it")
		require.Equal(t, 2, recorded.Scheduled)
	})

	t.Run("OR-J19-S11/a-solana-subscription-is-blocked-and-nothing-is-sent", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		target, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: "chain-weekly-v2", UnitAmount: 12_000_000, Currency: "USD",
			SellVia: []catalog.SellVia{catalog.SellViaSolana}, BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})
		require.NoError(t, err)
		sent := s.wire.sends()
		migration, err := s.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true, ArchiveSource: ptr(false)})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Blocked)
		require.Zero(t, migration.Scheduled)
		require.False(t, migration.Preparing)
		require.Equal(t, billing.MigrationKeepGrandfathered, migration.FallbackPolicy)
		require.True(t, migrationSettled(t, s.world, migration.ID))
		require.Equal(t, sent, s.wire.sends(), "the customer's own signature moves a Solana plan: nothing is sent")
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PriceID, current.PriceID)
		require.Nil(t, current.ScheduledChange)
	})

	t.Run("OR-J19-S12/by-key-and-cross-product-moves-send-their-notices-and-apply-at-renewal", func(t *testing.T) {
		a, p, alice := purchasedMembership(t)
		// The key's next version: bob subscribes to it.
		second, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: alice.ProductID, Key: "membership-day", UnitAmount: 11_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		require.NotEqual(t, alice.PriceID, second.ID)
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: second.ID}}, ExpectedTotal: ptr(int64(11_000_000)), Reusable: ptr(true),
			Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.bobToken, input, map[string]string{"Idempotency-Key": "bob-second-version"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		bob, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
		require.NoError(t, err)

		// By key: before the version they move to exists, both are counted.
		byKey := billing.CreatePriceMigrationParams{ProductKey: "membership", PriceKey: "membership-day"}
		counted, err := a.alpha.PreviewPriceMigration(t.Context(), byKey)
		require.NoError(t, err)
		require.Equal(t, 2, counted.Matched)
		require.Nil(t, counted.ToPriceID)
		third, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: alice.ProductID, Key: "membership-day", UnitAmount: 12_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		byKey.ToPriceID, byKey.AcknowledgeShortNotice, byKey.EffectiveAt = third.ID, true, *bob.CurrentPeriodEndsAt
		migration, err := a.alpha.CreatePriceMigration(t.Context(), byKey)
		require.NoError(t, err)
		require.Equal(t, 2, migration.Scheduled)
		require.Equal(t, ptr("membership"), migration.ProductKey)
		require.Equal(t, ptr("membership-day"), migration.PriceKey)
		require.Nil(t, migration.FromPriceID)
		for _, versions := range []*billing.Price{second} {
			archived, err := a.alpha.GetPrice(t.Context(), versions.ID)
			require.NoError(t, err)
			require.True(t, archived.Archived, "every source version stops selling")
		}
		notices := scheduledNotices(t, a.alpha, "subscription_reprice_scheduled", customerOf(a.bob))
		require.Len(t, notices, 1, "a same-product move is a price change")
		require.Equal(t, second.ID, notices[0].FromPriceID)
		require.Equal(t, third.ID, notices[0].ToPriceID)
		require.EqualValues(t, 11_000_000, *notices[0].OldAmount)
		require.EqualValues(t, 12_000_000, *notices[0].NewAmount)
		require.Equal(t, migration.ID, notices[0].PriceMigrationID)
		require.Empty(t, scheduledNotices(t, a.alpha, "subscription_plan_change_scheduled", customerOf(a.bob)))

		// Across products: alice's move is a plan change, named so.
		_, err = a.alpha.CancelPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		higher := upgradePrice(t, a, alice)
		cross, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: higher.ID, AcknowledgeShortNotice: true, EffectiveAt: *alice.CurrentPeriodEndsAt})
		require.NoError(t, err)
		require.Equal(t, 1, cross.Scheduled)
		plan := scheduledNotices(t, a.alpha, "subscription_plan_change_scheduled", customerOf(a.alice))
		require.Len(t, plan, 1)
		require.Equal(t, higher.ProductID, plan[0].ToProductID)
		require.Equal(t, "Higher membership", plan[0].ToProductName)
		require.EqualValues(t, 20_000_000, *plan[0].NewAmount)
		fleet := startFleet(t, a.world)
		a.clock.Advance(alice.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), alice.ID)
			return err == nil && current.PriceID == higher.ID && current.ProductID == higher.ProductID
		}, 20*time.Second, 20*time.Millisecond, "the renewal applies the plan change")
		result, err := a.alpha.GetPriceMigration(t.Context(), cross.ID)
		require.NoError(t, err)
		require.Equal(t, 1, result.Applied)
	})

	t.Run("OR-J19-S13/canceling-a-pushed-stripe-move-names-the-schedule-to-release", func(t *testing.T) {
		s := stripeSeatMembership(t)
		s.stripe.AddPrice(stripemock.Price{ID: "price_gold_v3", Product: "prod_gold", Amount: 2500, Interval: "month"})
		target, err := s.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: s.gold.ProductID, Key: "stripe-gold-v3", PSP: "seats", PriceID: "price_gold_v3"})
		require.NoError(t, err)
		migration, err := s.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: s.gold.ID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Scheduled)
		remote := *s.sub.RailSubscriptionID
		fleet := manualFleet(t, s.world)
		var schedule string
		require.Eventually(t, func() bool {
			wakeOperations(t, fleet)
			schedule, _ = s.stripe.Subscription(remote)["schedule"].(string)
			return schedule != "" && len(s.stripe.Mutations("/v1/subscription_schedules/"+schedule)) > 0
		}, 20*time.Second, 50*time.Millisecond, "the move is pushed as a Stripe schedule")
		canceled, err := s.alpha.CancelPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.Equal(t, 1, canceled.Canceled)
		require.Equal(t, []billing.SubscriptionID{s.sub.ID}, canceled.RailReleaseRequired, "Stripe still holds the schedule")
		require.Equal(t, 1, canceled.PriceMigration.Canceled)
		require.NotNil(t, canceled.PriceMigration.CanceledAt)
		attached, _ := s.stripe.Subscription(remote)["schedule"].(string)
		require.Equal(t, schedule, attached, "OpenRails does not release it")
		current, err := s.alpha.GetSubscription(t.Context(), s.sub.ID)
		require.NoError(t, err)
		require.Nil(t, current.ScheduledChange)
		require.True(t, migrationSettled(t, s.world, migration.ID))
	})

	t.Run("OR-J19-S14/an-interrupted-create-records-nothing-and-its-retry-moves-everyone-once", func(t *testing.T) {
		a, p, alice := purchasedMembership(t)
		bob := membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "interrupted-bob")
		target := migrationPrice(t, a, alice, "interrupted-target", 30_000_000)
		// Another writer holds bob's row: creation waits on it, past alice.
		holder, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = holder.Rollback(context.Background()) }()
		_, err = holder.Exec(t.Context(), "SELECT 1 FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id = $1 FOR UPDATE", bob.ID.UUID())
		require.NoError(t, err)
		var holderPID int
		require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPID))
		// Nothing archived: once a source is, the creation outlives its caller
		// (OR-J19-S18).
		params := billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true, ArchiveSource: ptr(false)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := a.alpha.CreatePriceMigration(ctx, params)
			done <- err
		}()
		require.Eventually(t, func() bool {
			var waiting int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))", holderPID).Scan(&waiting))
			return waiting > 0
		}, 20*time.Second, 20*time.Millisecond, "creation reached bob")
		cancel()
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("the interrupted creation never returned")
		}
		require.NoError(t, holder.Rollback(t.Context()))
		migrations, err := a.alpha.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{})
		require.NoError(t, err)
		require.Empty(t, migrations.Items, "an interrupted creation records nothing")
		for _, sub := range []*billing.Subscription{alice, bob} {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			require.Nil(t, current.ScheduledChange, "not even the move made before the interruption")
		}
		migration, err := a.alpha.CreatePriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, 2, migration.Scheduled)
		require.False(t, migration.Preparing)
		replay, err := a.alpha.CreatePriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, migration.ID, replay.ID, "the same request answers the migration it recorded")
		migrations, err = a.alpha.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{})
		require.NoError(t, err)
		require.Len(t, migrations.Items, 1)
		require.Len(t, p.effects(t), 2, "a migration charges nothing")
	})
}
