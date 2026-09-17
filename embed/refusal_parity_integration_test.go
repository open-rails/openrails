//go:build integration

package embed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// typedRefusal is what a caller can branch on for a server-classified refusal
// (#983): status, code, param and the root class sentinels. Never the message.
type typedRefusal struct {
	Status                      int
	Code, Param                 string
	Invalid, NotFound, Conflict bool
}

func observeTypedRefusal(t *testing.T, label string, err error) typedRefusal {
	t.Helper()
	require.Error(t, err, label)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "%s: %v", label, err)
	out := typedRefusal{
		Status: status.Status, Code: status.Code,
		Invalid: errors.Is(err, openrails.ErrInvalid), NotFound: errors.Is(err, openrails.ErrNotFound), Conflict: errors.Is(err, openrails.ErrConflict),
	}
	if status.Param != nil {
		out.Param = *status.Param
	}
	return out
}

// TestTypedRefusalsAreIdenticalAcrossDeployments drives refusals the server
// used to classify by message text — a missing catalog product, cancelling a
// missing and then an already-cancelled subscription, and a deposit in an
// unsupported currency — through the embedded in-process client, an embedded
// host's HTTP mount and a standalone server over real Postgres.
func TestTypedRefusalsAreIdenticalAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	embedded, err := host.Runtime().Client()
	require.NoError(t, err)

	mid := dbtest.TestMerchantID.UUID()
	customer, product, price, psp, card, cancelled := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		_, err := h.Pool().Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			`DELETE FROM openrails.subscriptions WHERE merchant_id=$1 AND psp_id=$2`,
			`DELETE FROM openrails.payment_methods WHERE merchant_id=$1 AND psp_id=$2`,
			`DELETE FROM openrails.psps WHERE merchant_id=$1 AND id=$2`,
		} {
			_, err := h.Pool().Exec(context.Background(), sql, mid, psp)
			require.NoError(t, err)
		}
	})
	exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
	exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Refusal parity plan')`, product, mid, product.String())
	exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,$4,1000000,'USD',true,720)`, price, mid, product, price.String())
	exec(`INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3)`, psp, mid, psp.String())
	exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi','refusal-parity','4242','visa')`, card, mid, customer, psp)
	exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type) VALUES($1,$2,$3,$4,$5,$6,'nmi','cancelled',$7,$8,$9,$10,$9,'merchant')`, cancelled, mid, customer, product, price, psp, cancelled.String(), card, now.Add(-48*time.Hour), now.Add(-time.Hour))

	script := func(client *openrails.Client) map[string]typedRefusal {
		out := map[string]typedRefusal{}
		_, err := client.GetProduct(ctx, openrails.ProductID(uuid.New()))
		out["product_not_found"] = observeTypedRefusal(t, "product not found", err)
		err = client.CancelSubscription(ctx, openrails.SubscriptionID(uuid.New()), openrails.CancelSubscriptionRequest{Reason: "parity"})
		out["subscription_not_found"] = observeTypedRefusal(t, "subscription not found", err)
		err = client.CancelSubscription(ctx, openrails.SubscriptionID(cancelled), openrails.CancelSubscriptionRequest{Reason: "parity"})
		out["subscription_not_active"] = observeTypedRefusal(t, "subscription not active", err)
		payer := openrails.CustomerID(customer)
		_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "parity", Currency: "XXX", Amount: 1, Source: "parity", SourceID: uuid.NewString()})
		out["currency_unsupported"] = observeTypedRefusal(t, "currency unsupported", err)
		return out
	}

	want := map[string]typedRefusal{
		"product_not_found":       {Status: 404, Code: "product_not_found", NotFound: true},
		"subscription_not_found":  {Status: 404, Code: "subscription_not_found", NotFound: true},
		"subscription_not_active": {Status: 409, Code: "subscription_not_active", Conflict: true},
		"currency_unsupported":    {Status: 400, Code: "currency_unsupported", Param: "currency", Invalid: true},
	}
	for name, client := range map[string]*openrails.Client{"embedded": embedded, "hosted_http": host.Client(), "standalone": standalone.Client()} {
		require.Equal(t, want, script(client), "%s refusal contract diverged", name)
	}
}
