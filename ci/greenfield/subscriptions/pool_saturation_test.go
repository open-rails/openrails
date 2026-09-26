//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// #1105: a request holds one pool connection and never waits for a second,
// so checkout traffic well past the pool's size completes instead of
// deadlocking: each request reads the checkout options (custodians and PSPs),
// then creates and pays its session.
func TestCheckoutBeyondPoolSizeCompletes(t *testing.T) {
	t.Parallel()
	const poolSize, requests = 6, 20
	w := prepareWorld(t, poolSize)
	w.start()
	client := w.client[embedded]
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", EntitlementsSpec: map[string]*int{"content:post": nil}})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	customers := make([]*customer, requests)
	for i := range customers {
		customers[i] = w.newCustomer()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	start := make(chan struct{})
	errs := make([]error, requests)
	var wg sync.WaitGroup
	for i, c := range customers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 2 {
				if _, err := client.ListCheckoutRailOptions(ctx, price.ID); err != nil {
					errs[i] = err
					return
				}
			}
			session, err := client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
				Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, PriceID: price.ID, IdempotencyKey: "checkout:" + uuid.NewString() + ":1", Confirm: true,
				PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["nmi"], Rail: "nmi", PaymentToken: w.nmi.Tokenize(visa), NameOnCard: "Pool Payer", Zip: "10001", Country: "US"},
			})
			if err == nil && session.Status != "succeeded" {
				err = errUnexpected(session.Status)
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	require.NoError(t, ctx.Err(), "every request finished before the deadline")
	for i, err := range errs {
		require.NoError(t, err, "request %d", i)
	}
	w.settle()
	require.Len(t, w.nmi.Ledger(""), requests, "one charge per buyer")
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected session status " + string(e) }
