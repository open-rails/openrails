package reconcile

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	solrpc "github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// usdcMint is the canonical USDC mainnet mint (declared in the repo token
// registry with 6 decimals — base units are micro-dollars).
var usdcMint = solanago.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")

// fakeSolanaRPC serves canned account data, signature listings, transactions
// and program-account listings (memcmp filters applied byte-for-byte).
type fakeSolanaRPC struct {
	accounts            map[string][]byte
	signatures          map[string][]solanaint.SignatureInfo
	transactions        map[string]*solrpc.GetTransactionResult
	programAccounts     []solanaint.ProgramAccount
	pageCalls           int
	programAccountCalls int
	accountDataCalls    int
}

func (f *fakeSolanaRPC) GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error) {
	f.accountDataCalls++
	return f.accounts[address.String()], nil
}

func (f *fakeSolanaRPC) GetSignaturesForAddressPage(ctx context.Context, address, before string, limit int) ([]solanaint.SignatureInfo, error) {
	f.pageCalls++
	sigs := f.signatures[address]
	start := 0
	if before != "" {
		for i := range sigs {
			if sigs[i].Signature == before {
				start = i + 1
				break
			}
		}
	}
	if start >= len(sigs) {
		return nil, nil
	}
	end := len(sigs)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return sigs[start:end], nil
}

func (f *fakeSolanaRPC) GetTransaction(ctx context.Context, signature solanago.Signature) (*solrpc.GetTransactionResult, error) {
	if res, ok := f.transactions[signature.String()]; ok {
		return res, nil
	}
	return nil, errors.New("transaction not found")
}

func (f *fakeSolanaRPC) GetProgramAccounts(ctx context.Context, program solanago.PublicKey, filters []solanaint.ProgramAccountFilter) ([]solanaint.ProgramAccount, error) {
	f.programAccountCalls++
	var out []solanaint.ProgramAccount
	for _, acc := range f.programAccounts {
		match := true
		for _, flt := range filters {
			o := int(flt.Offset)
			if o+len(flt.Bytes) > len(acc.Data) || !bytes.Equal(acc.Data[o:o+len(flt.Bytes)], flt.Bytes) {
				match = false
				break
			}
		}
		if match {
			out = append(out, acc)
		}
	}
	return out, nil
}

// buildPlanBlob assembles a synthetic Plan account in the on-chain layout
// (discriminator 1; fixed-size little-endian fields; 491 bytes total).
func buildPlanBlob(t *testing.T, mint solanago.PublicKey, amount, periodHours uint64, endTs int64) []byte {
	t.Helper()
	blob := make([]byte, 0, subscriptions.PlanAccountSize)
	blob = append(blob, 1)                           // discriminator
	blob = append(blob, make([]byte, 32)...)         // owner
	blob = append(blob, 0xFE)                        // bump
	blob = append(blob, 1)                           // status
	blob = binary.LittleEndian.AppendUint64(blob, 7) // planId
	blob = append(blob, mint.Bytes()...)
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

// buildSubBlob assembles a synthetic SubscriptionDelegation account in the
// program's v1 layout (155 bytes; see subscriptions.DecodeSubscriptionAccount).
func buildSubBlob(t *testing.T, delegator, delegatee solanago.PublicKey, amount, periodHours uint64, pulled uint64, periodStart, expiresAt int64) []byte {
	t.Helper()
	blob := make([]byte, 0, subscriptions.SubscriptionAccountSize)
	blob = append(blob, 4)    // discriminator (subscriptionDelegation)
	blob = append(blob, 1)    // version
	blob = append(blob, 0xFD) // bump
	blob = append(blob, delegator.Bytes()...)
	blob = append(blob, delegatee.Bytes()...)
	blob = append(blob, make([]byte, 32)...)         // payer
	blob = binary.LittleEndian.AppendUint64(blob, 9) // initId
	blob = binary.LittleEndian.AppendUint64(blob, amount)
	blob = binary.LittleEndian.AppendUint64(blob, periodHours)
	blob = binary.LittleEndian.AppendUint64(blob, uint64(1700000000)) // createdAt
	blob = binary.LittleEndian.AppendUint64(blob, pulled)
	blob = binary.LittleEndian.AppendUint64(blob, uint64(periodStart))
	blob = binary.LittleEndian.AppendUint64(blob, uint64(expiresAt))
	require.Len(t, blob, subscriptions.SubscriptionAccountSize)
	return blob
}

// buildTxResult wraps instructions in a serialized transaction inside a
// GetTransactionResult (base64 envelope, as the RPC returns it).
func buildTxResult(t *testing.T, hasErr bool, ixs ...solanago.Instruction) *solrpc.GetTransactionResult {
	t.Helper()
	meta := `{"err":null}`
	if hasErr {
		meta = `{"err":{"InstructionError":[0,{"Custom":1}]}}`
	}
	return buildTxResultMeta(t, meta, ixs...)
}

// buildTxResultMeta is buildTxResult with a caller-supplied meta JSON (token
// balance metas for the wallet-scan money truth).
func buildTxResultMeta(t *testing.T, meta string, ixs ...solanago.Instruction) *solrpc.GetTransactionResult {
	t.Helper()
	payer := solanago.NewWallet().PublicKey()
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(payer))
	require.NoError(t, err)
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	var res solrpc.GetTransactionResult
	require.NoError(t, json.Unmarshal(
		[]byte(`{"slot":1,"transaction":["`+base64.StdEncoding.EncodeToString(raw)+`","base64"],"meta":`+meta+`}`), &res))
	return &res
}

// tokenIntoWalletMeta builds a tx meta whose token balances show `post-pre`
// base units of mint arriving in a wallet-owned token account.
func tokenIntoWalletMeta(wallet, mint solanago.PublicKey, pre, post uint64) string {
	return fmt.Sprintf(`{"err":null,`+
		`"preTokenBalances":[{"accountIndex":1,"mint":%q,"owner":%q,"uiTokenAmount":{"amount":"%d","decimals":6}}],`+
		`"postTokenBalances":[{"accountIndex":1,"mint":%q,"owner":%q,"uiTokenAmount":{"amount":"%d","decimals":6}}]}`,
		mint.String(), wallet.String(), pre, mint.String(), wallet.String(), post)
}

func sigFromByte(b byte) string {
	return solanago.SignatureFromBytes(bytes.Repeat([]byte{b}, 64)).String()
}

// discoverySlotBase anchors the #720 discovery-cadence search helpers below;
// an arbitrary fixed instant (deterministic, not wall-clock-dependent).
var discoverySlotBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// dueDiscoverySlot finds a time inside planPDA's #720 discovery slot
// (searched across exactly one cadence period, so it always terminates).
func dueDiscoverySlot(t *testing.T, planPDA string) time.Time {
	t.Helper()
	slots := int(solanaDiscoveryCadence / solanaDiscoverySlotWidth)
	for i := 0; i < slots; i++ {
		c := discoverySlotBase.Add(time.Duration(i) * solanaDiscoverySlotWidth)
		if planDiscoveryDue(planPDA, c) {
			return c
		}
	}
	t.Fatalf("no due discovery slot found for plan %s", planPDA)
	return time.Time{}
}

