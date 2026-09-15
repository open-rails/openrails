//go:build integration

package service_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/stretchr/testify/require"
)

func TestDepositTermsThroughEmbeddedAndRemoteClients(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	embeddedClient, err := h.StartEmbeddedHost("USD").Runtime().Client()
	require.NoError(t, err)
	clients := map[string]*openrails.Client{
		"embedded": embeddedClient,
		"remote":   h.StartStandalone("USD").Client(),
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			payer := openrails.CustomerID(uuid.New())
			expiry := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
			in := openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "original", Currency: "USD", Amount: 1000000,
				Source: "original", SourceID: uuid.NewString(), ExpiresAt: &expiry, Description: "Original grant"}
			first, err := client.DepositCredits(ctx, in)
			require.NoError(t, err)
			for _, field := range []string{"currency", "expiry"} {
				changed := in
				if field == "currency" {
					changed.Currency = "EUR"
				} else {
					changed.ExpiresAt = nil
				}
				_, err := client.DepositCredits(ctx, changed)
				require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
				var status *openrails.StatusError
				require.ErrorAs(t, err, &status)
				require.Equal(t, http.StatusConflict, status.Status)
			}
			in.Invoker, in.Source, in.Description = "replacement", "replacement", "Replacement grant"
			replay, err := client.DepositCredits(ctx, in)
			require.NoError(t, err)
			first.Replayed = true
			require.Equal(t, first, replay)
			read, err := client.GetDeposit(ctx, uuid.UUID(payer).String(), in.SourceID)
			require.NoError(t, err)
			require.Equal(t, first, read)
		})
	}
}
