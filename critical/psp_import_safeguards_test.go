//go:build integration

package critical_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestPSPImportSafeguards(t *testing.T) {
	t.Run("OR-J38-S76/a-failed-import-is-never-applied", func(t *testing.T) {
		iw := bootImportWorld(t, "stripe")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil), "pm_bob", "price_basic", 999, start, start.AddDate(0, 1, 0))
		iw.clock.Advance(time.Minute)
		iw.start(t)
		id := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}}).ID
		_, err := iw.api.CreateProduct(t.Context(), billing.CreateProductParams{Key: "basic", DisplayName: "basic", Entitlements: []string{"member:basic"}})
		require.NoError(t, err)
		_, err = iw.api.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "basic", Key: "stripe-basic", PSP: "stripe", PriceID: "price_basic", Archived: true})
		require.NoError(t, err)
		require.NoError(t, iw.link(t, id, billing.LinkPSPImportParams{}))

		// Every landing of the import's book loses its answer after it
		// commits: the apply fails.
		remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
			if site.Point == failpoint.Committed && site.Kind == "billing_import" {
				return errors.New("lost billing import answer")
			}
			return nil
		})
		t.Cleanup(remove)
		_, err = iw.alpha.ApplyPSPImport(t.Context(), id)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			imp, err := iw.api.GetPSPImport(t.Context(), id)
			if err == nil && imp.Status == billing.PSPImportFailed {
				return true
			}
			for _, job := range iw.jobs(t, id) {
				if job.State == rivertype.JobStateRetryable {
					_, _ = iw.fleet.JobRetry(t.Context(), job.ID)
				}
			}
			return false
		}, 60*time.Second, 50*time.Millisecond, "the apply fails")
		remove()

		_, err = iw.alpha.ApplyPSPImport(t.Context(), id)
		status, code := refusalCode(t, err)
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, billing.CodeResourceConflict, code)
		imp, err := iw.api.GetPSPImport(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, billing.PSPImportFailed, imp.Status, "a person creates a failed import again")
	})

	t.Run("OR-J38-S77/a-price-picked-for-an-import-record-is-never-deleted", func(t *testing.T) {
		iw := newImportWorld(t, "ccbill")
		for _, key := range []string{"picked", "current"} {
			_, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
		}
		price := func(product, key string, archived bool) *billing.Price {
			t.Helper()
			created, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: product, Key: key, PSP: "ccbill", FlexID: "681cb38f-afb9-4665-931f-2b896072178a",
				RecurringBillingOptionID: "0000000023", UnitAmount: ptr(int64(23_000_000)), Currency: "USD", BillingIntervalHours: ptr(720), Archived: archived})
			require.NoError(t, err)
			return created
		}
		picked := price("picked", "ccbill-23-legacy", true)
		dave := authtest.NewUser(t, iw.auth)
		next := iw.clock.Now().Add(20 * 24 * time.Hour)
		ref := iw.ccbill.AddMember(ccbillmock.Member{Username: dave.Username, Email: dave.Email, Signup: iw.clock.Now().Add(-40 * 24 * time.Hour), Rebill: &next})
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}})
		record := iw.records(t, imp.ID)[ref]
		require.NoError(t, iw.link(t, imp.ID, billing.LinkPSPImportParams{Prices: []billing.PSPImportPriceLink{{Record: record.ID, Price: &picked.ID}}}))
		// A live price of another product takes over the provider object: the
		// picked price keeps no PSP link.
		price("current", "ccbill-23", false)

		status, code := refusalCode(t, iw.alphaRemote.DeleteProduct(t.Context(), picked.ProductID, billing.DeleteProductParams{}))
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, billing.CodeProductInUse, code)
		kept, err := iw.alphaRemote.GetPrice(t.Context(), picked.ID)
		require.NoError(t, err)
		require.Equal(t, picked.ID, kept.ID)
		require.Equal(t, picked.ID, *iw.records(t, imp.ID)[ref].ManualPrice, "the pick still names it")
	})
}