// notDueDiscoverySlot finds a time OUTSIDE planPDA's #720 discovery slot.
func notDueDiscoverySlot(t *testing.T, planPDA string) time.Time {
	t.Helper()
	slots := int(solanaDiscoveryCadence / solanaDiscoverySlotWidth)
	for i := 0; i < slots; i++ {
		c := discoverySlotBase.Add(time.Duration(i) * solanaDiscoverySlotWidth)
		if !planDiscoveryDue(planPDA, c) {
			return c
		}
	}
	t.Fatalf("no non-due discovery slot found for plan %s", planPDA)
	return time.Time{}
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
			openSub.String(): {0x02, 0x01, 0x02}, // exists but undecodable => presence inference
			planPDA.String(): buildPlanBlob(t, solanago.PublicKey{}, 5_000_000, 720, 0),
			// closedSub absent => account closed
		},
		signatures: map[string][]solanaint.SignatureInfo{
			openSub.String(): {
				// Not resolvable via GetTransaction => signature-only fallback.
				{Signature: sigFromByte(1), HasError: false, BlockTime: &blockTime},
				{Signature: sigFromByte(2), HasError: true, BlockTime: &blockTime},
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
	require.Zero(t, open.AmountCents) // zero mint is not a registry stablecoin
	require.Contains(t, string(open.Raw), `"amount":5000000`)
	require.Contains(t, string(open.Raw), `"period_hours":720`)
	require.Contains(t, string(open.Raw), "subscription_decode_error")

	closed := snap.Subscriptions[1]
	require.Equal(t, SubscriptionStatusCancelled, closed.Status)
	require.Equal(t, "account_closed", closed.RawStatus)

	// Signature listing for the open subscription only (closed has none); both
	// take the signature-only fallback since GetTransaction knows neither.
	require.Len(t, snap.Transactions, 2)
	require.Equal(t, sigFromByte(1), snap.Transactions[0].TransactionID)
	require.Equal(t, TransactionTypeSale, snap.Transactions[0].Type)
	require.True(t, snap.Transactions[0].Success)
	require.Equal(t, blockTime, snap.Transactions[0].OccurredAt)
	require.Equal(t, openSub.String(), snap.Transactions[0].SubscriptionID)
	require.Contains(t, string(snap.Transactions[0].Raw), "signature_only")

	require.Equal(t, TransactionTypeDecline, snap.Transactions[1].Type)
	require.False(t, snap.Transactions[1].Success)
}

func TestSolanaFetcher_DecodedSubscriptionStatuses(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	activeSub := solanago.NewWallet().PublicKey()
	pendingSub := solanago.NewWallet().PublicKey()
	lapsedSub := solanago.NewWallet().PublicKey()
	planPDA := solanago.NewWallet().PublicKey()
	wallet := solanago.NewWallet().PublicKey()

	periodStart := now.Add(-100 * time.Hour).Unix()
	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{
			activeSub.String():  buildSubBlob(t, wallet, planPDA, 9_990_000, 720, 9_990_000, periodStart, 0),
			pendingSub.String(): buildSubBlob(t, wallet, planPDA, 9_990_000, 720, 0, periodStart, now.Add(time.Hour).Unix()),
			lapsedSub.String():  buildSubBlob(t, wallet, planPDA, 9_990_000, 720, 0, periodStart, now.Add(-time.Hour).Unix()),
			planPDA.String():    buildPlanBlob(t, usdcMint, 9_990_000, 720, 0),
		},
	}
	source := func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
		return []SolanaSubscriptionRef{
			{SubscriptionPDA: activeSub.String(), PlanPDA: planPDA.String(), SubscriberWallet: wallet.String()},
			{SubscriptionPDA: pendingSub.String(), PlanPDA: planPDA.String(), SubscriberWallet: wallet.String()},
			{SubscriptionPDA: lapsedSub.String(), PlanPDA: planPDA.String(), SubscriberWallet: wallet.String()},
		}, nil
	}

	snap, err := (&SolanaFetcher{RPC: rpc, Source: source}).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 3)

	active := snap.Subscriptions[0]
	require.Equal(t, SubscriptionStatusActive, active.Status)
	require.Equal(t, "active", active.RawStatus)
	// The chain's own declarations win over the local ref.
	require.Equal(t, wallet.String(), active.CustomerID)
	require.Equal(t, planPDA.String(), active.PlanID)
	// Wire-pinned fiat: 9_990_000 micro-USDC => exactly 999 cents.
	require.Equal(t, int64(999), active.AmountCents)
	require.Equal(t, "USD", active.Currency)
	// Next billing window opens at period start + period_hours.
	require.NotNil(t, active.NextBillingAt)
	require.Equal(t, time.Unix(periodStart, 0).UTC().Add(720*time.Hour), *active.NextBillingAt)
	require.Contains(t, string(active.Raw), `"expires_at_ts":0`)

	pending := snap.Subscriptions[1]
	require.Equal(t, SubscriptionStatusActive, pending.Status)
	require.Equal(t, "cancel_at_period_end", pending.RawStatus)
	require.Nil(t, pending.NextBillingAt) // no billing follows a scheduled cancel

	lapsed := snap.Subscriptions[2]
	require.Equal(t, SubscriptionStatusCancelled, lapsed.Status)
	require.Equal(t, "expires_at_passed", lapsed.RawStatus)
	require.Nil(t, lapsed.NextBillingAt)
}

