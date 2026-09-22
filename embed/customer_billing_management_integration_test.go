//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestCustomerBillingManagementOwnHistoryAndPagination(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	runtime, mid, err := newDeclaredMerchant(ctx, embed.Options{
		Config: &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
			ProviderWriteMode: config.ProviderWriteModeReadOnly, NewSubscriptionCollectionPolicy: "engine", DB: &config.DBConfig{URL: dsn}},
		PGXPool: pool, River: embed.RiverFromHost(),
	}, "customer-management-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Customer management"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	alice, bob, foreignInvoice, foreignSubscription := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	authn := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		var subject uuid.UUID
		switch r.Header.Get("Authorization") {
		case "Bearer alice":
			subject = alice
		case "Bearer bob":
			subject = bob
		default:
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: mid.String(), SubjectID: subject.String(), CredentialClass: billingauth.CredentialClassUserSession}, nil
	})
	require.NoError(t, runtime.ConfigureHTTP(embed.HTTPConfig{CustomerExposures: []embed.CustomerHTTPConfig{{
		Prefix: "/v1/me", Scope: embed.CustomerBillingManagement, DelegatedAuthenticator: authn,
	}}}))
	routes, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, routes.Mount(mux, "/billing"))
	exec := func(query string, args ...any) { _, err := pool.Exec(ctx, query, args...); require.NoError(t, err) }
	exec(`INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2),($3,$2)`, alice, mid.UUID(), bob)
	prefix := "purchased-" + uuid.NewString()
	exec(`INSERT INTO billing.products(id,merchant_id,key,display_name)
 SELECT gen_random_uuid(),$1,$2||i::text,'Purchased item' FROM generate_series(1,106) AS i`, mid.UUID(), prefix)
	exec(`INSERT INTO billing.grants(merchant_id,customer_id,product_id,kind,source_type,source_id,event,starts_at)
 SELECT merchant_id,CASE WHEN key=$3 THEN $4::uuid ELSE $2::uuid END,id,'ownership','admin',id::text,'grant',now()-interval '1 hour'
 FROM billing.products WHERE merchant_id=$1 AND key LIKE $5`, mid.UUID(), alice, prefix+"106", bob, prefix+"%")
	var product uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM billing.products WHERE merchant_id=$1 AND key=$2`, mid.UUID(), prefix+"1").Scan(&product))
	price := uuid.New()
	exec(`INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency) VALUES($1,$2,$3,1000000,'USD')`, price, mid.UUID(), product)
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid.UUID(), "nmi")
	for i, customer := range []uuid.UUID{alice, alice, bob} {
		exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at)
 VALUES($1,$2,$3,$4,'nmi',$5,1000000,1000000,'USD','completed','rail',$6,$7)`, uuid.New(), mid.UUID(), customer, price, uuid.NewString(), psp, time.Now().Add(time.Duration(i)*time.Second))
	}
	exec(`INSERT INTO billing.invoices(id,merchant_id,customer_id,currency,period_from,period_to) VALUES($1,$2,$3,'USD',now()-interval '1 day',now())`, foreignInvoice, mid.UUID(), alice)
	exec(`INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id)
 VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7)`, foreignSubscription, mid.UUID(), alice, product, price, psp, "existing-"+foreignSubscription.String())
	request := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/billing/v1/me"+path, strings.NewReader(`{"feedback":"Owner isolation test","customer_id":"`+alice.String()+`"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-User-ID", alice.String())
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		response := request(http.MethodGet, "/products?limit=23&cursor="+cursor+"&customer_id="+bob.String(), "alice")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var page openrails.ProductAccessList
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
		require.LessOrEqual(t, len(page.Data), 23)
		for _, grant := range page.Data {
			require.False(t, seen[grant.ProductID], "cursor pages must not repeat a product")
			seen[grant.ProductID] = true
		}
		if !page.HasMore {
			break
		}
		require.NotEmpty(t, page.NextCursor)
		cursor = page.NextCursor
	}
	require.Len(t, seen, 105)
	bobPage := request(http.MethodGet, "/products?customer_id="+alice.String(), "bob")
	require.Equal(t, http.StatusOK, bobPage.Code, bobPage.Body.String())
	var products openrails.ProductAccessList
	require.NoError(t, json.Unmarshal(bobPage.Body.Bytes(), &products))
	require.Len(t, products.Data, 1)
	require.False(t, seen[products.Data[0].ProductID])
	for _, limit := range []int{0, 101} {
		require.Equal(t, http.StatusBadRequest, request(http.MethodGet, "/products?limit="+strconv.Itoa(limit), "alice").Code)
	}
	for _, path := range []string{"/invoices/" + foreignInvoice.String(), "/subscriptions/" + openrails.SubscriptionID(foreignSubscription).String()} {
		response := request(http.MethodGet, path, "bob")
		require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	}
	response := request(http.MethodPost, "/subscriptions/"+openrails.SubscriptionID(foreignSubscription).String()+"/cancel", "bob")
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	for _, token := range []string{"alice", "bob"} {
		response := request(http.MethodGet, "/payments?limit=1&customer_id="+alice.String(), token)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var payments struct {
			Data  []json.RawMessage `json:"data"`
			Total int               `json:"total"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payments))
		require.Len(t, payments.Data, 1)
		want := 2
		if token == "bob" {
			want = 1
		}
		require.Equal(t, want, payments.Total)
		require.Contains(t, string(payments.Data[0]), "created_at")
		require.Contains(t, string(payments.Data[0]), "amount")
	}
}
