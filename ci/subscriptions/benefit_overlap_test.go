//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Two purchases of one timed pass admitted before either is paid pay for
// consecutive windows, never the same one twice (tracker 1137): settlement
// stacks each window after what an earlier settlement granted.
func TestConcurrentTimedPassesStack(t *testing.T) {
	t.Parallel()
	p := newSolanaPay(t)
	buyer := p.w.newCustomer()
	first, second := p.checkout(buyer), p.checkout(buyer)
	// Both wallets relay their landed signatures at once: the two
	// settlements race, as two pollers or relays would.
	var settled sync.WaitGroup
	for _, req := range []transferRequest{first, second} {
		sig := p.pay(req, req.amount)
		settled.Go(func() {
			confirmed, err := p.w.engineConfirm(req.id, sig)
			if assert.NoError(t, err) {
				assert.Equal(t, "succeeded", confirmed.Status)
			}
		})
	}
	settled.Wait()
	require.Equal(t, []int{1, 1}, []int{p.payments(first), p.payments(second)}, "both transfers are credited once")
	rows, err := p.w.pool.Query(t.Context(), p.sql(`SELECT starts_at, ends_at FROM $schema.product_access
		WHERE customer_id = $1 AND deleted_at IS NULL AND revoked_at IS NULL ORDER BY starts_at`), uuid.MustParse(buyer.id))
	require.NoError(t, err)
	type window struct {
		Starts time.Time
		Ends   *time.Time
	}
	windows, err := pgx.CollectRows(rows, pgx.RowToStructByPos[window])
	require.NoError(t, err)
	require.Len(t, windows, 2)
	require.NotNil(t, windows[0].Ends)
	require.False(t, windows[1].Starts.Before(*windows[0].Ends), "the second pass starts when the first ends")
}

// A recurring product granting an entitlement another recurring product
// already grants is refused unless both share a tier group (tracker 1137):
// otherwise one customer could hold both and pay twice for that benefit.
func TestCatalogRefusesOverlappingRecurringBenefits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	w.membership("content:shared", 9_990_000)
	hours := monthHours
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "member-v2-" + uuid.NewString()[:8], DisplayName: "Membership v2", Entitlements: []string{"content:shared"}})
	require.NoError(t, err)
	_, err = client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	requireCode(t, err, http.StatusConflict, billing.CodeCatalogBenefitOverlap)

	group := "g" + uuid.NewString()[:8]
	for rank := 1; rank <= 2; rank++ {
		tiered, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "tier-" + uuid.NewString()[:8], DisplayName: "Tier", TierGroup: &group, TierRank: rank, Entitlements: []string{"content:tiered"}})
		require.NoError(t, err)
		_, err = client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: tiered.ID, Key: tiered.Key + "-usd", UnitAmount: int64(rank) * 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err, "a shared tier group may share benefits")
	}
}