func TestSolanaFetcher_PlanEndedMarksExpired(t *testing.T) {
	t.Parallel()

	sub := solanago.NewWallet().PublicKey()
	planPDA := solanago.NewWallet().PublicKey()

	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{
			sub.String():     buildSubBlob(t, solanago.PublicKey{}, planPDA, 1, 720, 0, 0, 0),
			planPDA.String(): buildPlanBlob(t, solanago.PublicKey{}, 1, 720, time.Now().Add(-time.Hour).Unix()),
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

func TestSolanaFetcher_TransactionClassification(t *testing.T) {
	t.Parallel()

	sub := solanago.NewWallet().PublicKey()
	wallet := solanago.NewWallet().PublicKey()

	transferIx := func(amount uint64, mint solanago.PublicKey) solanago.Instruction {
		return subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
			SubscriptionPDA:       sub,
			PlanPDA:               solanago.NewWallet().PublicKey(),
			SubscriptionAuthority: solanago.NewWallet().PublicKey(),
			DelegatorATA:          solanago.NewWallet().PublicKey(),
			ReceiverATA:           solanago.NewWallet().PublicKey(),
			Caller:                solanago.NewWallet().PublicKey(),
			Mint:                  mint,
			TokenProgram:          solanago.TokenProgramID,
			EventAuthority:        solanago.NewWallet().PublicKey(),
			Amount:                amount,
			Delegator:             wallet,
		})
	}
	cancelIx := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber:      wallet,
		PlanPDA:         solanago.NewWallet().PublicKey(),
		SubscriptionPDA: sub,
		EventAuthority:  solanago.NewWallet().PublicKey(),
	})
	subscribeIx := subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
		Subscriber:               wallet,
		Merchant:                 solanago.NewWallet().PublicKey(),
		PlanPDA:                  solanago.NewWallet().PublicKey(),
		SubscriptionPDA:          sub,
		SubscriptionAuthorityPDA: solanago.NewWallet().PublicKey(),
		EventAuthority:           solanago.NewWallet().PublicKey(),
		ExpectedMint:             usdcMint,
	})

	sigPull, sigOdd, sigSubscribe, sigCancel, sigFailPull, sigUnknown :=
		sigFromByte(1), sigFromByte(2), sigFromByte(3), sigFromByte(4), sigFromByte(5), sigFromByte(6)

	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{sub.String(): {0x02}},
		signatures: map[string][]solanaint.SignatureInfo{
			sub.String(): {
				{Signature: sigPull},
				{Signature: sigOdd},
				{Signature: sigSubscribe},
				{Signature: sigCancel},
				{Signature: sigFailPull, HasError: true},
				{Signature: sigUnknown},
			},
		},
		transactions: map[string]*solrpc.GetTransactionResult{
			sigPull:      buildTxResult(t, false, transferIx(9_990_000, usdcMint)),
			sigOdd:       buildTxResult(t, false, transferIx(1_234_567, usdcMint)),
			sigSubscribe: buildTxResult(t, false, subscribeIx),
			sigCancel:    buildTxResult(t, false, cancelIx),
			sigFailPull:  buildTxResult(t, true, transferIx(9_990_000, usdcMint)),
			// sigUnknown deliberately absent => signature-only fallback.
		},
	}
	source := func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
		return []SolanaSubscriptionRef{{SubscriptionPDA: sub.String(), SubscriberWallet: wallet.String()}}, nil
	}

	snap, err := (&SolanaFetcher{RPC: rpc, Source: source}).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Transactions, 6)

	pull := snap.Transactions[0]
	require.Equal(t, TransactionTypeSale, pull.Type)
	require.True(t, pull.Success)
	// Wire-pinned: 9_990_000 micro-USDC => exactly 999 cents.
	require.Equal(t, int64(999), pull.AmountCents)
	require.Equal(t, "USD", pull.Currency)
	require.Contains(t, string(pull.Raw), `"amount_base_units":9990000`)
	require.Contains(t, string(pull.Raw), usdcMint.String())
	require.Contains(t, string(pull.Raw), "transfer_subscription")

	odd := snap.Transactions[1]
	require.Equal(t, TransactionTypeSale, odd.Type)
	// Sub-cent precision is never rounded: 1_234_567 micros stays unnormalized.
	require.Zero(t, odd.AmountCents)
	require.Empty(t, odd.Currency)
	require.Contains(t, string(odd.Raw), "fiat_note")
	require.Contains(t, string(odd.Raw), `"amount_base_units":1234567`)

	require.Equal(t, TransactionTypeSubscribe, snap.Transactions[2].Type)
	require.True(t, snap.Transactions[2].Success)
	require.Zero(t, snap.Transactions[2].AmountCents)

	require.Equal(t, TransactionTypeCancel, snap.Transactions[3].Type)

	failPull := snap.Transactions[4]
	require.Equal(t, TransactionTypeDecline, failPull.Type)
	require.False(t, failPull.Success)
	require.Contains(t, failPull.DeclineReason, "on-chain failure")

	fallback := snap.Transactions[5]
	require.Equal(t, TransactionTypeSale, fallback.Type)
	require.Contains(t, string(fallback.Raw), "signature_only")
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

