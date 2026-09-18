//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestOneSubjectHasIndependentMerchantBillingThroughBothClients(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	server := h.StartStandalone("USD")
	other := server.ProvisionOwnedMerchant("sharedpayer" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
	subject := uuid.New()
	payer := openrails.CustomerID(subject)
	sourceID := uuid.NewString()
	secondSourceID := uuid.NewString()
	var firstReceipts []uuid.UUID
	type observation struct {
		clients   map[string]*openrails.Client
		amount    int64
		feature   string
		receiptID uuid.UUID
	}
	var observations []observation

	for _, side := range []struct {
		merchant merchant.ID
		slug     string
		amount   int64
		feature  string
	}{
		{dbtest.TestMerchantID, dbtest.TestMerchantSlug, 1_000_000, "access-a"},
		{other.MerchantID, other.MerchantSlug, 2_000_000, "access-b"},
	} {
		t.Run(side.feature, func(t *testing.T) {
			token := server.MintAPIKey(side.slug, "shared-payer-"+uuid.NewString(), []string{
				controlplane.PermMerchantCustomerSettingsRead,
				controlplane.PermMerchantCustomerSettingsUpdate,
			})
			remote := server.Client(openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
			status, body := requestJSON(t, http.MethodPost, server.BaseURL+"/v1/merchant/customers/entitlements:batch", token,
				map[string]any{"subjects": []string{"not-a-uuid"}})
			require.Equal(t, http.StatusBadRequest, status, string(body))
			host := h.StartEmbeddedMerchant("USD", side.merchant, side.slug)
			local, err := host.Runtime().Client(openrails.WithCurrency("USD"))
			require.NoError(t, err)
			request := openrails.DepositCreditsRequest{
				CustomerID: &payer, Invoker: subject.String(), Currency: "USD", Amount: side.amount,
				Source: "shared-subject", SourceID: sourceID,
			}
			// The exact external subject and caller key can independently fund
			// both merchants. No customer row is preseeded for these writes.
			first, err := remote.DepositCredits(ctx, request)
			require.NoError(t, err)
			require.False(t, first.Replayed)
			require.Equal(t, openrails.CustomerID(subject), first.CustomerID)
			firstReceipts = append(firstReceipts, first.ID)
			replay, err := local.DepositCredits(ctx, request)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			require.Equal(t, first.ID, replay.ID)
			request.SourceID = secondSourceID
			_, err = local.DepositCredits(ctx, request)
			require.NoError(t, err)

			// Host-provisioned access facts exercise the subject batch reader;
			// the same UUID must resolve only this merchant's entitlement.
			_, err = h.MerchantPool(side.merchant.UUID()).Exec(ctx, `
				INSERT INTO openrails.entitlements
				(id, merchant_id, customer_id, entitlement, start_at, source_id, source_type)
				VALUES ($1, $2, $3, $4, now() - interval '1 hour', $5, 'admin')`,
				uuid.New(), side.merchant.UUID(), subject, side.feature, uuid.New())
			require.NoError(t, err)
			observations = append(observations, observation{
				clients: map[string]*openrails.Client{"remote": remote, "embedded": local},
				amount:  side.amount, feature: side.feature, receiptID: first.ID,
			})
		})
	}
	// Read only after both merchants have written, so leakage in either direction
	// and overwriting an earlier merchant's balance are observable.
	for _, side := range observations {
		for name, client := range side.clients {
			t.Run(side.feature+"_"+name, func(t *testing.T) {
				balance, err := client.Balance(ctx, openrails.CustomerID(subject))
				require.NoError(t, err)
				require.Equal(t, 2*side.amount, balance.BalanceAmount)
				receipt, err := client.GetDeposit(ctx, openrails.CustomerID(subject), sourceID)
				require.NoError(t, err)
				require.Equal(t, side.receiptID, receipt.ID)
				require.Equal(t, side.amount, receipt.Amount)
				allowed, err := client.HasEntitlement(ctx, openrails.CustomerID(subject), side.feature, time.Time{})
				require.NoError(t, err)
				require.True(t, allowed)
				unknown := openrails.CustomerID(uuid.New())
				records, err := client.ListActiveEntitlements(ctx, []openrails.CustomerID{openrails.CustomerID(subject), unknown}, time.Time{})
				require.NoError(t, err)
				require.Len(t, records[openrails.CustomerID(subject)], 1)
				require.Empty(t, records[unknown])
				_, err = client.ListActiveEntitlements(ctx, []openrails.CustomerID{{}}, time.Time{})
				require.Error(t, err)
				foreignFeature := "access-a"
				if side.feature == foreignFeature {
					foreignFeature = "access-b"
				}
				allowed, err = client.HasEntitlement(ctx, openrails.CustomerID(subject), foreignFeature, time.Time{})
				require.NoError(t, err)
				require.False(t, allowed)
			})
		}
	}
	require.Len(t, firstReceipts, 2)
	require.NotEqual(t, firstReceipts[0], firstReceipts[1])
}
