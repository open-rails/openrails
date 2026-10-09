//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// noticeKinds is the event type of each of the customer's notifications.
func (c *customer) noticeKinds() []string {
	c.w.t.Helper()
	items, _ := c.must(http.MethodGet, "/notifications?limit=100", "", nil)["data"].([]any)
	var out []string
	for _, item := range items {
		out = append(out, item.(map[string]any)["event_type"].(string))
	}
	return out
}

// A migration with no effective date moves an engine subscription at its next
// renewal: until then the move is the subscription's scheduled change, nothing
// is charged early, and the renewal bills the new price.
func TestPriceMigrationAtRenewal(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]
	e := enroll(t, w, "stripe", embedded)
	old, err := client.GetPrice(ctx, pid(e.price), billing.GetPriceParams{})
	require.NoError(t, err)
	hours := monthHours
	next, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: old.ProductID, Key: "cheaper-" + uuid.NewString()[:8], UnitAmount: 7_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(t, err)
	move := billing.CreatePriceMigrationParams{FromPriceID: old.ID, ToPriceID: next.ID}

	preview, err := client.PreviewPriceMigration(ctx, move)
	require.NoError(t, err)
	require.Equal(t, [4]int{1, 1, 0, 0}, [4]int{preview.Matched, preview.Scheduled, preview.Skipped, preview.Blocked})
	require.Equal(t, billing.PriceMigrationRailCounts{Auto: 1}, *preview.ByRail["stripe"])
	require.Equal(t, []billing.PriceMigrationOutcome{{SubscriptionID: e.sub, Rail: "stripe", Disposition: billing.MigrationScheduled}}, preview.Outcomes)
	require.Nil(t, w.subscription(embedded, e.sub).ScheduledChange, "a preview writes nothing")

	charges := len(e.providerLedger())
	migration, err := client.CreatePriceMigration(ctx, move)
	require.NoError(t, err)
	require.Equal(t, old.ID, *migration.FromPriceID)
	require.Nil(t, migration.PriceKey)
	require.Equal(t, billing.MigrationKeepGrandfathered, migration.FallbackPolicy)
	require.Equal(t, [6]int{1, 0, 1, 0, 0, 0}, [6]int{migration.Matched, migration.Skipped, migration.Scheduled, migration.Applied, migration.Canceled, migration.Blocked})
	require.Len(t, e.providerLedger(), charges, "nothing is charged early")
	archived, err := client.GetPrice(ctx, old.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, archived.Archived, "the source stops selling")

	sub := w.subscription(embedded, e.sub)
	require.Equal(t, old.ID, sub.PriceID, "the paid period keeps its price")
	require.NotNil(t, sub.ScheduledChange)
	require.Equal(t, next.ID, sub.ScheduledChange.PriceID)
	require.Equal(t, billing.ScheduledChangeMigration, sub.ScheduledChange.Source)
	require.Equal(t, migration.ID, *sub.ScheduledChange.PriceMigrationID)
	require.Nil(t, sub.ScheduledChange.Quantity)
	require.Contains(t, e.c.noticeKinds(), "subscription_reprice_scheduled")
	listed, err := client.ListSubscriptions(ctx, billing.SubscriptionListParams{IDs: []billing.SubscriptionID{e.sub}})
	require.NoError(t, err)
	require.Equal(t, sub.ScheduledChange, listed.Items[0].ScheduledChange, "lists carry the scheduled change")

	// One pending change per subscription: another migration skips it.
	again, err := client.PreviewPriceMigration(ctx, move)
	require.NoError(t, err)
	require.Equal(t, billing.MigrationSkipped, again.Outcomes[0].Disposition)
	require.Contains(t, *again.Outcomes[0].Reason, "scheduled change")

	e.refreshBeforePeriodEnd()
	e.toPeriodEnd()
	w.runRenewals()
	ledger := e.providerLedger()
	require.Len(t, ledger, charges+1)
	require.Equal(t, int64(799), ledger[len(ledger)-1].Amount, "the renewal bills the new price")
	sub = w.subscription(embedded, e.sub)
	require.Equal(t, next.ID, sub.PriceID)
	require.Nil(t, sub.ScheduledChange, "an applied change is no longer pending")
	got, err := client.GetPriceMigration(ctx, migration.ID)
	require.NoError(t, err)
	require.Equal(t, [2]int{0, 1}, [2]int{got.Scheduled, got.Applied})
	require.True(t, e.c.entitled(e.ent))
}