// TestSolanaFetcher_WalletScanClassification is the #714 recognize/ignore/park
// table: no memo => nothing; foreign memo => nothing; recognized + clean =>
// finding money from the transfer; memo'd but mismatched => park; duplicate
// local-id => park; failed => decline evidence without a verdict.
func TestSolanaFetcher_WalletScanClassification(t *testing.T) {
	t.Parallel()

	wallet := solanago.NewWallet().PublicKey()
	pullSub := solanago.NewWallet().PublicKey()
	customerID, priceID := uuid.New(), uuid.New()

	cleanID, mismatchID, dupID, noneID, valuelessID, failedID, pullIntentID, oldID :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	session := func(tokenAmount uint64) *SolanaLocalRecord {
		return &SolanaLocalRecord{
			Kind:                SolanaLocalKindCheckoutSession,
			Rail:                "solana",
			CustomerID:          customerID,
			PriceID:             priceID,
			ExpectedRecipient:   wallet.String(),
			ExpectedMint:        usdcMint.String(),
			ExpectedTokenAmount: tokenAmount,
		}
	}
	records := map[uuid.UUID]*SolanaLocalRecord{
		cleanID:      session(9_990_000),
		mismatchID:   session(5_000_000),
		dupID:        session(9_990_000),
		failedID:     session(9_990_000),
		oldID:        session(9_990_000),
		pullIntentID: {Kind: SolanaLocalKindPullIntent, Rail: "solana", SubscriptionPDA: pullSub.String()},
	}
	resolve := func(ctx context.Context, id uuid.UUID) (*SolanaLocalRecord, error) {
		return records[id], nil
	}

	memoIx := func(id uuid.UUID) solanago.Instruction {
		return solanaint.NewMemoInstruction(solanaint.PurchaseMemo(id))
	}
	usdcMeta := tokenIntoWalletMeta(wallet, usdcMint, 0, 9_990_000)
	pullIx := subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA:       pullSub,
		PlanPDA:               solanago.NewWallet().PublicKey(),
		SubscriptionAuthority: solanago.NewWallet().PublicKey(),
		DelegatorATA:          solanago.NewWallet().PublicKey(),
		ReceiverATA:           solanago.NewWallet().PublicKey(),
		Caller:                solanago.NewWallet().PublicKey(),
		Mint:                  usdcMint,
		TokenProgram:          solanago.TokenProgramID,
		EventAuthority:        solanago.NewWallet().PublicKey(),
		Amount:                9_990_000,
		Delegator:             solanago.NewWallet().PublicKey(),
	})

	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	inWindow := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tooOld := since.Add(-time.Hour)

	sigNoMemo, sigForeign, sigClean, sigMismatch, sigDupA, sigDupB := sigFromByte(1), sigFromByte(2), sigFromByte(3), sigFromByte(4), sigFromByte(5), sigFromByte(6)
	sigNone, sigValueless, sigFailed, sigPull, sigOld := sigFromByte(7), sigFromByte(8), sigFromByte(9), sigFromByte(10), sigFromByte(11)

	sigInfo := func(sig string, at time.Time, hasErr bool) solanaint.SignatureInfo {
		bt := at
		return solanaint.SignatureInfo{Signature: sig, HasError: hasErr, BlockTime: &bt}
	}
	rpc := &fakeSolanaRPC{
		signatures: map[string][]solanaint.SignatureInfo{
			wallet.String(): {
				sigInfo(sigNoMemo, inWindow, false),
				sigInfo(sigForeign, inWindow, false),
				sigInfo(sigClean, inWindow, false),
				sigInfo(sigMismatch, inWindow, false),
				sigInfo(sigDupA, inWindow, false),
				sigInfo(sigDupB, inWindow, false),
				sigInfo(sigNone, inWindow, false),
				sigInfo(sigValueless, inWindow, false),
				sigInfo(sigFailed, inWindow, true),
				sigInfo(sigPull, inWindow, false),
				sigInfo(sigOld, tooOld, false), // newest-first: the below-Since floor
			},
		},
		transactions: map[string]*solrpc.GetTransactionResult{
			sigNoMemo:    buildTxResultMeta(t, usdcMeta, solanaint.NewMemoInstruction("gm")),
			sigForeign:   buildTxResultMeta(t, usdcMeta, solanaint.NewMemoInstruction("otherapp:1:"+uuid.New().String())),
			sigClean:     buildTxResultMeta(t, usdcMeta, memoIx(cleanID)),
			sigMismatch:  buildTxResultMeta(t, usdcMeta, memoIx(mismatchID)),
			sigDupA:      buildTxResultMeta(t, usdcMeta, memoIx(dupID)),
			sigDupB:      buildTxResultMeta(t, usdcMeta, memoIx(dupID)),
			sigNone:      buildTxResultMeta(t, usdcMeta, memoIx(noneID)),
			sigValueless: buildTxResultMeta(t, `{"err":null}`, memoIx(valuelessID)),
			sigFailed:    buildTxResultMeta(t, `{"err":{"InstructionError":[0,{"Custom":1}]}}`, memoIx(failedID)),
			sigPull:      buildTxResultMeta(t, usdcMeta, pullIx, memoIx(pullIntentID)),
			sigOld:       buildTxResultMeta(t, usdcMeta, memoIx(oldID)),
		},
	}

	fetcher := &SolanaFetcher{
		RPC:            rpc,
		Source:         func(ctx context.Context) ([]SolanaSubscriptionRef, error) { return nil, nil },
		Resolve:        resolve,
		MerchantWallet: wallet.String(),
	}
	snap, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until})
	require.NoError(t, err)

	byID := map[string]RemoteTransaction{}
	for _, txn := range snap.Transactions {
		byID[txn.TransactionID] = txn
	}

	for _, tc := range []struct {
		name         string
		sig          string
		present      bool
		txnType      TransactionType
		cents        int64
		verdict      string
		parkContains string
	}{
		{name: "no memo is invisible", sig: sigNoMemo},
		{name: "foreign memo is invisible", sig: sigForeign},
		{name: "valueless memo claiming nothing local is invisible", sig: sigValueless},
		{name: "below-Since is not scanned", sig: sigOld},
		// Wire-pinned money: 9_990_000 micro-USDC => exactly 999 cents.
		{name: "recognized clean one-off", sig: sigClean, present: true, txnType: TransactionTypeSale, cents: 999, verdict: "clean"},
		{name: "amount mismatch parks", sig: sigMismatch, present: true, txnType: TransactionTypeSale, cents: 999, verdict: "park", parkContains: "disagrees with the session's quoted"},
		{name: "duplicate local-id parks (a)", sig: sigDupA, present: true, txnType: TransactionTypeSale, cents: 999, verdict: "park", parkContains: "claimed by 2 successful transactions"},
		{name: "duplicate local-id parks (b)", sig: sigDupB, present: true, txnType: TransactionTypeSale, cents: 999, verdict: "park", parkContains: "claimed by 2 successful transactions"},
		{name: "no local record parks", sig: sigNone, present: true, txnType: TransactionTypeSale, cents: 999, verdict: "park", parkContains: "no local record"},
		{name: "failed attempt is decline evidence without a verdict", sig: sigFailed, present: true, txnType: TransactionTypeDecline},
		{name: "recognized pull is clean", sig: sigPull, present: true, txnType: TransactionTypeSale, cents: 999, verdict: "clean"},
	} {
		txn, ok := byID[tc.sig]
		require.Equal(t, tc.present, ok, tc.name)
		if !tc.present {
			continue
		}
		require.Equal(t, tc.txnType, txn.Type, tc.name)
		require.Equal(t, tc.cents, txn.AmountCents, tc.name)
		d := decodeSolanaDiscovery(txn.Raw)
		if tc.verdict == "" {
			require.Nil(t, d, tc.name)
			continue
		}
		require.NotNil(t, d, tc.name)
		require.Equal(t, tc.verdict, d.Verdict, tc.name)
		if tc.parkContains != "" {
			require.Contains(t, d.ParkReason, tc.parkContains, tc.name)
		}
	}

	// The clean one-off carries the resolved identity for the backfill lane.
	clean := decodeSolanaDiscovery(byID[sigClean].Raw)
	require.Equal(t, solanaDiscoveryKindOneOff, clean.Kind)
	require.Equal(t, customerID.String(), clean.CustomerID)
	require.Equal(t, priceID.String(), clean.PriceID)
	require.Equal(t, cleanID.String(), clean.MemoLocalID)
	require.Equal(t, "USD", byID[sigClean].Currency)
	require.Equal(t, inWindow, byID[sigClean].OccurredAt)

	// Duplicates never keep a backfillable identity.
	require.Empty(t, decodeSolanaDiscovery(byID[sigDupA].Raw).CustomerID)

	// The pull carries the subscription PDA for the generic correlator lane
	// and no backfill identity of its own.
	pull := decodeSolanaDiscovery(byID[sigPull].Raw)
	require.Equal(t, solanaDiscoveryKindPull, pull.Kind)
	require.Empty(t, pull.CustomerID)
	require.Equal(t, pullSub.String(), byID[sigPull].SubscriptionID)

	// Failed memo'd attempts keep the memo in Raw for forensics.
	require.Contains(t, string(byID[sigFailed].Raw), failedID.String())
}

