//go:build integration

package critical_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/reconcile"
)

// An upgrade dates each PSP an import brought before imports recorded their
// as_of, from what the import stored.
func TestKnownThroughUpgrade(t *testing.T) {
	t.Run("OR-J15-S80/an-upgrade-dates-each-psp-an-earlier-import-brought-from-what-it-stored", func(t *testing.T) {
		ctx := t.Context()
		pool, schema, logger := schemaBefore(t, "0018_")
		table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
		exec := func(sql string, args ...any) {
			t.Helper()
			_, err := pool.Exec(ctx, sql, args...)
			require.NoError(t, err, sql)
		}
		now := time.Now().UTC().Truncate(time.Second)
		day := 24 * time.Hour
		m, product, price := uuid.New(), uuid.New(), uuid.New()
		exec("INSERT INTO "+table("merchants")+" (id, slug, status) VALUES ($1, 'upgrade-known-through', 'active')", m)
		exec("INSERT INTO "+table("products")+" (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'member', 'Member', 0, false)", product, m)
		exec("INSERT INTO "+table("prices")+" (id, merchant_id, product_id, key, amount, currency, archived, billing_interval_hours) VALUES ($1, $2, $3, 'member', 10000000, 'USD', false, 720)",
			price, m, product)
		psps := map[string]uuid.UUID{}
		for _, p := range []struct{ key, rail, account string }{
			{"mobius", "nmi", "mobius-gateway"}, {"paykings", "nmi", "paykings-gateway"}, {"ccbill", "ccbill", "999999-0000"}, {"stripe", "stripe", "acct_upgradeknown"},
		} {
			psps[p.key] = uuid.New()
			exec("INSERT INTO "+table("psps")+" (id, merchant_id, key, rail, environment, account_id) VALUES ($1, $2, $3, $4, 'test', $5)", psps[p.key], m, p.key, p.rail, p.account)
		}
		// A subscription as a book lands it (host) or a provider's evidence
		// does, each its own customer's: its start, its current period and,
		// once canceled, when the cancel was recorded.
		subscribe := func(psp, rail, authority string, started, periodStart, periodEnd time.Time, canceled *time.Time) (uuid.UUID, uuid.UUID) {
			t.Helper()
			customer, sub := uuid.New(), uuid.New()
			policy := "provider"
			if rail == "nmi" {
				policy = "nmi_schedule"
			}
			exec("INSERT INTO "+table("customers")+" (id, merchant_id, created_at, last_seen_at) VALUES ($1, $2, $3, $3)", customer, m, started)
			status := "active"
			var cancelType, feedback *string
			if canceled != nil {
				status, cancelType, feedback = "canceled", ptr("user"), ptr("imported: declared user_canceled")
			}
			exec("INSERT INTO "+table("subscriptions")+` (id, merchant_id, customer_id, product_id, price_id, status, rail, collection_policy, rail_subscription_id, psp_id,
				started_at, current_period_starts_at, current_period_ends_at, canceled_at, ended_at, cancel_type, cancel_feedback, created_at, updated_at, facts_authority)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $17, $8, $9, $10, $11, $12, $13, $13, $14, $15, $10, $10, $16)`,
				sub, m, customer, product, price, status, rail, "sub-"+sub.String()[:8], psps[psp], started, periodStart, periodEnd, canceled, cancelType, feedback, authority, policy)
			return sub, customer
		}
		month := 30 * day

		// mobius: a book landed after imports recorded their as_of.
		mobiusAsOf, mobiusFailed := now.Add(-24*day), now.Add(-24*day+time.Minute)
		subscribe("mobius", "nmi", "host", now.Add(-60*day), now.Add(-30*day), now, nil)
		exec("INSERT INTO "+table("psp_freshness")+" (merchant_id, psp_id, imported_through, failing_since, failed_at, failure, stale_since) VALUES ($1, $2, $3, $4, $4, 'not_configured', $4)",
			m, psps["mobius"], mobiusAsOf, mobiusFailed)
		// paykings: a book landed before, its schedule accepted at its as_of;
		// then a read failed, as the build before recorded it.
		paykingsAsOf, paykingsFailed := now.Add(-30*day), now.Add(-2*day)
		sub, customer := subscribe("paykings", "nmi", "host", now.Add(-65*day), now.Add(-35*day), now.Add(-5*day), nil)
		exec("INSERT INTO "+table("provider_schedules")+` (merchant_id, subscription_id, customer_id, psp_id, accepted_version, source_reference, provider_reference,
			first_due_at, cadence_hours, cadence_months, accepted_bill, created_at) VALUES ($1, $2, $3, $4, 1, 'paykings-schedule', 'paykings-schedule', $5, 720, 0, '{}', $6)`,
			m, sub, customer, psps["paykings"], now.Add(-5*day), paykingsAsOf)
		exec("INSERT INTO "+table("psp_freshness")+" (merchant_id, psp_id, failing_since, failed_at, failure, stale_since) VALUES ($1, $2, $3, $3, 'not_configured', $3)",
			m, psps["paykings"], paykingsFailed)
		// ccbill: a book of a dump landed later with no schedule, and nothing
		// read since. Its newest source fact is a renewal's period start; the
		// import stamped a cancel the source did not date at its own run.
		ccbillLatest, ccbillRun := now.Add(-27*day), now.Add(-3*day)
		subscribe("ccbill", "ccbill", "host", now.Add(-90*day), ccbillLatest, ccbillLatest.Add(month), nil)
		subscribe("ccbill", "ccbill", "host", now.Add(-200*day), now.Add(-100*day), now.Add(-70*day), &ccbillRun)
		// stripe: provider-owned rows, never imported.
		subscribe("stripe", "stripe", "provider", now.Add(-5*day), now.Add(-5*day), now.Add(25*day), nil)

		require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}), "an upgrade over imports that recorded no as_of")

		database, err := db.NewWithPGXPool(slog.New(slog.DiscardHandler), pool, schema)
		require.NoError(t, err)
		list, complete, through, err := reconcile.KnownThroughs(ctx, database.Gen(ctx), m, now)
		require.NoError(t, err)
		known := map[string]billing.PSPKnownThrough{}
		for _, f := range list {
			require.True(t, f.HasData, f.Key)
			require.NotNil(t, f.KnownThrough, "%s holds data: never undated after the upgrade", f.Key)
			known[f.Key] = f
		}
		require.Len(t, known, 4)
		for key, want := range map[string]struct {
			at     time.Time
			source billing.PSPKnownThroughSource
		}{
			"mobius":   {mobiusAsOf, billing.KnownThroughImport},
			"paykings": {paykingsAsOf, billing.KnownThroughImport},
			"ccbill":   {ccbillLatest, billing.KnownThroughImportRecords},
		} {
			f := known[key]
			require.False(t, f.Current, key)
			require.True(t, f.KnownThrough.Equal(want.at), "%s known through %s, not %s: never an instant the import stamped at its run", key, want.at, f.KnownThrough)
			require.Equal(t, want.source, f.Source, key)
		}
		require.Equal(t, billing.PSPImported, known["paykings"].Cause, "imported, not connected: not merely without credentials")
		require.True(t, known["stripe"].Current, "a PSP no import brought is not dated by one")
		require.NotNil(t, complete)
		require.True(t, complete.Equal(paykingsAsOf), "the earliest stale PSP's: %s", complete)
		require.Nil(t, through, "stripe is current")

		// Which rule dated each: the import's own as_of, or its latest record.
		rows, err := pool.Query(ctx, "SELECT psp_id, imported_through, imported_from, stale_since FROM "+table("psp_freshness")+" WHERE merchant_id = $1", m)
		require.NoError(t, err)
		type dated struct {
			through, stale *time.Time
			from           *string
		}
		stored := map[uuid.UUID]dated{}
		for rows.Next() {
			var psp uuid.UUID
			var d dated
			require.NoError(t, rows.Scan(&psp, &d.through, &d.from, &d.stale))
			stored[psp] = d
		}
		require.NoError(t, rows.Err())
		require.Len(t, stored, 3, "no row for the PSP no import brought")
		for key, want := range map[string]struct {
			through time.Time
			from    string
			stale   time.Time
		}{
			"mobius":   {mobiusAsOf, "as_of", mobiusFailed},
			"paykings": {paykingsAsOf, "as_of", paykingsFailed},
			"ccbill":   {ccbillLatest, "latest_record", ccbillLatest},
		} {
			d := stored[psps[key]]
			require.NotNil(t, d.through, key)
			require.True(t, d.through.Equal(want.through), key)
			require.NotNil(t, d.from, key)
			require.Equal(t, want.from, *d.from, key)
			require.NotNil(t, d.stale, key)
			require.True(t, d.stale.Equal(want.stale), "%s stale since %s, not %s", key, want.stale, d.stale)
		}
	})
}
