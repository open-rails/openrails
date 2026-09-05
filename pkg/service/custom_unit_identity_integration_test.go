//go:build integration

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCustomUnitIdentityRenameReclaimAndCapture(t *testing.T) {
	ctx := context.Background()
	database := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	admin := dbtest.SharedSuperuserPGXPool(t)
	redis := dbtest.NewSharedRedisClient(t)
	t.Cleanup(func() { _ = redis.Close() })
	require.NoError(t, redis.Ping(ctx).Err())
	merchantA, merchantB := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	groupA, groupB := uuid.NewString(), uuid.NewString()
	prefix := "unit-" + uuid.NewString()[:8]
	old, newName := prefix+"-old", prefix+"-new"
	_, err := admin.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status,permission_group_id) VALUES($1,$2,'active',$3),($4,$5,'active',$6)`, merchantA.UUID(), old, groupA, merchantB.UUID(), prefix+"-shadow", groupB)
	require.NoError(t, err)
	names := map[string]string{groupA: old, groupB: prefix + "-shadow"}
	claims := map[string]string{old: groupA, prefix + "-shadow": groupB}
	directory := func() *merchants.Service {
		dir, err := merchants.NewDirectoryService(db.WrapPool(database.Pool(), ""))
		require.NoError(t, err)
		dir.WithGroupSlugResolver(func(_ context.Context, slug string) (string, string, error) {
			g, ok := claims[slug]
			if !ok {
				return "", "", merchants.ErrMerchantNotFound
			}
			return g, names[g], nil
		})
		dir.WithGroupIDResolver(func(_ context.Context, g string) (string, error) {
			n, ok := names[g]
			if !ok {
				return "", merchants.ErrMerchantNotFound
			}
			return n, nil
		})
		return dir
	}
	rt := &app.Runtime{DB: database, ProductService: catalog.NewProductService(database), MoneyService: money.NewMoneyService(database), EntitlementService: entitlements.NewEntitlementService(database), RedisClient: redis, Merchants: directory()}
	svc, err := New(rt)
	require.NoError(t, err)
	a := merchant.WithID(ctx, merchantA)
	b := merchant.WithID(ctx, merchantB)
	require.NoError(t, svc.SyncCatalogSidecars(a, SyncCatalogSidecarsRequest{CreditBalances: []CatalogCreditBalanceSpec{{Key: "tokens", Unit: old + "/tokens"}}}))
	require.NoError(t, svc.SyncCatalogSidecars(b, SyncCatalogSidecarsRequest{CreditBalances: []CatalogCreditBalanceSpec{{Key: "tokens", Unit: "tokens"}}}))
	var unitA, unitB uuid.UUID
	require.NoError(t, admin.QueryRow(ctx, `SELECT id FROM openrails.custom_credit_types WHERE merchant_id=$1 AND name='tokens'`, merchantA.UUID()).Scan(&unitA))
	require.NoError(t, admin.QueryRow(ctx, `SELECT id FROM openrails.custom_credit_types WHERE merchant_id=$1 AND name='tokens'`, merchantB.UUID()).Scan(&unitB))
	require.NotEqual(t, unitA, unitB)
	canonical := money.CreditUnitCode(unitA)
	_, err = admin.Exec(ctx, `UPDATE openrails.custom_credit_types SET decimals=3 WHERE merchant_id=$1 AND id=$2`, merchantA.UUID(), unitA)
	require.NoError(t, err)
	productKey := prefix + "-topup"
	product, err := svc.CreateProduct(a, CreateProductRequest{Key: productKey, DisplayName: "Tokens", CreditsSpec: CreditsSpec{"tokens": {Unit: "tokens", Amount: 10, Cadence: CreditGrantCadencePerRenewal}}})
	require.NoError(t, err)
	require.Equal(t, old+"/tokens", product.CreditsSpec["tokens"].Unit)
	var productUnit string
	require.NoError(t, admin.QueryRow(ctx, `SELECT credits_spec->'tokens'->>'unit' FROM openrails.products WHERE merchant_id=$1 AND id=$2`, merchantA.UUID(), product.ID).Scan(&productUnit))
	require.Equal(t, canonical, productUnit)
	require.NoError(t, svc.SyncCatalogSidecars(a, SyncCatalogSidecarsRequest{
		CreditBalances:  []CatalogCreditBalanceSpec{{Key: "tokens", Unit: old + "/tokens"}},
		CreditPurchases: []CatalogCreditPurchasePriceSpec{{ProductKey: productKey, Ordinal: 1, CreditKey: "tokens", Currency: "USD", Price: json.RawMessage(`{"model":"per_unit","per_unit":{"unit_amount":10000}}`)}},
	}))
	scale, err := svc.CreditUnitDecimals(a, old+"/tokens")
	require.NoError(t, err)
	require.Equal(t, 3, scale, "catalog updates preserve an existing unit's scale and UUID")
	var catalogUnit string
	require.NoError(t, admin.QueryRow(ctx, `SELECT unit FROM openrails.catalog_credit_balances WHERE merchant_id=$1 AND key='tokens'`, merchantA.UUID()).Scan(&catalogUnit))
	require.Equal(t, canonical, catalogUnit)
	quote, err := rt.MoneyService.QuoteCatalogCreditPurchase(a, money.CatalogCreditPurchaseQuoteInput{ProductKey: productKey, Credits: 100})
	require.NoError(t, err, "cold quote must retain the merchant connection during registry resolution")
	require.Equal(t, canonical, quote.Unit)
	payer := identity.CustomerID(uuid.New())
	key, err := NewDepositIdempotencyKey("admin", uuid.NewString())
	require.NoError(t, err)
	deposited, err := svc.DepositCredits(a, DepositCreditsRequest{CustomerID: &payer, Invoker: "test", Currency: old + "/tokens", Amount: 100, Key: key})
	require.NoError(t, err)
	require.Equal(t, old+"/tokens", deposited.Currency)
	var stored string
	require.NoError(t, admin.QueryRow(ctx, `SELECT currency FROM openrails.grants WHERE id=$1`, deposited.ID).Scan(&stored))
	require.Equal(t, canonical, stored)
	requestID := uuid.NewString()
	admitted, err := svc.Admit(a, AdmitInput{CustomerID: payer, Invoker: payer.UUID().String(), InvokerType: "payer", Currency: old + "/tokens", EstimatedAmount: 25, SourceID: requestID, ExpiresAtUnix: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	require.True(t, admitted.Allowed)
	held, err := spendgate.New(redis).HeldAmount(ctx, merchantA.String(), payer.UUID().String(), canonical)
	require.NoError(t, err)
	require.EqualValues(t, 25, held)
	// Rename while old still aliases A: both names resolve the same registry UUID.
	names[groupA] = newName
	claims[newName] = groupA
	renamed, err := svc.GetCreditAccount(a, payer, newName+"/tokens")
	require.NoError(t, err)
	require.EqualValues(t, 100, renamed.BalanceAmount)
	require.Equal(t, newName+"/tokens", renamed.Currency)
	_, err = svc.GetCreditAccount(a, payer, old+"/tokens")
	require.NoError(t, err)
	// Reclaim old for B while A's local projection is deliberately stale.
	_, err = admin.Exec(ctx, `UPDATE openrails.merchants SET slug=$2 WHERE id=$1`, merchantA.UUID(), old)
	require.NoError(t, err)
	names[groupB] = old
	claims[old] = groupB
	rt.Merchants = directory() // cold resolver/store, no alias cache authority
	_, err = svc.GetCreditAccount(a, payer, old+"/tokens")
	require.ErrorContains(t, err, "another merchant")
	account, err := svc.GetCreditAccount(a, payer, newName+"/tokens")
	require.NoError(t, err)
	require.Equal(t, newName+"/tokens", account.Currency)
	empty, err := svc.GetCreditAccount(b, payer, old+"/tokens")
	require.NoError(t, err)
	require.Zero(t, empty.BalanceAmount)
	quote, err = rt.MoneyService.QuoteCatalogCreditPurchase(a, money.CatalogCreditPurchaseQuoteInput{ProductKey: productKey, Credits: 100})
	require.NoError(t, err)
	require.Equal(t, canonical, quote.Unit, "reclaimed spelling cannot change a stored catalog unit")
	productRead, err := svc.GetProductByKey(a, productKey)
	require.NoError(t, err)
	require.Equal(t, newName+"/tokens", productRead.CreditsSpec["tokens"].Unit)
	_, err = svc.UpdateProduct(a, product.ID, UpdateProductRequest{SetCredits: true, CreditsSpec: CreditsSpec{"tokens": {Unit: old + "/tokens", Amount: 10}}})
	require.ErrorContains(t, err, "another merchant")
	_, err = svc.GetCreditAccount(b, payer, canonical)
	require.Error(t, err, "foreign registry UUID must not bypass ownership")
	// A custom unit cannot turn an excess capture into fiat debt. A refused
	// attempt preserves both the balance and reservation for a corrected retry.
	for range 2 {
		_, err = svc.CaptureHold(a, CaptureHoldRequest{RequestID: requestID, Amount: 101})
		require.ErrorIs(t, err, money.ErrInsufficientCredits)
	}
	held, err = spendgate.New(redis).HeldAmount(ctx, merchantA.String(), payer.UUID().String(), canonical)
	require.NoError(t, err)
	require.EqualValues(t, 25, held)
	account, err = svc.GetCreditAccount(a, payer, newName+"/tokens")
	require.NoError(t, err)
	require.EqualValues(t, 100, account.BalanceAmount)
	captured, err := svc.CaptureHold(a, CaptureHoldRequest{RequestID: requestID, Amount: 25, EventType: "render", Resource: "test-render"})
	require.NoError(t, err)
	require.Equal(t, newName+"/tokens", captured.Currency)
	replay, err := svc.CaptureHold(a, CaptureHoldRequest{RequestID: requestID, Amount: 25, CustomerID: payer.UUID().String(), Currency: newName + "/tokens", Invoker: payer.UUID().String()})
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	account, err = svc.GetCreditAccount(a, payer, newName+"/tokens")
	require.NoError(t, err)
	require.EqualValues(t, 75, account.BalanceAmount)
	page, err := svc.ListCreditGrants(a, payer, newName+"/tokens", 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 75, page.Grants[0].RemainingAmount)
	require.Equal(t, newName+"/tokens", page.Grants[0].Currency)
	transactions, _, err := svc.GetCustomerCreditTransactions(a, payer, newName+"/tokens", 10, 0)
	require.NoError(t, err)
	for _, tx := range transactions {
		require.Equal(t, newName+"/tokens", tx.Currency)
	}
	_, err = svc.FinalizeInvoice(a, payer, newName+"/tokens", time.Now().Add(-time.Hour), time.Now())
	require.ErrorIs(t, err, money.ErrBillingUnitRequired)
	usage, err := svc.GetUsage(a, payer, newName+"/tokens", time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.Equal(t, newName+"/tokens", usage[0].Currency)
	require.EqualValues(t, 25, usage[0].TotalAmount)
	revoked, err := svc.RevokeCreditGrant(a, payer, deposited.ID, "support revoke")
	require.NoError(t, err)
	require.EqualValues(t, 75, revoked.Grant.RevokedAmount)
	require.Equal(t, newName+"/tokens", revoked.Grant.Currency)
	// Future renewal still consumes the captured product unit UUID after rename.
	priceID, subscriptionID := uuid.New(), uuid.New()
	pspID := dbtest.EnsureTestPSP(a, t, admin, merchantA.UUID(), string(models.RailStripe))
	_, err = admin.Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,1000000,'USD',720,true)`, priceID, merchantA.UUID(), product.ID)
	require.NoError(t, err)
	periodEnd := time.Now().Add(30 * 24 * time.Hour)
	_, err = admin.Exec(ctx, `INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,psp_id,rail_subscription_id,current_period_starts_at,current_period_ends_at,credits_spec_snapshot)
 SELECT $1,$2,$3,id,$4,'active','stripe',$5,$6,now(),$7,credits_spec FROM openrails.products WHERE merchant_id=$2 AND id=$8`, subscriptionID, merchantA.UUID(), payer.UUID(), priceID, pspID, "sub_"+subscriptionID.String(), periodEnd, product.ID)
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, rt.MoneyService.GrantSubscriptionCredits(a, money.GrantSubscriptionCreditsParams{SubscriptionID: subscriptionID, PeriodEnd: periodEnd, Cadence: models.CreditGrantCadencePerRenewal, Source: "subscription_renewal"}))
	}
	account, err = svc.GetCreditAccount(a, payer, newName+"/tokens")
	require.NoError(t, err)
	require.EqualValues(t, 10, account.BalanceAmount)
	// No mutable merchant spelling survived into financial or reservation keys.
	var bad int
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE merchant_id=$1 AND currency<>$2`, merchantA.UUID(), canonical).Scan(&bad))
	require.Zero(t, bad)
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM openrails.invoice_items WHERE merchant_id=$1`, merchantA.UUID()).Scan(&bad))
	require.Zero(t, bad)
}