// TestSolanaFetcher_WalletScanCap pins the bounded pagination: past the
// per-window cap nothing is fetched or emitted; within it everything is.
func TestSolanaFetcher_WalletScanCap(t *testing.T) {
	t.Parallel()

	wallet := solanago.NewWallet().PublicKey()
	customerID, priceID := uuid.New(), uuid.New()
	cleanID := uuid.New()
	records := map[uuid.UUID]*SolanaLocalRecord{
		cleanID: {
			Kind: SolanaLocalKindCheckoutSession, Rail: "solana",
			CustomerID: customerID, PriceID: priceID,
			ExpectedRecipient: wallet.String(), ExpectedMint: usdcMint.String(), ExpectedTokenAmount: 9_990_000,
		},
	}

	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	newRPC := func() *fakeSolanaRPC {
		var sigs []solanaint.SignatureInfo
		for b := byte(1); b <= 4; b++ {
			bt := at
			sigs = append(sigs, solanaint.SignatureInfo{Signature: sigFromByte(b), BlockTime: &bt})
		}
		return &fakeSolanaRPC{
			signatures: map[string][]solanaint.SignatureInfo{wallet.String(): sigs},
			transactions: map[string]*solrpc.GetTransactionResult{
				// Only the LAST (oldest) signature is a recognized purchase.
				sigFromByte(4): buildTxResultMeta(t, tokenIntoWalletMeta(wallet, usdcMint, 0, 9_990_000),
					solanaint.NewMemoInstruction(solanaint.PurchaseMemo(cleanID))),
			},
		}
	}
	newFetcher := func(rpc *fakeSolanaRPC, cap int) *SolanaFetcher {
		return &SolanaFetcher{
			RPC:                rpc,
			Source:             func(ctx context.Context) ([]SolanaSubscriptionRef, error) { return nil, nil },
			Resolve:            func(ctx context.Context, id uuid.UUID) (*SolanaLocalRecord, error) { return records[id], nil },
			MerchantWallet:     wallet.String(),
			WalletScanPageSize: 2,
			WalletScanCap:      cap,
		}
	}

	// Cap 3: the recognized 4th signature is never listed.
	capped := newRPC()
	snap, err := newFetcher(capped, 3).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Empty(t, snap.Transactions)
	require.Equal(t, 2, capped.pageCalls) // 2 + 1, then cap

	// Cap 10: the walk reaches it.
	full := newRPC()
	snap, err = newFetcher(full, 10).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Transactions, 1)
	require.Equal(t, sigFromByte(4), snap.Transactions[0].TransactionID)
	require.Equal(t, int64(999), snap.Transactions[0].AmountCents)
}

// TestSolanaFetcher_PlanEnumeration covers #714 scan 2: subscription accounts
// under OUR plans are enumerated via getProgramAccounts; the ones the local
// mirror does not know are emitted marked discovered_not_local, normalized
// exactly like locally-known ones (status doctrine + wire-pinned fiat).
func TestSolanaFetcher_PlanEnumeration(t *testing.T) {
	t.Parallel()

	subscriberA := solanago.NewWallet().PublicKey() // locally known
	subscriberB := solanago.NewWallet().PublicKey() // permissionless on-chain subscriber
	planPDA := solanago.NewWallet().PublicKey()
	otherPlan := solanago.NewWallet().PublicKey()
	knownSub := solanago.NewWallet().PublicKey()
	discoveredSub := solanago.NewWallet().PublicKey()
	foreignSub := solanago.NewWallet().PublicKey()

	periodStart := time.Now().Add(-100 * time.Hour).Unix()
	knownBlob := buildSubBlob(t, subscriberA, planPDA, 9_990_000, 720, 0, periodStart, 0)
	discoveredBlob := buildSubBlob(t, subscriberB, planPDA, 9_990_000, 720, 0, periodStart, 0)

	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{
			knownSub.String(): knownBlob,
			planPDA.String():  buildPlanBlob(t, usdcMint, 9_990_000, 720, 0),
		},
		programAccounts: []solanaint.ProgramAccount{
			{Address: knownSub, Data: knownBlob},
			{Address: discoveredSub, Data: discoveredBlob},
			{Address: foreignSub, Data: buildSubBlob(t, subscriberB, otherPlan, 9_990_000, 720, 0, periodStart, 0)},
		},
	}
	// #720: the plan enumeration lane runs on a slow cadence; pin Now inside
	// this plan's discovery slot so the test is deterministic regardless of
	// wall-clock time.
	due := dueDiscoverySlot(t, planPDA.String())
	fetcher := &SolanaFetcher{
		RPC: rpc,
		Source: func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
			return []SolanaSubscriptionRef{{SubscriptionPDA: knownSub.String(), PlanPDA: planPDA.String(), SubscriberWallet: subscriberA.String()}}, nil
		},
		Plans: func(ctx context.Context) ([]string, error) { return []string{planPDA.String()}, nil },
		Now:   func() time.Time { return due },
	}

	snap, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 2) // known once (lane 1), discovered once; foreign plan filtered out

	known := snap.Subscriptions[0]
	require.Equal(t, knownSub.String(), known.RailSubscriptionID)
	require.NotContains(t, string(known.Raw), "discovered_not_local")

	discovered := snap.Subscriptions[1]
	require.Equal(t, discoveredSub.String(), discovered.RailSubscriptionID)
	require.Equal(t, subscriberB.String(), discovered.CustomerID)
	require.Equal(t, planPDA.String(), discovered.PlanID)
	require.Equal(t, SubscriptionStatusActive, discovered.Status)
	require.Equal(t, "active", discovered.RawStatus)
	require.Equal(t, int64(999), discovered.AmountCents) // wire-pinned from the terms snapshot
	require.Equal(t, "USD", discovered.Currency)
	require.Contains(t, string(discovered.Raw), `"discovered_not_local":true`)
	require.Contains(t, string(discovered.Raw), `"source":"solana_program_scan"`)
}

