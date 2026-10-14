//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/openrailstest"
)

// pspImported waits for an import's job to reach status.
func (w *world) pspImported(id billing.PSPImportID, status billing.PSPImportStatus) *billing.PSPImport {
	w.t.Helper()
	var out *billing.PSPImport
	require.Eventually(w.t, func() bool {
		var err error
		out, err = w.client[remote].GetPSPImport(w.t.Context(), id)
		require.NoError(w.t, err)
		require.NotEqual(w.t, billing.PSPImportFailed, out.Status, "import failed: %v", out.Error)
		return out.Status == status
	}, 30*time.Second, 50*time.Millisecond)
	return out
}

// pspRecords maps an import's records by ref.
func (w *world) pspRecords(id billing.PSPImportID) map[string]billing.PSPImportRecord {
	w.t.Helper()
	out := map[string]billing.PSPImportRecord{}
	cursor := ""
	for {
		page, err := w.client[remote].ListPSPImportRecords(w.t.Context(), id, billing.PSPImportRecordListParams{PageRequest: billing.PageRequest{Limit: 2, Cursor: cursor}})
		require.NoError(w.t, err)
		for _, r := range page.Items {
			out[r.Ref] = r
		}
		if page.Next == "" {
			return out
		}
		cursor = page.Next
	}
}

