//go:build integration

package embed_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

type catalogFailingProvider struct {
	calls  atomic.Int32
	onCall func()
}

func (p *catalogFailingProvider) RoundTrip(*http.Request) (*http.Response, error) {
	p.calls.Add(1)
	if p.onCall != nil {
		p.onCall()
	}
	return nil, errors.New("fixture provider unavailable")
}

func TestCatalogPriceKeyTransactions(t *testing.T) {
	ctx := t.Context()
	owner, pool, dsn := scopeWithoutRLSDatabase(t)
	require.Greater(t, pool.Config().MaxConns, int32(1), "concurrent transactions use independent connections")
	provider := &catalogFailingProvider{}
	rt, mid, err := newDeclaredMerchant(ctx, embed.Options{Config: &config.Config{
		TestMode:             config.CredentialPostureSandbox,
		MerchantConfigSource: config.MerchantConfigSourceManifest, AllowCatalogUpdates: true,
		ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: dsn},
	}, PGXPool: pool, River: embed.RiverManagedByOpenRails(), StripeTransport: provider}, "price-tx-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Price transaction", PSPs: map[string]embed.PSPConfig{"stripe": {"stripe": {AccountID: "acct_transaction_fixture", Secrets: map[string]string{"secret_key": "sk_test_transaction_fixture", "webhook_signing_secret": "whsec_transaction_fixture"}}}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	local, err := rt.Client()
	require.NoError(t, err)
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true}, Gate: creatorAdminTestGate{mid: mid}})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey("administrator"), openrails.WithMerchantID(mid))
	require.NoError(t, err)
	_, err = owner.Exec(ctx, `CREATE FUNCTION billing.reject_price_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.amount=4000000 THEN RAISE EXCEPTION 'fixture price insert rejected'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_price_fixture BEFORE INSERT ON billing.prices FOR EACH ROW EXECUTE FUNCTION billing.reject_price_fixture();`)
	require.NoError(t, err)
	for _, transport := range []struct {
		name   string
		client *openrails.Client
	}{{"embedded", local}, {"remote", remote}} {
		t.Run(transport.name, func(t *testing.T) {
			client := transport.client
			tier := "platform-" + transport.name
			definition := &openrails.ProductCreateParams{Key: tier, DisplayName: "First platform title", TierGroup: &tier, TierRank: 2, EntitlementsSpec: map[string]*int{"premium": nil}}
			products := make([]*openrails.Product, 12)
			var group errgroup.Group
			for i := range products {
				group.Go(func() error { var err error; products[i], err = client.Products.Ensure(ctx, definition); return err })
			}
			require.NoError(t, group.Wait())
			product := products[0]
			for _, p := range products {
				require.Equal(t, product.ID, p.ID, "concurrent initial declarations converge")
			}
			definition.DisplayName = "Do not silently replace title"
			definition.TierRank = 9
			same, err := client.Products.Ensure(ctx, definition)
			require.NoError(t, err)
			require.Equal(t, product.ID, same.ID)
			require.Equal(t, "First platform title", same.DisplayName)
			require.Equal(t, 2, same.TierRank)
			require.Contains(t, same.EntitlementsSpec, "premium")
			request := openrails.PriceCreateParams{ProductID: product.ID, Key: tier + "-usd", UnitAmount: 1000000, Currency: "USD", PSPs: []string{"stripe"}}
			creates := make([]*openrails.Price, 12)
			for i := range creates {
				group.Go(func() error { var err error; creates[i], err = client.Prices.Create(ctx, &request); return err })
			}
			require.NoError(t, group.Wait())
			for _, p := range creates {
				require.Equal(t, creates[0].ID, p.ID)
			}
			// Inline natural-key creation and ProductID creation share lock order.
			// Their overlapping declarations must converge on the same price.
			for i := range creates {
				group.Go(func() error {
					mixed := request
					if i%2 == 0 {
						mixed.ProductID = ""
						mixed.ProductData = &openrails.PriceCreateProductDataParams{Key: definition.Key, DisplayName: "Preserve first title"}
					}
					var err error
					creates[i], err = client.Prices.Create(ctx, &mixed)
					return err
				})
			}
			require.NoError(t, group.Wait())
			for _, p := range creates {
				require.Equal(t, creates[0].ID, p.ID)
			}
			// The stable key is a version pointer for ProductID creation. Concurrent
			// revisions serialize; each has an immutable row and exactly one is current.
			versions := make([]*openrails.Price, 2)
			for i := range versions {
				group.Go(func() error {
					changed := request
					changed.UnitAmount = int64(i+2) * 1000000
					var err error
					versions[i], err = client.Prices.Create(ctx, &changed)
					return err
				})
			}
			require.NoError(t, group.Wait())
			require.NotEqual(t, versions[0].ID, versions[1].ID)
			current, err := client.Prices.RetrieveByKey(ctx, request.Key)
			require.NoError(t, err)
			require.Contains(t, []string{versions[0].ID, versions[1].ID}, current.ID)
			previous, err := client.Prices.Retrieve(ctx, creates[0].ID)
			require.NoError(t, err)
			require.True(t, previous.Archived)
			prices, err := client.Prices.List(ctx, &openrails.PriceListParams{ProductID: product.ID})
			require.NoError(t, err)
			require.Len(t, prices.Items, 3)
			var live, movements int
			for _, p := range prices.Items {
				if !p.Archived {
					live++
				}
			}
			require.Equal(t, 1, live)
			require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM billing.price_key_movements WHERE merchant_id=$1 AND key=$2`, mid.UUID(), request.Key).Scan(&movements))
			require.Equal(t, 5, movements, "three price activations and two retirements are retained; identical replays add nothing")
			var retirements int
			require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM billing.price_key_movements WHERE merchant_id=$1 AND key=$2 AND archived`, mid.UUID(), request.Key).Scan(&retirements))
			require.Equal(t, 2, retirements, "each superseded version records its retirement")
			// INSERT fails after the helper has archived the previous current row.
			// The transaction must restore the original pointer and movement history.
			broken := request
			broken.UnitAmount = 4000000
			_, err = client.Prices.Create(ctx, &broken)
			require.Error(t, err)
			after, err := client.Prices.RetrieveByKey(ctx, request.Key)
			require.NoError(t, err)
			require.Equal(t, current.ID, after.ID)
			require.False(t, after.Archived)
			var movementsAfter int
			require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM billing.price_key_movements WHERE merchant_id=$1 AND key=$2`, mid.UUID(), request.Key).Scan(&movementsAfter))
			require.Equal(t, movements, movementsAfter, "failed writes roll back movement history")
			prices, err = client.Prices.List(ctx, &openrails.PriceListParams{ProductID: product.ID})
			require.NoError(t, err)
			require.Len(t, prices.Items, 3)
			other, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: tier + "-other", DisplayName: "Another product"})
			require.NoError(t, err)
			hijack := request
			hijack.ProductID = other.ID
			for range 6 {
				group.Go(func() error {
					_, err := client.Prices.Create(ctx, &hijack)
					if !errors.Is(err, openrails.ErrConflict) {
						return fmt.Errorf("cross-product key claim: expected conflict, got %v", err)
					}
					return nil
				})
				group.Go(func() error {
					replay := request
					replay.UnitAmount = current.UnitAmount
					_, err := client.Prices.Create(ctx, &replay)
					return err
				})
			}
			require.NoError(t, group.Wait(), "cross-product key contention must refuse transfer without deadlocking")
			require.Zero(t, provider.calls.Load(), "engine catalog creation never contacts Stripe")
		})
	}
	// Explicit legacy attachment retains provider resolution outside the local
	// transaction. A failed provider read leaves the current key untouched.
	product, err := local.Products.RetrieveByKey(ctx, "platform-embedded")
	require.NoError(t, err)
	current, err := local.Prices.RetrieveByKey(ctx, "platform-embedded-usd")
	require.NoError(t, err)
	provider.onCall = func() {
		var activeTx int
		require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction'`, pool.Config().ConnConfig.User).Scan(&activeTx))
		require.Zero(t, activeTx, "provider network call must precede the price transaction")
	}
	_, err = local.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: current.Key, UnitAmount: 5000000, Currency: "USD", PSPLinks: map[string]map[string]string{"stripe": {"price_id": "price_unavailable"}}})
	require.Error(t, err)
	require.Positive(t, provider.calls.Load())
	after, err := local.Prices.RetrieveByKey(ctx, current.Key)
	require.NoError(t, err)
	require.Equal(t, current.ID, after.ID)
	require.False(t, after.Archived)
}