// TestWalletTransferMoney pins the wallet-scan money truth: the ONE asset the
// transaction moved into the wallet, from balance metas only.
func TestWalletTransferMoney(t *testing.T) {
	t.Parallel()

	wallet := solanago.NewWallet().PublicKey()
	other := solanago.NewWallet().PublicKey()
	tb := func(idx uint16, owner solanago.PublicKey, mint solanago.PublicKey, amt uint64) solrpc.TokenBalance {
		o := owner
		return solrpc.TokenBalance{AccountIndex: idx, Mint: mint, Owner: &o, UiTokenAmount: &solrpc.UiTokenAmount{Amount: fmt.Sprintf("%d", amt)}}
	}

	// SPL into the wallet: exact base units, wire-pinned.
	got, ok, _ := walletTransferMoney(&solrpc.TransactionMeta{
		PreTokenBalances:  []solrpc.TokenBalance{tb(1, wallet, usdcMint, 10_000)},
		PostTokenBalances: []solrpc.TokenBalance{tb(1, wallet, usdcMint, 10_000_000)},
	}, nil, wallet)
	require.True(t, ok)
	require.Equal(t, walletTransfer{Mint: usdcMint.String(), BaseUnits: 9_990_000}, got)

	// Another owner's balances never count as ours.
	_, ok, note := walletTransferMoney(&solrpc.TransactionMeta{
		PostTokenBalances: []solrpc.TokenBalance{tb(1, other, usdcMint, 9_990_000)},
	}, nil, wallet)
	require.False(t, ok)
	require.Contains(t, note, "no transfer into the merchant wallet")

	// Native SOL into the wallet: lamports delta at the wallet's key index.
	payer := solanago.NewWallet().PublicKey()
	tx, err := solanago.NewTransaction(
		[]solanago.Instruction{system.NewTransferInstruction(5_000, payer, wallet).Build()},
		solanago.Hash{}, solanago.TransactionPayer(payer))
	require.NoError(t, err)
	walletIdx := -1
	for i, key := range tx.Message.AccountKeys {
		if key.Equals(wallet) {
			walletIdx = i
		}
	}
	require.NotEqual(t, -1, walletIdx)
	pre := make([]uint64, len(tx.Message.AccountKeys))
	post := make([]uint64, len(tx.Message.AccountKeys))
	pre[walletIdx], post[walletIdx] = 100, 5_100
	got, ok, _ = walletTransferMoney(&solrpc.TransactionMeta{PreBalances: pre, PostBalances: post}, tx, wallet)
	require.True(t, ok)
	require.Equal(t, walletTransfer{Mint: "", BaseUnits: 5_000}, got)

	// Multiple assets in: ambiguous, never guessed.
	_, ok, note = walletTransferMoney(&solrpc.TransactionMeta{
		PostTokenBalances: []solrpc.TokenBalance{tb(1, wallet, usdcMint, 100), tb(2, wallet, other, 5)},
	}, nil, wallet)
	require.False(t, ok)
	require.Contains(t, note, "multiple assets")

	// No meta: unusable.
	_, ok, _ = walletTransferMoney(nil, nil, wallet)
	require.False(t, ok)

	// A token balance beyond int64 range is refused, never wrapped into a
	// bogus negative/small delta.
	_, ok, note = walletTransferMoney(&solrpc.TransactionMeta{
		PostTokenBalances: []solrpc.TokenBalance{tb(1, wallet, usdcMint, math.MaxInt64+1)},
	}, nil, wallet)
	require.False(t, ok)
	require.Contains(t, note, "exceeds representable range")
}

// TestDiffSolanaDiscoveryRouting pins the #714 finding routes: clean+priced
// one-offs backfill via the shared payment lane (money from the transfer),
// parks and unpriced discoveries go to the operator queue with no apply
// action, already-recorded payments produce nothing, and clean pulls take the
// generic correlator lane (unknown subscription => park).
func TestDiffSolanaDiscoveryRouting(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	customerID, priceID := uuid.New(), uuid.New()
	sessionID := uuid.New()

	envelope := func(d *solanaDiscovery, extra map[string]any) json.RawMessage {
		raw := map[string]any{"solana_discovery": d}
		for k, v := range extra {
			raw[k] = v
		}
		return rawJSON(raw)
	}
	clean := &solanaDiscovery{
		Verdict: "clean", Kind: solanaDiscoveryKindOneOff, MemoLocalID: sessionID.String(),
		LocalKind: SolanaLocalKindCheckoutSession, CustomerID: customerID.String(), PriceID: priceID.String(),
	}
	parked := &solanaDiscovery{
		Verdict: "park", ParkReason: "transfer of 1 base units disagrees with the session's quoted 2",
		Kind: solanaDiscoveryKindOneOff, MemoLocalID: uuid.New().String(), LocalKind: SolanaLocalKindCheckoutSession,
	}
	unpriced := &solanaDiscovery{
		Verdict: "clean", Kind: solanaDiscoveryKindOneOff, MemoLocalID: uuid.New().String(),
		LocalKind: SolanaLocalKindCheckoutSession, CustomerID: customerID.String(), PriceID: priceID.String(),
	}
	pull := &solanaDiscovery{
		Verdict: "clean", Kind: solanaDiscoveryKindPull, MemoLocalID: uuid.New().String(), LocalKind: SolanaLocalKindPullIntent,
	}

	sigClean, sigPark, sigUnpriced, sigRecorded, sigPull :=
		sigFromByte(1), sigFromByte(2), sigFromByte(3), sigFromByte(4), sigFromByte(5)
	snap := &RemoteSnapshot{
		Provider:     ProviderSolana,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true},
		Transactions: []RemoteTransaction{
			{TransactionID: sigClean, Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now, Raw: envelope(clean, nil)},
			{TransactionID: sigPark, Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now, Raw: envelope(parked, nil)},
			{TransactionID: sigUnpriced, Type: TransactionTypeSale, Success: true, AmountCents: 0, OccurredAt: now, Raw: envelope(unpriced, map[string]any{"lamports": uint64(5_000)})},
			{TransactionID: sigRecorded, Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now, Raw: envelope(clean, nil)},
			{TransactionID: sigPull, Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: now, SubscriptionID: solanago.NewWallet().PublicKey().String(), Raw: envelope(pull, nil)},
		},
	}
	localPayments := []LocalPayment{{ID: uuid.New(), Rail: "solana", TransactionID: sigRecorded, AmountCents: 999, Status: "completed"}}

	findings := diffProvider(ProviderSolana, snap, &LocalState{}, localPayments, now, diffOptions{})
	byKey := map[string]Finding{}
	for _, f := range findings {
		byKey[f.SubjectKey] = f
	}
	require.Len(t, findings, 4) // recorded one produces nothing

	fClean := byKey[sigClean]
	require.Equal(t, FindingChargeMissingLocal, fClean.Type)
	require.Equal(t, FindingStatusReconcileRequired, fClean.Status)
	require.False(t, fClean.RequiresAdmin)
	require.NotNil(t, fClean.Apply)
	require.NotNil(t, fClean.Apply.BackfillPayment)
	b := fClean.Apply.BackfillPayment
	require.Equal(t, "solana", b.Rail)
	require.Equal(t, int64(999), b.AmountCents) // wire-pinned: money from the transfer
	require.Equal(t, "usd", b.Currency)
	require.Equal(t, customerID, b.CustomerID)
	require.Equal(t, priceID, b.PriceID)
	require.Equal(t, sessionID.String(), fClean.LocalEvidence["checkout_session_id"])
	require.Equal(t, "purchase_memo", fClean.LocalEvidence["correlated_via"])

	fPark := byKey[sigPark]
	require.Equal(t, FindingStatusAdminRequired, fPark.Status)
	require.True(t, fPark.RequiresAdmin)
	require.Nil(t, fPark.Apply)
	require.Contains(t, fPark.RecommendedAction, "verify-not-decline")
	require.Contains(t, fPark.RemoteEvidence["park_reason"], "disagrees")

	fUnpriced := byKey[sigUnpriced]
	require.Equal(t, FindingStatusAdminRequired, fUnpriced.Status)
	require.Nil(t, fUnpriced.Apply)
	require.Contains(t, fUnpriced.RemoteEvidence["park_reason"], "unpriced")

	// Clean pull with an unknown subscription takes the generic PS-4 lane and
	// parks there (no local identity — never guessed).
	fPull := byKey[sigPull]
	require.Equal(t, FindingChargeMissingLocal, fPull.Type)
	require.Equal(t, FindingStatusAdminRequired, fPull.Status)
	require.Nil(t, fPull.Apply)
}

