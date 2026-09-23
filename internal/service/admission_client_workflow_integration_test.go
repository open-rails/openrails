//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	otherClient, err := openrails.NewRemote(standalone.BaseURL, openrails.WithAPIKey(otherMerchant.APIKey), openrails.WithMerchantID(otherMerchant.MerchantID), openrails.WithTimeout(30*time.Second))
	require.NoError(t, err)
	remoteMerchant := standalone.ProvisionOwnedMerchant("admission-remote-" + uuid.NewString())
	remoteClient, err := openrails.NewRemote(standalone.BaseURL, openrails.WithAPIKey(remoteMerchant.APIKey), openrails.WithMerchantID(remoteMerchant.MerchantID), openrails.WithTimeout(30*time.Second))
	require.NoError(t, err)
	clients := map[string]*openrails.Client{"embedded": embeddedClient, "remote": remoteClient}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			payer := openrails.CustomerID(uuid.New())
			_, err := client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: new(payer.String()), Invoker: "owner", Currency: "USD", Amount: 1000, Source: "fixture", SourceID: uuid.NewString()})
			require.NoError(t, err)
			deadline := time.Now().Add(time.Hour)
			in := openrails.AdmitRequest{CustomerID: (openrails.CustomerID(payer)).String(), Invoker: "original", InvokerType: openrails.InvokerTypePayer,
				Currency: "USD", EstimatedAmount: 200, RequestID: ".", ExpiresAt: &deadline}
			admit := func(in openrails.AdmitRequest) openrails.AdmitBatchVerdict {
				t.Helper()
				out, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{in})
				require.NoError(t, err)
				require.Len(t, out, 1)
				return out[0]
			}
			for _, invalid := range []struct {
				field  string
				change func(*openrails.AdmitRequest)
			}{
				{"request_id", func(v *openrails.AdmitRequest) { v.RequestID = "" }},
				{"request_id", func(v *openrails.AdmitRequest) { v.RequestID = strings.Repeat("é", 128) }},
				{"accrual_rate_delta_per_hour", func(v *openrails.AdmitRequest) { v.AccrualRateDeltaPerHour = -1 }},
			} {
				bad := in
				invalid.change(&bad)
				verdict := admit(bad)
				require.Equal(t, http.StatusBadRequest, verdict.Status)
				require.NotNil(t, verdict.Error)
				require.NotNil(t, verdict.Error.Param)
				require.Equal(t, invalid.field, *verdict.Error.Param)
				require.NotEmpty(t, verdict.Error.Code)
				require.NotEmpty(t, verdict.Error.RequestID)
			}
			first := admit(in)
			require.True(t, first.Allowed())
			require.Equal(t, "open", first.Result.State)
			require.False(t, first.Result.Replayed)
			require.NoError(t, client.ExtendHold(ctx, in.RequestID, time.Now().Add(2*time.Hour)))
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
			usage := openrails.CaptureUsage{EventType: "inference", Resource: "model", Source: "platform", SourceID: in.RequestID,
				Dimensions: map[string]int64{"tokens": 7}, Metadata: map[string]any{"availability_tier": "paid"}}
			captured, err := client.Capture(ctx, in.RequestID, 500, &usage)
			require.NoError(t, err)
			require.Equal(t, payer.String(), captured.CustomerID)
			require.Equal(t, "USD", captured.Currency)
			require.EqualValues(t, 500, captured.Amount)
			require.NotNil(t, captured.LedgerTransferID)
			replay, err := client.Capture(ctx, in.RequestID, 500, &usage)
			require.NoError(t, err)
			captured.Replayed = true
			require.Equal(t, captured, replay)
			_, err = client.Capture(ctx, in.RequestID, 501, &usage)
			require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
			require.ErrorContains(t, err, "committed amount=500")
			for _, change := range []func(*openrails.CaptureUsage){
				func(u *openrails.CaptureUsage) { u.EventType = "other" },
				func(u *openrails.CaptureUsage) { u.Resource = "other" },
				func(u *openrails.CaptureUsage) { u.Source = "other" },
				func(u *openrails.CaptureUsage) { u.SourceID = "other" },
				func(u *openrails.CaptureUsage) { u.Dimensions = map[string]int64{"tokens": 8} },
				func(u *openrails.CaptureUsage) { u.Metadata = map[string]any{"availability_tier": "free"} },
			} {
				changed := usage
				change(&changed)
				_, err := client.Capture(ctx, in.RequestID, 500, &changed)
				require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
			}
			_, err = client.Capture(ctx, in.RequestID, 500, nil)
			require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
			rollup, err := client.UsageRollup(ctx, (openrails.CustomerID(payer)).String(), "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "invoker")
			require.NoError(t, err)
			require.Len(t, rollup, 1)
			require.Equal(t, "original", rollup[0].Key)
			require.EqualValues(t, 500, rollup[0].TotalAmount)
			require.EqualValues(t, 1, rollup[0].EventCount)
			terminal := admit(in)
			require.False(t, terminal.Allowed(), "terminal replay cannot be used to launch work")
			require.True(t, terminal.Result.Allowed, "receipt preserves the original decision")
			require.True(t, terminal.Result.Replayed)
			require.Equal(t, "captured", terminal.Result.State)
			require.ErrorIs(t, client.Release(ctx, in.RequestID), openrails.ErrConflict)
			balance, err := client.GetCreditAccount(ctx, (openrails.CustomerID(payer)).String(), "USD")
			require.NoError(t, err)
			require.EqualValues(t, 500, balance.BalanceAmount)
			require.Zero(t, balance.HeldAmount)

			// A different request cannot claim an existing usage event. The unique
			// insert fails after the ledger write, proving the entire capture rolls back.
			in.RequestID = ".."
			in.EstimatedAmount = 100
			require.True(t, admit(in).Allowed())
			_, err = client.Capture(ctx, in.RequestID, 50, &usage)
			require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
			balance, err = client.GetCreditAccount(ctx, (openrails.CustomerID(payer)).String(), "USD")
			require.NoError(t, err)
			require.EqualValues(t, 500, balance.BalanceAmount)
			require.EqualValues(t, 100, balance.HeldAmount)
			require.True(t, admit(in).Allowed(), "failed usage insert leaves the original operation open")
			require.NoError(t, client.ExtendHold(ctx, in.RequestID, time.Now().Add(2*time.Hour)))
			require.NoError(t, client.Release(ctx, in.RequestID))
			usage.SourceID = in.RequestID
			settled, err := client.Capture(ctx, in.RequestID, 50, &usage)
			require.NoError(t, err)
			require.False(t, settled.Replayed)

			in.RequestID = "path/%?#雪"
			in.EstimatedAmount = 0
			in.ExpiresAt = nil
			require.True(t, admit(in).Allowed())
			freeUsage := openrails.CaptureUsage{EventType: "free", SourceID: in.RequestID}
			free, err := client.Capture(ctx, in.RequestID, 0, &freeUsage)
			require.NoError(t, err)
			require.Nil(t, free.LedgerTransferID)
			require.Zero(t, free.Amount)
			freeReplay, err := client.Capture(ctx, in.RequestID, 0, &freeUsage)
			require.NoError(t, err)
			free.Replayed = true
			require.Equal(t, free, freeReplay)
			rollup, err = client.UsageRollup(ctx, (openrails.CustomerID(payer)).String(), "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "invoker")
			require.NoError(t, err)
			require.Len(t, rollup, 1)
			require.EqualValues(t, 3, rollup[0].EventCount, "free completion records usage without a fake transfer")
			require.EqualValues(t, 550, rollup[0].TotalAmount)
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
