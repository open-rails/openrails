package openrails

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestOperationIdempotencyKeysHaveOneValue(t *testing.T) {
	for name, call := range map[string]func(*Client, string, ...RequestOption) error{
		"order": func(c *Client, key string, options ...RequestOption) error {
			_, err := c.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.New()), IdempotencyKey: key}, options...)
			return err
		},
		"refund": func(c *Client, key string, options ...RequestOption) error {
			_, err := c.RefundPayment(t.Context(), billing.PaymentID(uuid.New()), billing.RefundPaymentParams{Full: true, IdempotencyKey: key}, options...)
			return err
		},
		"subscription change": func(c *Client, key string, options ...RequestOption) error {
			_, err := c.ChangeSubscription(t.Context(), billing.SubscriptionID(uuid.New()), billing.ChangeSubscriptionParams{Quantity: new(1), IdempotencyKey: key}, options...)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, []string{"purchase-1"}, r.Header.Values("Idempotency-Key"))
				_, _ = w.Write([]byte(`{}`))
			})
			require.NoError(t, call(client, "", WithIdempotencyKey("purchase-1")))
			require.NoError(t, call(client, "purchase-1"))
			require.NoError(t, call(client, "purchase-1", WithIdempotencyKey("purchase-1")))
			require.ErrorIs(t, call(client, "purchase-1", WithIdempotencyKey("purchase-2")), billing.ErrInvalid)
			require.Equal(t, 3, calls, "conflicting keys never send a request")
		})
	}
}

func TestNaturallyIdentifiedWriteDoesNotInventHTTPKey(t *testing.T) {
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Values("Idempotency-Key"))
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	_, err := client.Admit(context.Background(), []billing.AdmitParams{{RequestID: "own-identity"}})
	require.NoError(t, err)
}