// TestDiffSolanaDiscoveredSubscriptionNeverMaterializes pins the #714 triage
// rule: a chain-discovered subscription (permissionless subscribe) surfaces as
// PS-1 but NEVER auto-creates local billing state, even when identity and plan
// would resolve — the identical non-discovered remote row does materialize.
func TestDiffSolanaDiscoveredSubscriptionNeverMaterializes(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	wallet := solanago.NewWallet().PublicKey().String()
	planPDA := solanago.NewWallet().PublicKey().String()
	subPDA := solanago.NewWallet().PublicKey().String()
	customerID, priceID := uuid.New(), uuid.New()

	local := &LocalState{
		PaymentMethods: []LocalPaymentMethod{{ID: uuid.New(), CustomerID: customerID, Rail: "solana", RailCustomerRef: wallet}},
		Prices: []LocalPrice{{
			ID: priceID, ProductID: uuid.New(), Amount: 9_990_000, Currency: "USD",
			PSPLinks: map[string]map[string]string{"solana": {"rail": "solana", "plan_pda": planPDA, "provider": "solana"}},
		}},
	}
	remote := func(raw json.RawMessage) *RemoteSnapshot {
		return &RemoteSnapshot{
			Provider:     ProviderSolana,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true},
			Subscriptions: []RemoteSubscription{{
				RailSubscriptionID: subPDA,
				Status:             SubscriptionStatusActive,
				CustomerID:         wallet,
				PlanID:             planPDA,
				Raw:                raw,
			}},
		}
	}

	// Control: without the discovery marker, identity (vault) + plan (plan_pda
	// link) resolve and enforce would materialize.
	findings := diffProvider(ProviderSolana, remote(rawJSON(map[string]any{"source": "solana_subscription_account"})), local, nil, now, diffOptions{Materialize: true})
	require.Len(t, findings, 1)
	require.Equal(t, FindingRemoteSubMissingLocal, findings[0].Type)
	require.NotNil(t, findings[0].Apply)
	require.NotNil(t, findings[0].Apply.Materialize)

	// Discovered: same evidence, but creation is an operator decision.
	findings = diffProvider(ProviderSolana, remote(rawJSON(map[string]any{"discovered_not_local": true})), local, nil, now, diffOptions{Materialize: true})
	require.Len(t, findings, 1)
	f := findings[0]
	require.Equal(t, FindingRemoteSubMissingLocal, f.Type)
	require.Equal(t, FindingStatusAdminRequired, f.Status)
	require.True(t, f.RequiresAdmin)
	require.Nil(t, f.Apply)
	require.Contains(t, f.RemoteEvidence["materialize_blocked"], "#714")
}

// TestSolanaFiatCents wire-pins the micro-dollar => cent boundary (money-unit
// doctrine: known base units produce the exact fiat value or nothing).
func TestSolanaFiatCents(t *testing.T) {
	t.Parallel()

	usdc := usdcMint.String()
	for _, tc := range []struct {
		mint     string
		decimals int
		base     uint64
		cents    int64
		ok       bool
	}{
		{usdc, 6, 1_000_000, 100, true}, // $1.00
		{usdc, 6, 10_000, 1, true},      // exactly one cent
		{usdc, 6, 9_990_000, 999, true}, // $9.99
		{usdc, 6, 123_450_000, 12_345, true},
		{usdc, 6, 1_234_567, 0, false}, // sub-cent precision: never rounded
		{usdc, 6, 9_999, 0, false},
		// #817: the shift is the MINT's, not a baked-in 6. The same $1.00 is a
		// different base-unit integer at 9 and at 2 decimals.
		{usdc, 9, 1_000_000_000, 100, true},
		{usdc, 9, 1_000_000, 0, false}, // $0.001 — sub-cent at 9 decimals
		{usdc, 2, 100, 100, true},
		{usdc, 0, 1, 0, false},  // 0 decimals cannot represent a cent
		{usdc, 19, 1, 0, false}, // outside the payable range
		// Registry USD stablecoins.
		{"USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB", 6, 5_000_000, 500, true},  // USD1
		{"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", 6, 5_000_000, 500, true}, // PYUSD
		{"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", 6, 5_000_000, 500, true}, // USDG
		{"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", 6, 1_000_000, 100, true}, // USDT
		// Devnet USDC is not in the canonical stablecoin registry.
		{"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", 6, 1_000_000, 0, false},
		// SOL: not a stablecoin.
		{"So11111111111111111111111111111111111111112", 9, 1_000_000_000, 0, false},
		{"", 6, 1_000_000, 0, false},
	} {
		cents, ok := solanaFiatCents(tc.mint, tc.decimals, tc.base)
		require.Equal(t, tc.ok, ok, "mint %s decimals %d base %d", tc.mint, tc.decimals, tc.base)
		require.Equal(t, tc.cents, cents, "mint %s decimals %d base %d", tc.mint, tc.decimals, tc.base)
	}
}

// TestSolanaFetcher_DueWindow pins the #720 due-window boundary: a ref's
// chain read only happens when the fake Due source (simulating
// ListDueSolanaSubscriptions) reports it at/before now+lead — just-inside
// and past-due refs are read, just-outside is skipped.
func TestSolanaFetcher_DueWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	lead := 4 * time.Hour

	justInside := solanago.NewWallet().PublicKey()
	justOutside := solanago.NewWallet().PublicKey()
	pastDue := solanago.NewWallet().PublicKey()

	// nextPullAt mirrors what a real openrails.solana_subscriptions row would
	// carry; the fake Due closure applies the SAME inequality the SQL query
	// does (next_pull_at <= before) so the test exercises dueWindowLead()'s
	// wiring, not just set membership.
	nextPullAt := map[string]time.Time{
		justInside.String():  now.Add(lead),               // exactly at the window edge -> due
		justOutside.String(): now.Add(lead + time.Minute), // one minute past the edge -> not due
		pastDue.String():     now.Add(-48 * time.Hour),    // long overdue (stuck/dunning) -> due
	}
	due := func(ctx context.Context, before time.Time) (map[string]struct{}, error) {
		out := map[string]struct{}{}
		for pda, npa := range nextPullAt {
			if !npa.After(before) {
				out[pda] = struct{}{}
			}
		}
		return out, nil
	}

	accounts := map[string][]byte{
		justInside.String():  {0x02},
		justOutside.String(): {0x02},
		pastDue.String():     {0x02},
	}
	rpc := &fakeSolanaRPC{accounts: accounts}
	refs := []SolanaSubscriptionRef{
		{SubscriptionPDA: justInside.String()},
		{SubscriptionPDA: justOutside.String()},
		{SubscriptionPDA: pastDue.String()},
	}
	fetcher := &SolanaFetcher{
		RPC:           rpc,
		Source:        func(ctx context.Context) ([]SolanaSubscriptionRef, error) { return refs, nil },
		Due:           due,
		DueWindowLead: lead,
		Now:           func() time.Time { return now },
	}

	snap, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)

	var got []string
	for _, s := range snap.Subscriptions {
		got = append(got, s.RailSubscriptionID)
	}
	require.ElementsMatch(t, []string{justInside.String(), pastDue.String()}, got)
	// 2 subscription-account reads (skipped one never calls GetAccountData).
	require.Equal(t, 2, rpc.accountDataCalls)
}