// A migration on a date moves each subscription at its first renewal on or
// after it: an earlier renewal still bills the old price. Moving to another
// product is a plan change: its own notice, and the renewal moves access.
func TestPriceMigrationOnADate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]
	e := enroll(t, w, "stripe", embedded)
	old := pid(e.price)
	hours := monthHours
	group := benefitGroup(e.ent)
	plan, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "plan-b-" + uuid.NewString()[:8], DisplayName: "Plan B", TierGroup: &group, Entitlements: []string{"content:plan-b"}})
	require.NoError(t, err)
	target, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: plan.ID, Key: plan.Key + "-usd", UnitAmount: 12_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(t, err)

	// A price increase needs notice: at the next renewal it is skipped unless acknowledged.
	now, err := client.PreviewPriceMigration(ctx, billing.CreatePriceMigrationParams{FromPriceID: old, ToPriceID: target.ID})
	require.NoError(t, err)
	require.Equal(t, billing.MigrationSkipped, now.Outcomes[0].Disposition)
	require.Contains(t, *now.Outcomes[0].Reason, "notice window")
	acknowledged, err := client.PreviewPriceMigration(ctx, billing.CreatePriceMigrationParams{FromPriceID: old, ToPriceID: target.ID, AcknowledgeShortNotice: true})
	require.NoError(t, err)
	require.Equal(t, billing.MigrationScheduled, acknowledged.Outcomes[0].Disposition)

	first := e.periodEnd()
	effective := first.Add(15 * day).UTC().Truncate(time.Second)
	migration, err := client.CreatePriceMigration(ctx, billing.CreatePriceMigrationParams{FromPriceID: old, ToPriceID: target.ID, EffectiveAt: effective, ArchiveSource: new(false)})
	require.NoError(t, err)
	require.Equal(t, effective, migration.EffectiveAt)
	require.Equal(t, 1, migration.Scheduled)
	require.Contains(t, e.c.noticeKinds(), "subscription_plan_change_scheduled")
	price, err := client.GetPrice(ctx, old, billing.GetPriceParams{})
	require.NoError(t, err)
	require.False(t, price.Archived, "archive_source false keeps the source selling")
	require.Equal(t, effective, w.subscription(embedded, e.sub).ScheduledChange.EffectiveAt)

	e.refreshBeforePeriodEnd()
	e.toPeriodEnd()
	w.runRenewals()
	ledger := e.providerLedger()
	require.Equal(t, int64(999), ledger[len(ledger)-1].Amount, "a renewal before the date bills the old price")
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, old, sub.PriceID)
	require.NotNil(t, sub.ScheduledChange, "the move still waits for its date")

	e.refreshBeforePeriodEnd()
	e.toPeriodEnd()
	w.runRenewals()
	ledger = e.providerLedger()
	require.Equal(t, int64(1299), ledger[len(ledger)-1].Amount, "the first renewal after the date bills the new price")
	sub = w.subscription(embedded, e.sub)
	require.Equal(t, target.ID, sub.PriceID)
	require.Equal(t, plan.ID, sub.ProductID, "a plan change moves the product")
	require.Nil(t, sub.ScheduledChange)
	require.True(t, e.c.entitled("content:plan-b"))
	require.False(t, e.c.entitled(e.ent))
}

// A migration by key moves the subscribers of every version of a price but
// the target. Its preview counts them before the target version exists; a
// cancel drops what is still scheduled.
func TestPriceMigrationOfEveryVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]
	v1 := w.membership("content:versions", 10_000_000)
	product, err := client.GetProduct(ctx, v1.ProductID)
	require.NoError(t, err)
	hours := monthHours
	version := func(amount int64) *billing.Price {
		p, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: v1.ProductID, Key: v1.Key, UnitAmount: amount, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		return p
	}
	a := w.newCustomer()
	subA := a.subscribe(embedded, "stripe", v1.ID.String(), "content:versions", a.saveCard("stripe", visa))
	v2 := version(11_000_000)
	b := w.newCustomer()
	subB := b.subscribe(embedded, "stripe", v2.ID.String(), "content:versions", b.saveCard("stripe", visa))

	byKey := billing.CreatePriceMigrationParams{ProductKey: product.Key, PriceKey: v1.Key}
	before, err := client.PreviewPriceMigration(ctx, byKey)
	require.NoError(t, err)
	require.Equal(t, 2, before.Matched, "before the target exists every version's subscribers count")
	require.Nil(t, before.ToPriceID)
	_, err = client.CreatePriceMigration(ctx, byKey)
	require.Error(t, err, "creating needs the target")

	v3 := version(12_000_000)
	byKey.ToPriceID = v3.ID
	byKey.EffectiveAt = w.clock.Now().Add(45 * day).UTC().Truncate(time.Second)
	preview, err := client.PreviewPriceMigration(ctx, byKey)
	require.NoError(t, err)
	require.Equal(t, [4]int{2, 2, 0, 0}, [4]int{preview.Matched, preview.Scheduled, preview.Skipped, preview.Blocked})
	migration, err := client.CreatePriceMigration(ctx, byKey)
	require.NoError(t, err)
	require.Nil(t, migration.FromPriceID)
	require.Equal(t, product.Key, *migration.ProductKey)
	require.Equal(t, v1.Key, *migration.PriceKey)
	require.Equal(t, v3.ID, migration.ToPriceID)
	require.Equal(t, [2]int{2, 2}, [2]int{migration.Matched, migration.Scheduled})
	for _, id := range []billing.SubscriptionID{subA, subB} {
		change := w.subscription(embedded, id).ScheduledChange
		require.NotNil(t, change)
		require.Equal(t, v3.ID, change.PriceID)
	}

	listed, err := client.ListPriceMigrations(ctx, billing.PriceMigrationListParams{ProductKey: product.Key, PriceKey: v1.Key})
	require.NoError(t, err)
	require.Len(t, listed.Items, 1)
	require.Equal(t, migration.ID, listed.Items[0].ID)
	other, err := client.ListPriceMigrations(ctx, billing.PriceMigrationListParams{ProductKey: product.Key, PriceKey: "elsewhere"})
	require.NoError(t, err)
	require.Empty(t, other.Items)
	byID, err := client.ListPriceMigrations(ctx, billing.PriceMigrationListParams{IDs: []billing.PriceMigrationID{migration.ID}})
	require.NoError(t, err)
	require.Equal(t, []billing.PriceMigration{*migration}, byID.Items)

	canceled, err := client.CancelPriceMigration(ctx, migration.ID)
	require.NoError(t, err)
	require.Equal(t, 2, canceled.Canceled)
	require.Empty(t, canceled.RailReleaseRequired)
	require.NotNil(t, canceled.PriceMigration.CanceledAt)
	require.Equal(t, [2]int{0, 2}, [2]int{canceled.PriceMigration.Scheduled, canceled.PriceMigration.Canceled})
	for _, id := range []billing.SubscriptionID{subA, subB} {
		require.Nil(t, w.subscription(embedded, id).ScheduledChange, "a canceled move is not pending")
	}
	again, err := client.CancelPriceMigration(ctx, migration.ID)
	require.NoError(t, err)
	require.Zero(t, again.Canceled, "nothing is left to cancel")

	_, err = client.GetPriceMigration(ctx, billing.PriceMigrationID(uuid.New()))
	requireCode(t, err, http.StatusNotFound, "price_migration_not_found")
}

