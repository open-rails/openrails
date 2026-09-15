//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/stretchr/testify/require"
)

func TestAdmissionClientRecoveryAndCaptureReceipt(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	embeddedClient, err := h.StartEmbeddedHost("USD").Runtime().Client()
	require.NoError(t, err)
	standalone := h.StartStandalone("USD")
	otherMerchant := standalone.ProvisionOwnedMerchant("admission-other-" + uuid.NewString())
	otherClient, err := openrails.NewRemote(standalone.BaseURL, openrails.WithAPIKey(otherMerchant.APIKey), openrails.WithTimeout(30*time.Second))
	require.NoError(t, err)
	clients := map[string]*openrails.Client{"embedded": embeddedClient, "remote": standalone.Client()}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			payer := openrails.CustomerID(uuid.New())
			_, err := client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "owner", Currency: "USD", Amount: 1000, Source: "fixture", SourceID: uuid.NewString()})
			require.NoError(t, err)
			deadline := time.Now().Add(time.Hour).Unix()
			in := openrails.AdmitRequest{CustomerID: payer.String(), Invoker: "original", InvokerType: openrails.InvokerTypePayer,
				Currency: "USD", EstimatedAmount: 200, RequestID: uuid.NewString(), ExpiresAt: &deadline}
			admit := func(in openrails.AdmitRequest) openrails.AdmitBatchVerdict {
				t.Helper()
				out, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{in})
				require.NoError(t, err)
				require.Len(t, out, 1)
				return out[0]
			}
			first := admit(in)
			require.True(t, first.Allowed())
			require.Equal(t, "open", first.Result.State)
			require.False(t, first.Result.Replayed)
			_, err = otherClient.Capture(ctx, in.RequestID, 1, nil)
			require.ErrorIs(t, err, openrails.ErrNotFound, "another merchant cannot resolve original request ownership")
			require.ErrorIs(t, otherClient.Release(ctx, in.RequestID), openrails.ErrNotFound)
			require.ErrorIs(t, otherClient.ExtendHold(ctx, in.RequestID, time.Now().Add(2*time.Hour)), openrails.ErrNotFound)
			changed := in
			changed.EstimatedAmount = 2000
			conflict := admit(changed)
			require.Equal(t, http.StatusConflict, conflict.Status)
			require.Equal(t, "idempotency_key_reused", conflict.Error.Code)
			require.NoError(t, h.Redis.FlushDB(ctx).Err())
			captured, err := client.Capture(ctx, in.RequestID, 500, nil)
			require.NoError(t, err)
			require.Equal(t, payer.UUID(), captured.CustomerID)
			require.Equal(t, "USD", captured.Currency)
			require.EqualValues(t, 500, captured.Amount)
			require.NotNil(t, captured.LedgerTransferID)
			replay, err := client.Capture(ctx, in.RequestID, 500, nil)
			require.NoError(t, err)
			captured.Replayed = true
			require.Equal(t, captured, replay)
			_, err = client.Capture(ctx, in.RequestID, 501, nil)
			require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
			require.ErrorContains(t, err, "committed amount=500")
			terminal := admit(in)
			require.False(t, terminal.Allowed(), "terminal replay cannot be used to launch work")
			require.True(t, terminal.Result.Allowed, "receipt preserves the original decision")
			require.True(t, terminal.Result.Replayed)
			require.Equal(t, "captured", terminal.Result.State)
			require.ErrorIs(t, client.Release(ctx, in.RequestID), openrails.ErrConflict)
			balance, err := client.GetCreditAccount(ctx, payer.String(), "USD")
			require.NoError(t, err)
			require.EqualValues(t, 500, balance.BalanceAmount)
			require.Zero(t, balance.HeldAmount)

			in.RequestID = uuid.NewString()
			in.EstimatedAmount = 0
			in.ExpiresAt = nil
			require.True(t, admit(in).Allowed())
			free, err := client.Capture(ctx, in.RequestID, 0, nil)
			require.NoError(t, err)
			require.Nil(t, free.LedgerTransferID)
			require.Zero(t, free.Amount)
			_, err = client.Capture(ctx, uuid.NewString(), 1, nil)
			require.ErrorIs(t, err, openrails.ErrNotFound)
			in.RequestID = uuid.NewString()
			require.True(t, admit(in).Allowed())
			large := int64(9007199254740993)
			actual, err := client.Capture(ctx, in.RequestID, large, nil)
			require.NoError(t, err)
			require.Equal(t, large, actual.Amount)
			encoded, err := json.Marshal(actual)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"amount":"9007199254740993"`)
		})
	}
}
