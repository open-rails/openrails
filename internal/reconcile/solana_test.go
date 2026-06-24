package reconcile

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// fakeSolanaRPC serves canned account data and signature listings.
type fakeSolanaRPC struct {
	accounts   map[string][]byte
	signatures map[string][]solanaint.SignatureInfo
}

func (f *fakeSolanaRPC) GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error) {
	return f.accounts[address.String()], nil
}

func (f *fakeSolanaRPC) GetSignaturesForAddress(ctx context.Context, address string, limit int) ([]solanaint.SignatureInfo, error) {
	return f.signatures[address], nil
}

// buildPlanBlob assembles a synthetic Plan account in the on-chain layout
// (discriminator 1; fixed-size little-endian fields; 491 bytes total).
func buildPlanBlob(t *testing.T, amount, periodHours uint64, endTs int64) []byte {
	t.Helper()
	blob := make([]byte, 0, subscriptions.PlanAccountSize)
	blob = append(blob, 1)                           // discriminator
	blob = append(blob, make([]byte, 32)...)         // owner
	blob = append(blob, 0xFE)                        // bump
	blob = append(blob, 1)                           // status
	blob = binary.LittleEndian.AppendUint64(blob, 7) // planId
	blob = append(blob, make([]byte, 32)...)         // mint
	blob = binary.LittleEndian.AppendUint64(blob, amount)
	blob = binary.LittleEndian.AppendUint64(blob, periodHours)
	blob = binary.LittleEndian.AppendUint64(blob, uint64(1700000000)) // createdAt
	blob = binary.LittleEndian.AppendUint64(blob, uint64(endTs))
	blob = append(blob, make([]byte, 4*32)...) // destinations
	blob = append(blob, make([]byte, 4*32)...) // pullers
	blob = append(blob, make([]byte, 128)...)  // metadataUri
	require.Len(t, blob, subscriptions.PlanAccountSize)
	return blob
}

func TestSolanaFetcher_Fetch(t *testing.T) {
	t.Parallel()

	openSub := solanago.NewWallet().PublicKey()
	closedSub := solanago.NewWallet().PublicKey()
	planPDA := solanago.NewWallet().PublicKey()
	wallet := solanago.NewWallet().PublicKey()

	blockTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{
			openSub.String(): {0x02, 0x01, 0x02}, // subscription account exists (opaque bytes)
			planPDA.String(): buildPlanBlob(t, 5_000_000, 720, 0),
			// closedSub absent => account closed
		},
		signatures: map[string][]solanaint.SignatureInfo{
			openSub.String(): {
				{Signature: "sigOK", HasError: false, BlockTime: &blockTime},
				{Signature: "sigFail", HasError: true, BlockTime: &blockTime},
			},
		},
	}

	source := func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
		return []SolanaSubscriptionRef{
			{SubscriptionPDA: openSub.String(), PlanPDA: planPDA.String(), SubscriberWallet: wallet.String()},
			{SubscriptionPDA: closedSub.String(), PlanPDA: planPDA.String(), SubscriberWallet: wallet.String()},
		}, nil
	}

	fetcher := &SolanaFetcher{RPC: rpc, Source: source}
	snap, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)

	require.Equal(t, ProviderSolana, snap.Provider)
	require.True(t, snap.Capabilities.Subscriptions)
	require.True(t, snap.Capabilities.Transactions)
	require.False(t, snap.Capabilities.Refunds)
	require.False(t, snap.Capabilities.Vault)

	require.Len(t, snap.Subscriptions, 2)
	open := snap.Subscriptions[0]
	require.Equal(t, openSub.String(), open.RailSubscriptionID)
	require.Equal(t, SubscriptionStatusActive, open.Status)
	require.Equal(t, "account_open", open.RawStatus)
	require.Equal(t, wallet.String(), open.CustomerID)
	require.Equal(t, planPDA.String(), open.PlanID)
	require.Zero(t, open.AmountCents) // base units live in Raw, not cents
	require.Contains(t, string(open.Raw), `"amount":5000000`)
	require.Contains(t, string(open.Raw), `"period_hours":720`)

	closed := snap.Subscriptions[1]
	require.Equal(t, SubscriptionStatusCancelled, closed.Status)
	require.Equal(t, "account_closed", closed.RawStatus)

	// Signature listing for the open subscription only (closed has none).
	require.Len(t, snap.Transactions, 2)
	require.Equal(t, "sigOK", snap.Transactions[0].TransactionID)
	require.Equal(t, TransactionTypeSale, snap.Transactions[0].Type)
	require.True(t, snap.Transactions[0].Success)
	require.Equal(t, blockTime, snap.Transactions[0].OccurredAt)
	require.Equal(t, openSub.String(), snap.Transactions[0].SubscriptionID)

	require.Equal(t, TransactionTypeDecline, snap.Transactions[1].Type)
	require.False(t, snap.Transactions[1].Success)
}

func TestSolanaFetcher_PlanEndedMarksExpired(t *testing.T) {
	t.Parallel()

	sub := solanago.NewWallet().PublicKey()
	planPDA := solanago.NewWallet().PublicKey()

	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{
			sub.String():     {0x02},
			planPDA.String(): buildPlanBlob(t, 1, 720, time.Now().Add(-time.Hour).Unix()),
		},
	}
	source := func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
		return []SolanaSubscriptionRef{{SubscriptionPDA: sub.String(), PlanPDA: planPDA.String()}}, nil
	}

	snap, err := (&SolanaFetcher{RPC: rpc, Source: source}).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 1)
	require.Equal(t, SubscriptionStatusExpired, snap.Subscriptions[0].Status)
	require.Equal(t, "plan_ended", snap.Subscriptions[0].RawStatus)
}

func TestSolanaFetcher_SubscriptionFilter(t *testing.T) {
	t.Parallel()

	keep := solanago.NewWallet().PublicKey()
	skip := solanago.NewWallet().PublicKey()

	rpc := &fakeSolanaRPC{accounts: map[string][]byte{keep.String(): {0x02}}}
	source := func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
		return []SolanaSubscriptionRef{
			{SubscriptionPDA: keep.String()},
			{SubscriptionPDA: skip.String()},
		}, nil
	}

	snap, err := (&SolanaFetcher{RPC: rpc, Source: source}).Fetch(context.Background(), FetchParams{SubscriptionID: keep.String()})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 1)
	require.Equal(t, keep.String(), snap.Subscriptions[0].RailSubscriptionID)
}