// A merchant's NMI subscribers import straight from the gateway: the pull
// only reads, rules and mappings link each vault, subscription and charge to
// the merchant's customers, and the linked ones land as subscriptions,
// payment methods and payments. Unmatched and conflicting objects stay staged.
func TestPSPImportFromNMI(t *testing.T) {
	t.Parallel()
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12)
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.start()
	w.waive("recorded", "the conflicted member's charge and the one-off charge stay staged, never landed")
	ctx := t.Context()
	embeddedClient, remoteClient := w.client[embedded], w.client[remote]
	mutations := len(w.nmi.Calls())

	entitlement := "content:imported-" + uuid.NewString()[:6]
	product, err := embeddedClient.CreateProduct(ctx, billing.CreateProductParams{Key: "premium-" + uuid.NewString()[:8], DisplayName: "Premium", Entitlements: []string{entitlement}})
	require.NoError(t, err)
	plan := "premium_" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Premium", Amount: "9.99", Days: 30})

	paid := w.clock.Now().Add(10 * 24 * time.Hour)
	start := paid.AddDate(0, 0, -30)
	type member struct {
		vault, schedule string
		sale            nmimock.Sale
	}
	add := func(email, shipping, order string) member {
		vault := w.nmi.AddVault(visa)
		w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Email, v.ShippingID = email, shipping })
		schedule := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: "9.99", Days: 30, Order: order, NextBilling: paid, Created: start})
		return member{vault, schedule, w.nmi.AddScheduleSale(schedule, start)}
	}
	a, b, c, d := w.newCustomer(), w.newCustomer(), w.newCustomer(), w.newCustomer()
	directory.Put(userinfo.User{ID: a.id, Email: "a@host.test"})
	byEmail := add("A@Host.Test", "", "ord-a")        // the directory's email
	byShipping := add("", "shipment-1002", "ord-b")   // legacy user 1002
	byOrder := add("", "", "77")                      // legacy order 77
	unmatched := add("d@elsewhere.test", "", "ord-d") // nobody's
	conflicted := add("a@host.test", "shipment-1002", "ord-e")
	oneOff := w.nmi.AddSale(nmimock.Sale{Vault: byEmail.vault, Amount: "4.99", OrderID: "one-off", At: start.Add(-5 * 24 * time.Hour)})

	res, err := embeddedClient.SetPSPImportMappings(ctx, billing.SetPSPImportMappingsParams{Mappings: []billing.PSPImportMapping{
		{Mapping: "legacy_user", Key: "1002", Customer: new(b.cid())},
		{Mapping: "legacy_order", Key: "77", Customer: new(c.cid())},
	}})
	require.NoError(t, err)
	require.Equal(t, 2, res.Set)

	since := start.AddDate(0, 0, -30)
	imp, err := embeddedClient.CreatePSPImport(ctx, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since,
		Rules: []billing.PSPImportRule{
			{Fact: "email", Via: billing.PSPImportViaDirectory},
			{Fact: "shipping_id", Pattern: "shipment-{key}", Via: billing.PSPImportViaMapping, Mapping: "legacy_user"},
			{Fact: "order_id", Via: billing.PSPImportViaMapping, Mapping: "legacy_order"},
		},
		Products: []billing.PSPImportProduct{{Product: product.Key}}})
	require.NoError(t, err)
	require.Equal(t, billing.PSPImportPulling, imp.Status)
	imp = w.pspImported(imp.ID, billing.PSPImportPulled)
	require.NotNil(t, imp.PulledAt)
	require.True(t, imp.HistoryThrough.Equal(*imp.PulledAt), "history is read up to the roster's read")

	recs := w.pspRecords(imp.ID)
	state := func(ref string) billing.PSPImportRecordState { return recs[ref].State }
	require.Equal(t, billing.PSPImportLinked, state(byEmail.vault))
	require.Equal(t, a.cid(), *recs[byEmail.vault].Customer)
	require.Equal(t, "rule:1", *recs[byEmail.vault].LinkedVia)
	require.Equal(t, "customer:"+byEmail.vault, *recs[byEmail.schedule].LinkedVia, "a subscription belongs to its vault's customer")
	require.Equal(t, "subscription:"+byEmail.schedule, *recs[byEmail.sale.TransactionID].LinkedVia, "a charge attributes by its order id")
	require.Equal(t, b.cid(), *recs[byShipping.vault].Customer)
	require.Equal(t, "rule:2", *recs[byShipping.vault].LinkedVia)
	require.Equal(t, c.cid(), *recs[byOrder.schedule].Customer)
	require.Equal(t, "rule:3", *recs[byOrder.schedule].LinkedVia)
	require.Equal(t, "subscription:"+byOrder.schedule, *recs[byOrder.vault].LinkedVia, "a vault takes the customer its subscriptions name")
	require.Equal(t, billing.PSPImportUnmatched, state(unmatched.vault))
	require.Equal(t, billing.PSPImportUnmatched, state(unmatched.schedule))
	require.Equal(t, billing.PSPImportConflict, state(conflicted.vault))
	require.Contains(t, *recs[conflicted.vault].Reason, a.id)
	require.Contains(t, *recs[conflicted.vault].Reason, b.id)
	require.Equal(t, billing.PSPImportConflict, state(conflicted.schedule), "what belongs to a conflicted vault is not linked")
	require.Equal(t, billing.PSPImportLinked, state(oneOff.TransactionID))
	require.Nil(t, recs[oneOff.TransactionID].SubscriptionRef, "a charge before every subscription names none")

	// The merchant links the unmatched vault by hand; what belongs to it follows.
	imp, err = remoteClient.LinkPSPImport(ctx, imp.ID, billing.LinkPSPImportParams{Links: []billing.PSPImportLink{{Record: recs[unmatched.vault].ID, Customer: new(d.cid())}}})
	require.NoError(t, err)
	recs = w.pspRecords(imp.ID)
	require.Equal(t, "manual", *recs[unmatched.vault].LinkedVia)
	require.Equal(t, d.cid(), *recs[unmatched.schedule].Customer)

	imp, err = remoteClient.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	imp = w.pspImported(imp.ID, billing.PSPImportApplied)

	prices, err := remoteClient.ListPrices(ctx, billing.PriceListParams{ProductID: product.ID})
	require.NoError(t, err)
	require.Len(t, prices.Items, 1, "the plan became a provider price of the product")
	require.True(t, prices.Items[0].Archived, "kept for imported members, never sold")
	require.Equal(t, int64(9_990_000), prices.Items[0].UnitAmount, "terms read from the NMI plan")

	for _, m := range []struct {
		who *customer
		row member
	}{{a, byEmail}, {b, byShipping}, {c, byOrder}, {d, unmatched}} {
		subs, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: m.who.cid()})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		sub := subs.Items[0]
		require.Equal(t, m.row.schedule, str(sub.RailSubscriptionID))
		require.Equal(t, "active", string(sub.Status))
		require.True(t, m.who.entitled(entitlement))
		methods, err := remoteClient.ListPaymentMethods(ctx, m.who.cid(), billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1)
		require.Equal(t, "4242", *methods.Items[0].Card.Last4)
		payments, err := remoteClient.ListPayments(ctx, billing.PaymentListParams{CustomerID: m.who.cid(), TransactionID: m.row.sale.TransactionID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "the signup charge is history")
		require.Equal(t, int64(9_990_000), payments.Items[0].Amount)
	}
	recs = w.pspRecords(imp.ID)
	for _, ref := range []string{byEmail.vault, byEmail.schedule, byEmail.sale.TransactionID, byOrder.vault, unmatched.schedule} {
		require.Equal(t, billing.PSPImportImported, state(ref), ref)
	}
	require.Equal(t, billing.PSPImportBlocked, recs[oneOff.TransactionID].State)
	require.Contains(t, *recs[oneOff.TransactionID].Reason, "no subscription")
	require.Equal(t, billing.PSPImportConflict, recs[conflicted.schedule].State)
	nobody, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{Rail: "nmi"})
	require.NoError(t, err)
	for _, s := range nobody.Items {
		require.NotEqual(t, conflicted.schedule, str(s.RailSubscriptionID), "a conflict never lands")
	}

	// Linking and landing again changes nothing.
	_, err = remoteClient.LinkPSPImport(ctx, imp.ID, billing.LinkPSPImportParams{})
	require.NoError(t, err)
	_, err = remoteClient.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	w.pspImported(imp.ID, billing.PSPImportApplied)
	subs, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: a.cid()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	require.Equal(t, billing.PSPImportImported, w.pspRecords(imp.ID)[byEmail.schedule].State)

	require.Len(t, w.nmi.Calls(), mutations, "the import only read NMI")
	require.Positive(t, w.nmi.Reads()["query:recurring"])

	require.NoError(t, remoteClient.DeletePSPImport(ctx, imp.ID))
	_, err = remoteClient.GetPSPImport(ctx, imp.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	subs, err = remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: a.cid()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1, "what an import landed stays")
}
