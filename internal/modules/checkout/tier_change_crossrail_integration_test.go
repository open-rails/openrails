//go:build integration

package checkout

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Both services use the same real merchant DB as two server replicas would.
// Hold one request after the public replay lookup, let the other rail commit,
// then release the first. The client's key has one owner across both rails.
func TestTierChangeCrossRailKeyReuseDuringPreflightIsRefused(t *testing.T) {
	for _, heldRail := range []string{"stripe", "nmi"} {
		t.Run(heldRail+" loses", func(t *testing.T) {
			stripe := newStripeTierFixture(t)
			nmi := newUpgradeAdoptFixture(t)
			nmi.svc.ProviderSecrets = fakePSPCatalog{scopes: []merchants.PSPScope{*nmi.target.Scope}}
			_, err := nmi.db.Qx(nmi.ctx).Exec(nmi.ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,$4)`, nmi.existingSub.MerchantID, nmi.newPrice.ID, nmi.existingSub.PspID, nmi.gateway.planID)
			require.NoError(t, err)
			_, err = nmi.db.Qx(nmi.ctx).Exec(nmi.ctx, `UPDATE openrails.prices SET amount=60000000 WHERE id=$1`, nmi.newPrice.ID)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = nmi.db.Qx(nmi.ctx).Exec(nmi.ctx, `DELETE FROM openrails.price_psp_bindings WHERE price_id=$1`, nmi.newPrice.ID)
			})
			key := "cross-rail-" + uuid.NewString()
			stripeReq := &TierChangeRequest{PriceID: openrails.PriceID(stripe.pro.ID).String(), SubscriptionID: stripe.sub.ID, IdempotencyKey: key}
			nmiReq := &TierChangeRequest{PriceID: openrails.PriceID(nmi.newPrice.ID).String(), SubscriptionID: nmi.existingSub.ID, IdempotencyKey: key}
			stripeCall := func() (*TierChangeResponse, error) { return stripe.svc.TierChange(stripe.ctx, stripeReq, stripe.user) }
			nmiCall := func() (*TierChangeResponse, error) { return nmi.svc.TierChange(nmi.ctx, nmiReq, nmi.user) }
			blocked, winner := stripeCall, nmiCall
			var arrived <-chan struct{}
			var release func()
			if heldRail == "stripe" {
				arrived, release = stripe.stripe.holdNextRead("/v1/subscriptions/" + stripe.sub.RailSubscriptionID)
			} else {
				block := &heldTierPSPCatalog{fakePSPCatalog: fakePSPCatalog{scopes: []merchants.PSPScope{*nmi.target.Scope}}, arrived: make(chan struct{}), release: make(chan struct{})}
				nmi.svc.ProviderSecrets = block
				arrived = block.arrived
				release = func() { close(block.release) }
				blocked, winner = nmiCall, stripeCall
			}
			var once sync.Once
			unblock := func() { once.Do(release) }
			defer unblock()
			type answer struct {
				response *TierChangeResponse
				err      error
			}
			done := make(chan answer, 1)
			go func() { response, err := blocked(); done <- answer{response, err} }()
			select {
			case <-arrived:
			case <-time.After(20 * time.Second):
				t.Fatal("request never reached its preflight")
			}
			var rows int
			require.NoError(t, nmi.db.Qx(nmi.ctx).QueryRow(nmi.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE payload->>'user_id' IN ($1,$2)`, nmi.user.ID, stripe.user.ID).Scan(&rows))
			require.Zero(t, rows, "both requests see a new key before the first admission")
			accepted, err := winner()
			require.NoError(t, err)
			require.Equal(t, "succeeded", accepted.Status)
			unblock()
			rejected := <-done
			t.Logf("held %s result=%+v error=%v; Stripe updates=%d, NMI creates=%d, NMI sales=%d", heldRail, rejected.response, rejected.err, len(stripe.stripe.posts("/v1/subscriptions/")), nmi.gateway.createCalls.Load(), nmi.gateway.saleCalls.Load())
			require.Nil(t, rejected.response, "a different customer's request must not be admitted on a second rail")
			var conflict *TierChangeError
			require.ErrorAs(t, rejected.err, &conflict)
			require.Equal(t, http.StatusConflict, conflict.HTTPStatus)
			require.Equal(t, openrails.CodeTierChangeIdempotencyConflict, conflict.Code)
			require.NoError(t, nmi.db.Qx(nmi.ctx).QueryRow(nmi.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE subscription_id IN ($1,$2) AND intent_type IN ('nmi_upgrade','stripe_tier_change')`, nmi.existingSub.ID, stripe.sub.ID).Scan(&rows))
			require.Equal(t, 1, rows, "one merchant/client key owns exactly one durable tier operation")
			again, err := winner()
			require.NoError(t, err)
			requireSameWire(t, accepted, again)
			_, err = blocked()
			require.ErrorAs(t, err, &conflict)
			require.Equal(t, openrails.CodeTierChangeIdempotencyConflict, conflict.Code)
			if heldRail == "stripe" {
				require.Empty(t, stripe.stripe.posts("/v1/subscriptions/"))
				require.Equal(t, stripe.basic.ID, stripe.local(t).PriceID)
				require.EqualValues(t, 1, nmi.gateway.createCalls.Load())
				require.EqualValues(t, 1, nmi.gateway.saleCalls.Load())
			} else {
				require.Len(t, stripe.stripe.posts("/v1/subscriptions/"), 1)
				require.Zero(t, nmi.gateway.createCalls.Load())
				require.Zero(t, nmi.gateway.saleCalls.Load())
				old, err := nmi.svc.SubscriptionService.GetByID(nmi.ctx, nmi.existingSub.ID)
				require.NoError(t, err)
				require.Equal(t, models.StatusActive, old.Status)
				require.Equal(t, nmi.existingSub.PriceID, old.PriceID)
			}
		})
	}
}

type heldTierPSPCatalog struct {
	fakePSPCatalog
	arrived, release chan struct{}
	once             sync.Once
}

func (p *heldTierPSPCatalog) ActivePSPScopesForRail(ctx context.Context, id merchant.ID, rail, environment string) ([]merchants.PSPScope, error) {
	p.once.Do(func() { close(p.arrived); <-p.release })
	return p.fakePSPCatalog.ActivePSPScopesForRail(ctx, id, rail, environment)
}
