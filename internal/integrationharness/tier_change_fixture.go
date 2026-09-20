//go:build integration

package integrationharness

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// LoopbackStripeSecretKey is the secret key seeded for loopback Stripe accounts.
const LoopbackStripeSecretKey = "sk_test_loopback"

// ArmLoopbackStripe arms one Stripe account for the merchant in the canonical
// armed state; the runtime's sandbox API URL decides where its requests go.
func (h *Harness) ArmLoopbackStripe(rt *app.Runtime, mid merchant.ID) uuid.UUID {
	h.t.Helper()
	account := "acct_" + strings.ReplaceAll(mid.String(), "-", "")[:12]
	SeedPSPs(h.ctx, h.t, rt, mid, config.PSPSet{"stripe": {Rail: models.RailStripe, AccountID: account, Stripe: &config.StripeRailConfig{SecretKey: LoopbackStripeSecretKey}}})
	var id uuid.UUID
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT id FROM billing.psps WHERE merchant_id=$1 AND rail='stripe' AND account_id=$2`, mid.UUID(), account).Scan(&id))
	return id
}

// TierChangeFixture is an active Stripe subscription on the basic tier of a
// two-tier group, with both tiers bound to Stripe prices.
type TierChangeFixture struct {
	Merchant       merchant.ID
	Customer       uuid.UUID
	Subscription   openrails.SubscriptionID
	StripeSub      string
	BasicPrice     openrails.PriceID
	ProPrice       openrails.PriceID
	BasicStripe    string
	ProStripe      string
	PeriodEnd      time.Time
	BasicAmount    int64
	ProAmount      int64
	SubscriptionID uuid.UUID
}

// SeedStripeTierSubscription arms a loopback Stripe account, seeds a
// basic/pro tier group bound to Stripe prices, and gives a fresh customer an
// active basic subscription that the gateway also knows about.
func (h *Harness) SeedStripeTierSubscription(rt *app.Runtime, mid merchant.ID, gateway *FakeStripeGateway) TierChangeFixture {
	h.t.Helper()
	psp := h.ArmLoopbackStripe(rt, mid)
	pool := h.sharedPool()
	sfx := uuid.NewString()[:8]
	now := time.Now().UTC().Truncate(time.Second)
	group := "tier-" + sfx
	seed := func(key, name string, rank int, amount int64, ref string) uuid.UUID {
		product, price := uuid.New(), uuid.New()
		_, err := pool.Exec(h.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,tier_group,tier_rank) VALUES($1,$2,$3,$4,$5,$6)`, product, mid.UUID(), key+"-"+sfx, name, group, rank)
		require.NoError(h.t, err)
		_, err = pool.Exec(h.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,$5,'USD',720,true)`, price, mid.UUID(), product, key+"-"+sfx+"-monthly", amount)
		require.NoError(h.t, err)
		_, err = pool.Exec(h.ctx, `INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,price_ref) VALUES($1,$2,$3,$4)`, mid.UUID(), price, psp, ref)
		require.NoError(h.t, err)
		return price
	}
	basicStripe, proStripe := "price_basic_"+sfx, "price_pro_"+sfx
	basic := seed("basic", "Basic", 1, 10_000_000, basicStripe)
	pro := seed("pro", "Pro", 2, 30_000_000, proStripe)
	var basicProduct uuid.UUID
	require.NoError(h.t, pool.QueryRow(h.ctx, `SELECT product_id FROM billing.prices WHERE id=$1`, basic).Scan(&basicProduct))

	customer, method, subscription := uuid.New(), uuid.New(), uuid.New()
	_, err := pool.Exec(h.ctx, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), customer)
	require.NoError(h.t, err)
	_, err = dbtest.Queries(pool).CreatePaymentMethod(h.ctx, gen.CreatePaymentMethodParams{
		ID: method, MerchantID: mid.UUID(), CustomerID: customer, Rail: string(models.RailStripe), PspID: psp,
		InitialTransactionID: "init-" + method.String(), RailCustomerRef: "cus_" + sfx, RailMethodRef: "pm_" + sfx,
	})
	require.NoError(h.t, err)
	stripeSub := "sub_" + sfx
	periodStart, periodEnd := now.Add(-5*24*time.Hour), now.Add(25*24*time.Hour)
	_, err = pool.Exec(h.ctx, `INSERT INTO billing.subscriptions
	        (id, merchant_id, customer_id, price_id, product_id, status, rail, psp_id, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at, payment_method_id)
	      VALUES ($1, $2, $3, $4, $5, 'active', 'stripe', $6, $7, $8, $9, $8, $10)`,
		subscription, mid.UUID(), customer, basic, basicProduct, psp, stripeSub, periodStart, periodEnd, method)
	require.NoError(h.t, err)
	gateway.DeclareSubscription(stripeSub, basicStripe, periodStart, periodEnd)
	return TierChangeFixture{Merchant: mid, Customer: customer, Subscription: openrails.SubscriptionID(subscription), SubscriptionID: subscription, StripeSub: stripeSub,
		BasicPrice: openrails.PriceID(basic), ProPrice: openrails.PriceID(pro), BasicStripe: basicStripe, ProStripe: proStripe, PeriodEnd: periodEnd, BasicAmount: 10_000_000, ProAmount: 30_000_000}
}

// NMITierFixture is an active NMI subscription on the basic tier of a
// two-tier group, with both tiers bound to NMI plans and a vaulted instrument.
type NMITierFixture struct {
	Merchant       merchant.ID
	Subscription   openrails.SubscriptionID
	SubscriptionID uuid.UUID
	BasicPrice     openrails.PriceID
	ProPrice       openrails.PriceID
	ProPlan        string
	Vault          string
	ProAmount      int64
}

// SeedNMITierSubscription arms a loopback NMI account, seeds a basic/pro tier
// group bound to NMI plans, and gives a fresh customer an active basic
// subscription paid by a vaulted instrument.
func (h *Harness) SeedNMITierSubscription(rt *app.Runtime, mid merchant.ID) NMITierFixture {
	h.t.Helper()
	psp := h.ArmLoopbackNMI(rt, mid)
	pool := h.sharedPool()
	sfx := uuid.NewString()[:8]
	now := time.Now().UTC().Truncate(time.Second)
	group := "tier-" + sfx
	seed := func(key, name string, rank int, amount int64, plan string) uuid.UUID {
		product, price := uuid.New(), uuid.New()
		_, err := pool.Exec(h.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,tier_group,tier_rank) VALUES($1,$2,$3,$4,$5,$6)`, product, mid.UUID(), key+"-"+sfx, name, group, rank)
		require.NoError(h.t, err)
		_, err = pool.Exec(h.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,$5,'USD',720,true)`, price, mid.UUID(), product, key+"-"+sfx+"-monthly", amount)
		require.NoError(h.t, err)
		_, err = pool.Exec(h.ctx, `INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,$4)`, mid.UUID(), price, psp, plan)
		require.NoError(h.t, err)
		return price
	}
	proPlan := "plan-pro-" + sfx
	basic := seed("basic", "Basic", 1, 10_000_000, "plan-basic-"+sfx)
	pro := seed("pro", "Pro", 2, 30_000_000, proPlan)
	var basicProduct uuid.UUID
	require.NoError(h.t, pool.QueryRow(h.ctx, `SELECT product_id FROM billing.prices WHERE id=$1`, basic).Scan(&basicProduct))

	customer, method, subscription := uuid.New(), uuid.New(), uuid.New()
	vault := "vault-" + sfx
	_, err := pool.Exec(h.ctx, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), customer)
	require.NoError(h.t, err)
	_, err = dbtest.Queries(pool).CreatePaymentMethod(h.ctx, gen.CreatePaymentMethodParams{
		ID: method, MerchantID: mid.UUID(), CustomerID: customer, Rail: string(models.RailNMI), PspID: psp,
		InitialTransactionID: "init-" + method.String(), RailCustomerRef: vault,
	})
	require.NoError(h.t, err)
	dbtest.SeedNMIStoredCredentialRefs(h.ctx, h.t, pool, method)
	periodStart, periodEnd := now.Add(-5*24*time.Hour), now.Add(25*24*time.Hour)
	_, err = pool.Exec(h.ctx, `INSERT INTO billing.subscriptions
	        (id, merchant_id, customer_id, price_id, product_id, status, rail, psp_id, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at, payment_method_id)
	      VALUES ($1, $2, $3, $4, $5, 'active', 'nmi', $6, $7, $8, $9, $8, $10)`,
		subscription, mid.UUID(), customer, basic, basicProduct, psp, "rsub-basic-"+sfx, periodStart, periodEnd, method)
	require.NoError(h.t, err)
	return NMITierFixture{Merchant: mid, Subscription: openrails.SubscriptionID(subscription), SubscriptionID: subscription,
		BasicPrice: openrails.PriceID(basic), ProPrice: openrails.PriceID(pro), ProPlan: proPlan, Vault: vault, ProAmount: 30_000_000}
}

// TierChangeOperation is the durable tier change operation for a
// subscription, read from the ledger.
type TierChangeOperation struct {
	ID     uuid.UUID
	Status string
}

// LatestTierChangeOperation returns the newest tier change operation (NMI
// upgrade or Stripe tier change) the subscription owns.
func (h *Harness) LatestTierChangeOperation(subscription uuid.UUID) TierChangeOperation {
	h.t.Helper()
	var op TierChangeOperation
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT id, status FROM billing.rail_intents WHERE intent_type IN ('nmi_upgrade','stripe_tier_change') AND subscription_id=$1 ORDER BY created_at DESC LIMIT 1`, subscription).Scan(&op.ID, &op.Status))
	return op
}

// TierChangeOperations counts every tier change operation for the subscription.
func (h *Harness) TierChangeOperations(subscription uuid.UUID) int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT count(*) FROM billing.rail_intents WHERE intent_type IN ('nmi_upgrade','stripe_tier_change') AND subscription_id=$1`, subscription).Scan(&n))
	return n
}

// LocalSubscriptionStatus reads the subscription's local status.
func (h *Harness) LocalSubscriptionStatus(subscription uuid.UUID) string {
	h.t.Helper()
	var status string
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT status FROM billing.subscriptions WHERE id=$1`, subscription).Scan(&status))
	return status
}

// LocalSubscriptionPrice reads the subscription's current local price.
func (h *Harness) LocalSubscriptionPrice(subscription uuid.UUID) openrails.PriceID {
	h.t.Helper()
	var price uuid.UUID
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT price_id FROM billing.subscriptions WHERE id=$1`, subscription).Scan(&price))
	return openrails.PriceID(price)
}
