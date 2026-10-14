//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/openrailstest"
)

// A merchant's CCBill members import from DataLink: a member's username
// links through the directory, the product's declared CCBill price prices
// it (CCBill has no catalog to read), and DataLink's rebills land as
// payments in that price's currency.
func TestPSPImportFromCCBill(t *testing.T) {
	t.Parallel()
	dl := newDataLinkFake(t)
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12, func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{CCBillDataLinkURL: dl.URL}
	})
	w.declare = func(psps map[string]openrails.PSPConfig) {
		account := psps["ccbill"]
		account.Secrets = map[string]string{"salt": "e2e-ccbill-salt", "datalink_username": "e2e-datalink", "datalink_password": "e2e-datalink-password"}
		psps["ccbill"] = account
	}
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.start()
	ctx := t.Context()
	client := w.client[remote]

	entitlement := "content:ccbill-" + uuid.NewString()[:6]
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "premium-" + uuid.NewString()[:8], DisplayName: "Premium", Entitlements: []string{entitlement}})
	require.NoError(t, err)
	hours := monthHours
	_, err = client.CreateProviderPrice(ctx, billing.CreateProviderPriceParams{ProductID: product.ID, Key: product.Key + "-ccbill", PSP: "ccbill", FlexID: ccbillFlexID,
		RecurringBillingOptionID: ccbillRBO, UnitAmount: new(int64(9_990_000)), Currency: "USD", BillingIntervalHours: &hours, Archived: true})
	require.NoError(t, err)
	// A second CCBill price on the product: the import names the one its members bill.
	_, err = client.CreateProviderPrice(ctx, billing.CreateProviderPriceParams{ProductID: product.ID, Key: product.Key + "-ccbill-annual", PSP: "ccbill", FlexID: ccbillFlexID,
		RecurringBillingOptionID: "0000009999", UnitAmount: new(int64(99_000_000)), Currency: "USD", BillingIntervalHours: new(365 * 24), Archived: true})
	require.NoError(t, err)

	member, stranger := w.newCustomer(), w.newCustomer()
	directory.Put(userinfo.User{ID: member.id, Email: "member@host.test", Username: "legacy_member"})
	directory.Put(userinfo.User{ID: stranger.id, Username: "someone_else"})
	sub, other := ccbillNumericID(), ccbillNumericID()
	signup := w.clock.Now().Add(-40 * 24 * time.Hour)
	rebillAt := w.clock.Now().Add(-10 * 24 * time.Hour)
	next := rebillAt.Add(monthHours * time.Hour)
	dl.mu.Lock()
	dl.members = []string{
		fmt.Sprintf(`"ACTIVEMEMBERS","999999","0000","%s","%s","legacy_member","member@host.test","1","%s",""`, sub, signup.Format("2006-01-02"), next.Format("2006-01-02")),
		fmt.Sprintf(`"ACTIVEMEMBERS","999999","0000","%s","%s","nobody_here","nobody@elsewhere.test","1","%s",""`, other, signup.Format("2006-01-02"), next.Format("2006-01-02")),
	}
	dl.mu.Unlock()
	rebill := ccbillNumericID()
	dl.export(fmt.Sprintf(`"REBILL","999999","0000","%s","%s","%s","9.99"`, sub, ccbillTimestamp(rebillAt), rebill))

	since := rebillAt.Add(-time.Hour)
	imp, err := w.client[embedded].CreatePSPImport(ctx, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since,
		Rules:    []billing.PSPImportRule{{Fact: "username", Via: billing.PSPImportViaDirectory}},
		Products: []billing.PSPImportProduct{{Product: product.Key, Price: product.Key + "-ccbill"}}})
	require.NoError(t, err)
	imp = w.pspImported(imp.ID, billing.PSPImportPulled)
	recs := w.pspRecords(imp.ID)
	require.Equal(t, member.cid(), *recs[sub].Customer)
	require.Equal(t, "subscription:"+sub, *recs[rebill].LinkedVia)
	require.Equal(t, billing.PSPImportUnmatched, recs[other].State)

	_, err = client.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	imp = w.pspImported(imp.ID, billing.PSPImportApplied)
	recs = w.pspRecords(imp.ID)
	require.Equal(t, billing.PSPImportImported, recs[sub].State, "%v", recs[sub].Reason)

	subs, err := client.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: member.cid()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	require.Equal(t, sub, str(subs.Items[0].RailSubscriptionID))
	require.Equal(t, "active", string(subs.Items[0].Status))
	require.True(t, member.entitled(entitlement))
	payments, err := client.ListPayments(ctx, billing.PaymentListParams{CustomerID: member.cid(), TransactionID: rebill})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1, "DataLink's rebill is a payment")
	require.Equal(t, int64(9_990_000), payments.Items[0].Amount)
	require.Equal(t, "USD", payments.Items[0].Currency, "the declared price denominates DataLink's amount")
	require.Zero(t, dl.canceled(sub), "the import only read DataLink")
}