// TestSolanaFetcher_DueWindowNoData pins the "no period data" boundary: Due
// unset entirely (the fetcher has no way to know a due window) fails open —
// every locally-known ref is still read, exactly like pre-#720 behavior.
func TestSolanaFetcher_DueWindowNoData(t *testing.T) {
	t.Parallel()

	a := solanago.NewWallet().PublicKey()
	b := solanago.NewWallet().PublicKey()
	rpc := &fakeSolanaRPC{accounts: map[string][]byte{a.String(): {0x02}, b.String(): {0x02}}}
	refs := []SolanaSubscriptionRef{{SubscriptionPDA: a.String()}, {SubscriptionPDA: b.String()}}

	fetcher := &SolanaFetcher{
		RPC:    rpc,
		Source: func(ctx context.Context) ([]SolanaSubscriptionRef, error) { return refs, nil },
		// Due left nil.
	}
	snap, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 2)
	require.Equal(t, 2, rpc.accountDataCalls)
}

// TestSolanaFetcher_DueWindowBypassedByNarrowedFetch pins that an explicit
// per-subscription probe always reads regardless of the due window — an
// operator asking for one subscription by id gets it now, not next tick.
func TestSolanaFetcher_DueWindowBypassedByNarrowedFetch(t *testing.T) {
	t.Parallel()

	target := solanago.NewWallet().PublicKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	rpc := &fakeSolanaRPC{accounts: map[string][]byte{target.String(): {0x02}}}
	fetcher := &SolanaFetcher{
		RPC: rpc,
		Source: func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
			return []SolanaSubscriptionRef{{SubscriptionPDA: target.String()}}, nil
		},
		Due: func(ctx context.Context, before time.Time) (map[string]struct{}, error) {
			return map[string]struct{}{}, nil // nothing is due
		},
		Now: func() time.Time { return now },
	}

	snap, err := fetcher.Fetch(context.Background(), FetchParams{SubscriptionID: target.String()})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 1)
	require.Equal(t, target.String(), snap.Subscriptions[0].RailSubscriptionID)
}

// TestSolanaPlanDiscoveryCadence pins the #720 slow-cadence gate as a pure
// function: exactly one of the day's slots is a given plan's turn, and the
// same slot recurs every solanaDiscoveryCadence with no stored state.
func TestSolanaPlanDiscoveryCadence(t *testing.T) {
	t.Parallel()

	plan := solanago.NewWallet().PublicKey().String()
	slots := int(solanaDiscoveryCadence / solanaDiscoverySlotWidth)

	dueCount := 0
	var due time.Time
	for i := 0; i < slots; i++ {
		c := discoverySlotBase.Add(time.Duration(i) * solanaDiscoverySlotWidth)
		if planDiscoveryDue(plan, c) {
			dueCount++
			due = c
		}
	}
	require.Equal(t, 1, dueCount, "exactly one slot per day is this plan's turn")

	// Recurs every cadence period with no stored watermark.
	require.True(t, planDiscoveryDue(plan, due.Add(solanaDiscoveryCadence)))
	require.True(t, planDiscoveryDue(plan, due.Add(2*solanaDiscoveryCadence)))
	// Neighboring slots (same day) are not due.
	require.False(t, planDiscoveryDue(plan, due.Add(solanaDiscoverySlotWidth)))
	require.False(t, planDiscoveryDue(plan, due.Add(-solanaDiscoverySlotWidth)))
}

// TestSolanaFetcher_DiscoveryCadenceGatesEnumeration proves the cadence gate
// is actually wired into Fetch(): getProgramAccounts fires on the plan's due
// slot and not on any other slot, so a routine 4h tick does NOT call it for
// every plan on file.
func TestSolanaFetcher_DiscoveryCadenceGatesEnumeration(t *testing.T) {
	t.Parallel()

	planPDA := solanago.NewWallet().PublicKey().String()
	due := dueDiscoverySlot(t, planPDA)
	notDue := notDueDiscoverySlot(t, planPDA)

	rpc := &fakeSolanaRPC{}
	fetcher := &SolanaFetcher{
		RPC:    rpc,
		Source: func(ctx context.Context) ([]SolanaSubscriptionRef, error) { return nil, nil },
		Plans:  func(ctx context.Context) ([]string, error) { return []string{planPDA}, nil },
	}

	fetcher.Now = func() time.Time { return notDue }
	_, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Zero(t, rpc.programAccountCalls, "enumeration must not run outside the plan's discovery slot")

	fetcher.Now = func() time.Time { return due }
	_, err = fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Equal(t, 1, rpc.programAccountCalls, "enumeration runs once the plan's discovery slot arrives")
}

// TestSolanaSkippedRefExcludedFromDiff is the #720 correctness-critical
// proof: a live local subscription whose PDA was skipped this tick (due
// window, or discovery not run) is simply ABSENT from the snapshot — and an
// absent-from-snapshot solana subscription must never be read as
// "disappeared". Solana's traits.absenceMeansCancelled is false and its
// snapshots never claim SubscriptionsExhaustive coverage, so the diff must
// produce zero findings here.
func TestSolanaSkippedRefExcludedFromDiff(t *testing.T) {
	t.Parallel()

	require.False(t, traitsFor(ProviderSolana).absenceMeansCancelled,
		"solana absence must never be treated as proof of termination (a skipped ref would misreport as PS-2)")

	now := time.Now().UTC()
	local := &LocalState{
		Subscriptions: []LocalSubscription{{
			ID:                 uuid.New(),
			CustomerID:         uuid.New(),
			Status:             "active",
			Rail:               "solana",
			RailSubscriptionID: solanago.NewWallet().PublicKey().String(),
		}},
	}
	// This tick's snapshot carries no subscriptions at all — exactly what a
	// due-window skip (or a not-yet-run discovery pass) produces. Coverage is
	// left zero-value: real solana fetches never claim SubscriptionsExhaustive.
	snap := &RemoteSnapshot{
		Provider:     ProviderSolana,
		Capabilities: Capabilities{Subscriptions: true},
	}

	findings := diffProvider(ProviderSolana, snap, local, nil, now, diffOptions{})
	require.Empty(t, findings, "a ref skipped this tick must never be read as disappeared")
}