// A subscription its provider owns moves only where OpenRails can move it at
// the provider: CCBill is refused (blocked, no scheduled change, still billed
// at the old price), and a custom NMI schedule's amount is pushed, moving it
// now.
func TestPriceMigrationProviderOwned(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]

	m := importCCBill(t, w)
	hours := monthHours
	cheaper, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: m.price.ProductID, Key: "cheaper-" + uuid.NewString()[:8], UnitAmount: 4_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(t, err)
	move := billing.CreatePriceMigrationParams{FromPriceID: m.price.ID, ToPriceID: cheaper.ID, FallbackPolicy: billing.MigrationCancelAtPeriodEnd}
	preview, err := client.PreviewPriceMigration(ctx, move)
	require.NoError(t, err)
	require.Equal(t, billing.PriceMigrationRailCounts{RequiresAction: 1}, *preview.ByRail["ccbill"])
	require.Equal(t, billing.MigrationBlocked, preview.Outcomes[0].Disposition)
	require.Equal(t, "rail_requires_user_action", *preview.Outcomes[0].Reason)
	refused, err := client.CreatePriceMigration(ctx, move)
	require.NoError(t, err)
	require.Equal(t, [3]int{1, 0, 1}, [3]int{refused.Matched, refused.Scheduled, refused.Blocked})
	require.Equal(t, billing.MigrationCancelAtPeriodEnd, refused.FallbackPolicy)
	sub := w.subscription(embedded, m.sub)
	require.Equal(t, m.price.ID, sub.PriceID)
	require.Nil(t, sub.ScheduledChange, "a refused move is never pending")
	require.NotContains(t, m.c.noticeKinds(), "subscription_reprice_scheduled")

	group := "g" + uuid.NewString()[:8]
	from := w.tierPrice(group, 2, 1999, monthHours, true)
	to := w.tierPrice(group, 1, 1499, monthHours, true)
	l := w.legacyOnTier(embedded, from, 1999, monthHours, 10*day)
	w.nmi.customSchedule(l.railSub) // a named plan changes only by switching plans
	sales := len(l.tierSales())
	pushed := billing.CreatePriceMigrationParams{FromPriceID: from.ID, ToPriceID: to.ID}
	preview, err = client.PreviewPriceMigration(ctx, pushed)
	require.NoError(t, err)
	require.Equal(t, billing.MigrationScheduled, preview.Outcomes[0].Disposition, "%+v", preview.Outcomes[0])
	nmi, err := client.CreatePriceMigration(ctx, pushed)
	require.NoError(t, err)
	require.Equal(t, [3]int{1, 0, 1}, [3]int{nmi.Matched, nmi.Scheduled, nmi.Applied}, "the pushed schedule bills the new amount from its next rebill")
	require.Equal(t, "14.99", w.nmi.Schedule(l.railSub).Amount)
	require.Len(t, l.tierSales(), sales, "nothing is charged now")
	sub = w.subscription(embedded, l.sub)
	require.Equal(t, to.ID, sub.PriceID)
	require.Nil(t, sub.ScheduledChange)
	require.Contains(t, l.c.noticeKinds(), "subscription_plan_change_scheduled")
}
