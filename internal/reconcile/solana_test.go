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

	"github.com/open-rails/openrails"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

var usdcMint = solanago.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")

// fakeSolanaRPC is the chain boundary: canned accounts, signature pages,
// transactions and program accounts (memcmp filters applied byte-for-byte).
type fakeSolanaRPC struct {
	accounts            map[string][]byte
	signatures          map[string][]solanaint.SignatureInfo
	transactions        map[string]*solrpc.GetTransactionResult
	programAccounts     []solanaint.ProgramAccount
	pageCalls           int
	programAccountCalls int
}

// fakeMint6 is an initialized 6-decimal SPL mint account.
var fakeMint6 = func() []byte {
	b := make([]byte, solanaint.MintAccountSize)
	b[44], b[45] = 6, 1
	return b
}()

func (f *fakeSolanaRPC) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	if address.Equals(usdcMint) {
		return fakeMint6, nil
	}
	return f.accounts[address.String()], nil
}

func (f *fakeSolanaRPC) GetSignaturesForAddressPage(_ context.Context, address, before string, limit int) ([]solanaint.SignatureInfo, error) {
	f.pageCalls++
	sigs := f.signatures[address]
	start := 0
	for i := range sigs {
		if before != "" && sigs[i].Signature == before {
			start = i + 1
		}
	}
	if start >= len(sigs) {
		return nil, nil
	}
	end := len(sigs)
	if limit > 0 {
		end = min(end, start+limit)
	}
	return sigs[start:end], nil
}

func (f *fakeSolanaRPC) GetTransaction(_ context.Context, signature solanago.Signature) (*solrpc.GetTransactionResult, error) {
	if res, ok := f.transactions[signature.String()]; ok {
		return res, nil
	}
	return nil, errors.New("transaction not found")
}

func (f *fakeSolanaRPC) GetProgramAccounts(_ context.Context, _ solanago.PublicKey, filters []solanaint.ProgramAccountFilter) ([]solanaint.ProgramAccount, error) {
	f.programAccountCalls++
	var out []solanaint.ProgramAccount
	for _, acc := range f.programAccounts {
		match := true
		for _, flt := range filters {
			o := int(flt.Offset)
			match = match && o+len(flt.Bytes) <= len(acc.Data) && bytes.Equal(acc.Data[o:o+len(flt.Bytes)], flt.Bytes)
		}
		if match {
			out = append(out, acc)
		}
	}
	return out, nil
}

// planBlob is a Plan account in the on-chain layout (discriminator 1).
func planBlob(t *testing.T, mint solanago.PublicKey, amount, periodHours uint64, endTs int64) []byte {
	b := []byte{1}
	b = append(b, make([]byte, 32)...)
	b = append(b, 0xFE, 1)
	b = binary.LittleEndian.AppendUint64(b, 7)
	b = append(b, mint.Bytes()...)
	for _, v := range []uint64{amount, periodHours, 1700000000, uint64(endTs)} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	b = append(b, make([]byte, 4*32+4*32+128)...)
	require.Len(t, b, subscriptions.PlanAccountSize)
	return b
}

// subBlob is a v1 SubscriptionDelegation account (discriminator 4).
func subBlob(t *testing.T, delegator, delegatee solanago.PublicKey, amount, periodHours, pulled uint64, periodStart, expiresAt int64) []byte {
	b := []byte{4, 1, 0xFD}
	b = append(b, delegator.Bytes()...)
	b = append(b, delegatee.Bytes()...)
	b = append(b, make([]byte, 32)...)
	for _, v := range []uint64{9, amount, periodHours, 1700000000, pulled, uint64(periodStart), uint64(expiresAt)} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	require.Len(t, b, subscriptions.SubscriptionAccountSize)
	return b
}

func txResult(t *testing.T, meta string, ixs ...solanago.Instruction) *solrpc.GetTransactionResult {
	tx, err := solanago.NewTransaction(ixs, solanago.Hash{}, solanago.TransactionPayer(solanago.NewWallet().PublicKey()))
	require.NoError(t, err)
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	var res solrpc.GetTransactionResult
	require.NoError(t, json.Unmarshal([]byte(`{"slot":1,"transaction":["`+base64.StdEncoding.EncodeToString(raw)+`","base64"],"meta":`+meta+`}`), &res))
	return &res
}

const (
	okMeta     = `{"err":null}`
	failedMeta = `{"err":{"InstructionError":[0,{"Custom":1}]}}`
)

func tokenIntoWalletMeta(wallet, mint solanago.PublicKey, pre, post uint64) string {
	bal := func(amt uint64) string {
		return fmt.Sprintf(`[{"accountIndex":1,"mint":%q,"owner":%q,"uiTokenAmount":{"amount":"%d","decimals":6}}]`, mint, wallet, amt)
	}
	return `{"err":null,"preTokenBalances":` + bal(pre) + `,"postTokenBalances":` + bal(post) + `}`
}

func sigFromByte(b byte) string {
	return solanago.SignatureFromBytes(bytes.Repeat([]byte{b}, 64)).String()
}

func newKey() solanago.PublicKey { return solanago.NewWallet().PublicKey() }

func transferIx(sub, delegator, mint solanago.PublicKey, amount uint64) solanago.Instruction {
	return subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA: sub, PlanPDA: newKey(), SubscriptionAuthority: newKey(), DelegatorATA: newKey(), ReceiverATA: newKey(),
		Caller: newKey(), Mint: mint, TokenProgram: solanago.TokenProgramID, EventAuthority: newKey(), Amount: amount, Delegator: delegator,
	})
}

func staticRefs(refs ...SolanaSubscriptionRef) SolanaSubscriptionSource {
	return func(context.Context) ([]SolanaSubscriptionRef, error) { return refs, nil }
}

// discoverySlot finds an instant inside (due) or outside planPDA's #720 slot.
func discoverySlot(t *testing.T, planPDA string, due bool) time.Time {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < int(solanaDiscoveryCadence/solanaDiscoverySlotWidth); i++ {
		c := base.Add(time.Duration(i) * solanaDiscoverySlotWidth)
		if planDiscoveryDue(planPDA, c) == due {
			return c
		}
	}
	t.Fatalf("no slot with due=%v for plan %s", due, planPDA)
	return time.Time{}
}

// The chain's own declarations decide status; money is wire-pinned or absent.
func TestSolanaSubscriptionStatusDoctrine(t *testing.T) {
	now := time.Now().UTC()
	wallet, plan, endedPlan := newKey(), newKey(), newKey()
	active, pending, lapsed, ended := newKey(), newKey(), newKey(), newKey()
	periodStart := now.Add(-100 * time.Hour).Unix()
	rpc := &fakeSolanaRPC{accounts: map[string][]byte{
		active.String():    subBlob(t, wallet, plan, 9_990_000, 720, 9_990_000, periodStart, 0),
		pending.String():   subBlob(t, wallet, plan, 9_990_000, 720, 0, periodStart, now.Add(time.Hour).Unix()),
		lapsed.String():    subBlob(t, wallet, plan, 9_990_000, 720, 0, periodStart, now.Add(-time.Hour).Unix()),
		ended.String():     subBlob(t, wallet, endedPlan, 9_990_000, 720, 0, periodStart, 0),
		plan.String():      planBlob(t, usdcMint, 9_990_000, 720, 0),
		endedPlan.String(): planBlob(t, usdcMint, 9_990_000, 720, now.Add(-time.Hour).Unix()),
	}}
	var refs []SolanaSubscriptionRef
	for _, s := range []solanago.PublicKey{active, pending, lapsed} {
		refs = append(refs, SolanaSubscriptionRef{SubscriptionPDA: s.String(), PlanPDA: plan.String(), SubscriberWallet: wallet.String()})
	}
	refs = append(refs, SolanaSubscriptionRef{SubscriptionPDA: ended.String(), PlanPDA: endedPlan.String()})

	snap, err := (&SolanaFetcher{RPC: rpc, Source: staticRefs(refs...)}).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.False(t, snap.Coverage.SubscriptionsExhaustive, "#720: a skipped ref must never read as absent")
	require.Len(t, snap.Subscriptions, 4)

	a := snap.Subscriptions[0]
	require.Equal(t, []string{"active", "active", wallet.String(), plan.String(), "USD"}, []string{string(a.Status), a.RawStatus, a.CustomerID, a.PlanID, a.Currency})
	require.Equal(t, int64(999), a.AmountCents, "9_990_000 micro-USDC is exactly 999 cents")
	require.Equal(t, time.Unix(periodStart, 0).UTC().Add(720*time.Hour), *a.NextBillingAt)

	for i, want := range []struct {
		status SubscriptionStatus
		raw    string
	}{{SubscriptionStatusActive, "cancel_at_period_end"}, {SubscriptionStatusCancelled, "expires_at_passed"}, {SubscriptionStatusExpired, "plan_ended"}} {
		s := snap.Subscriptions[i+1]
		require.Equal(t, want.status, s.Status, want.raw)
		require.Equal(t, want.raw, s.RawStatus)
		if want.raw != "plan_ended" {
			require.Nil(t, s.NextBillingAt, "no billing follows a scheduled or passed expiry")
		}
	}
}

// FAB-4: subscribe/cancel are lifecycle events, never sales; sub-cent never rounds.
func TestSolanaSignatureClassification(t *testing.T) {
	sub, wallet := newKey(), newKey()
	cancelIx := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{Subscriber: wallet, PlanPDA: newKey(), SubscriptionPDA: sub, EventAuthority: newKey()})
	subscribeIx := subscriptions.BuildSubscribe(subscriptions.SubscribeParams{Subscriber: wallet, Merchant: newKey(), PlanPDA: newKey(), SubscriptionPDA: sub,
		SubscriptionAuthorityPDA: newKey(), EventAuthority: newKey(), ExpectedMint: usdcMint})
	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{sub.String(): {0x02}},
		signatures: map[string][]solanaint.SignatureInfo{sub.String(): {
			{Signature: sigFromByte(1)}, {Signature: sigFromByte(2)}, {Signature: sigFromByte(3)},
			{Signature: sigFromByte(4)}, {Signature: sigFromByte(5), HasError: true}, {Signature: sigFromByte(6)},
		}},
		transactions: map[string]*solrpc.GetTransactionResult{
			sigFromByte(1): txResult(t, okMeta, transferIx(sub, wallet, usdcMint, 9_990_000)),
			sigFromByte(2): txResult(t, okMeta, transferIx(sub, wallet, usdcMint, 1_234_567)),
			sigFromByte(3): txResult(t, okMeta, subscribeIx),
			sigFromByte(4): txResult(t, okMeta, cancelIx),
			sigFromByte(5): txResult(t, failedMeta, transferIx(sub, wallet, usdcMint, 9_990_000)),
		},
	}
	snap, err := (&SolanaFetcher{RPC: rpc, Source: staticRefs(SolanaSubscriptionRef{SubscriptionPDA: sub.String(), SubscriberWallet: wallet.String()})}).
		Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Transactions, 6)

	pull, odd, subscribe, cancel, failed, unreadable := snap.Transactions[0], snap.Transactions[1], snap.Transactions[2], snap.Transactions[3], snap.Transactions[4], snap.Transactions[5]
	require.Equal(t, TransactionTypeSale, pull.Type)
	require.True(t, pull.Success)
	require.Equal(t, int64(999), pull.AmountCents)
	require.Equal(t, "USD", pull.Currency)
	for _, s := range []string{`"amount_base_units":"9990000"`, usdcMint.String(), "transfer_subscription"} {
		require.Contains(t, string(pull.Raw), s)
	}
	require.Zero(t, odd.AmountCents, "1_234_567 micros is sub-cent: kept in Raw, never rounded")
	require.Empty(t, odd.Currency)
	require.Contains(t, string(odd.Raw), `"amount_base_units":"1234567"`)
	require.Equal(t, TransactionTypeSubscribe, subscribe.Type)
	require.Zero(t, subscribe.AmountCents)
	require.Equal(t, TransactionTypeCancel, cancel.Type)
	require.Equal(t, TransactionTypeDecline, failed.Type)
	require.False(t, failed.Success)
	require.Contains(t, failed.DeclineReason, "on-chain failure")
	require.Equal(t, TransactionTypeSale, unreadable.Type)
	require.Contains(t, string(unreadable.Raw), "signature_only")
}

// #714: the merchant-wallet scan recognizes OUR memos only, parks anything it
// cannot fully verify, and walks a bounded number of signatures.
func TestSolanaWalletScan(t *testing.T) {
	wallet, pullSub := newKey(), newKey()
	customerID, priceID := uuid.New(), uuid.New()
	cleanID, mismatchID, dupID, noneID, valuelessID, failedID, pullIntentID, oldID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	session := func(amount uint64) *SolanaLocalRecord {
		return &SolanaLocalRecord{Kind: SolanaLocalKindCheckoutSession, Rail: "solana", CustomerID: customerID, PriceID: priceID,
			ExpectedRecipient: wallet.String(), ExpectedMint: usdcMint.String(), ExpectedTokenAmount: amount}
	}
	records := map[uuid.UUID]*SolanaLocalRecord{
		cleanID: session(9_990_000), mismatchID: session(5_000_000), dupID: session(9_990_000), failedID: session(9_990_000), oldID: session(9_990_000),
		pullIntentID: {Kind: SolanaLocalKindPullIntent, Rail: "solana", SubscriptionPDA: pullSub.String()},
	}
	resolve := func(_ context.Context, id uuid.UUID) (*SolanaLocalRecord, error) { return records[id], nil }
	memo := func(id uuid.UUID) solanago.Instruction {
		return solanaint.NewMemoInstruction(solanaint.PurchaseMemo(id))
	}
	paid := tokenIntoWalletMeta(wallet, usdcMint, 0, 9_990_000)

	since, until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	inWindow := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	type scanned struct {
		tx      *solrpc.GetTransactionResult
		at      time.Time
		failed  bool
		present bool
		typ     TransactionType
		cents   int64
		verdict string
		park    string
	}
	scan := map[string]scanned{
		"no memo":        {tx: txResult(t, paid, solanaint.NewMemoInstruction("gm"))},
		"foreign memo":   {tx: txResult(t, paid, solanaint.NewMemoInstruction("otherapp:1:"+uuid.New().String()))},
		"clean one-off":  {tx: txResult(t, paid, memo(cleanID)), present: true, typ: TransactionTypeSale, cents: 999, verdict: "clean"},
		"amount differs": {tx: txResult(t, paid, memo(mismatchID)), present: true, typ: TransactionTypeSale, cents: 999, verdict: "park", park: "disagrees with the session's quoted"},
		"duplicate a":    {tx: txResult(t, paid, memo(dupID)), present: true, typ: TransactionTypeSale, cents: 999, verdict: "park", park: "claimed by 2 successful transactions"},
		"duplicate b":    {tx: txResult(t, paid, memo(dupID)), present: true, typ: TransactionTypeSale, cents: 999, verdict: "park", park: "claimed by 2 successful transactions"},
		"no local":       {tx: txResult(t, paid, memo(noneID)), present: true, typ: TransactionTypeSale, cents: 999, verdict: "park", park: "no local record"},
		"valueless":      {tx: txResult(t, okMeta, memo(valuelessID))},
		"failed":         {tx: txResult(t, failedMeta, memo(failedID)), failed: true, present: true, typ: TransactionTypeDecline},
		"pull":           {tx: txResult(t, paid, transferIx(pullSub, newKey(), usdcMint, 9_990_000), memo(pullIntentID)), present: true, typ: TransactionTypeSale, cents: 999, verdict: "clean"},
		"below since":    {tx: txResult(t, paid, memo(oldID)), at: since.Add(-time.Hour)},
	}
	order := []string{"no memo", "foreign memo", "clean one-off", "amount differs", "duplicate a", "duplicate b", "no local", "valueless", "failed", "pull", "below since"}
	sigOf := map[string]string{}
	rpc := &fakeSolanaRPC{signatures: map[string][]solanaint.SignatureInfo{}, transactions: map[string]*solrpc.GetTransactionResult{}}
	for i, name := range order {
		c := scan[name]
		sig := sigFromByte(byte(i + 1))
		sigOf[name] = sig
		at := inWindow
		if !c.at.IsZero() {
			at = c.at
		}
		rpc.signatures[wallet.String()] = append(rpc.signatures[wallet.String()], solanaint.SignatureInfo{Signature: sig, HasError: c.failed, BlockTime: &at})
		rpc.transactions[sig] = c.tx
	}
	snap, err := (&SolanaFetcher{RPC: rpc, Source: staticRefs(), Resolve: resolve, MerchantWallet: wallet.String()}).
		Fetch(context.Background(), FetchParams{Since: since, Until: until})
	require.NoError(t, err)
	byID := map[string]RemoteTransaction{}
	for _, txn := range snap.Transactions {
		byID[txn.TransactionID] = txn
	}
	for _, name := range order {
		c := scan[name]
		txn, ok := byID[sigOf[name]]
		require.Equal(t, c.present, ok, name)
		if !ok {
			continue
		}
		require.Equal(t, c.typ, txn.Type, name)
		require.Equal(t, c.cents, txn.AmountCents, name)
		d := decodeSolanaDiscovery(txn.Raw)
		if c.verdict == "" {
			require.Nil(t, d, name)
			continue
		}
		require.Equal(t, c.verdict, d.Verdict, name)
		require.Contains(t, d.ParkReason, c.park, name)
	}
	clean := decodeSolanaDiscovery(byID[sigOf["clean one-off"]].Raw)
	require.Equal(t, []string{solanaDiscoveryKindOneOff, customerID.String(), priceID.String(), cleanID.String()}, []string{clean.Kind, clean.CustomerID, clean.PriceID, clean.MemoLocalID})
	require.Equal(t, "USD", byID[sigOf["clean one-off"]].Currency)
	require.Equal(t, inWindow, byID[sigOf["clean one-off"]].OccurredAt)
	require.Empty(t, decodeSolanaDiscovery(byID[sigOf["duplicate a"]].Raw).CustomerID, "duplicates never keep a backfillable identity")
	pull := decodeSolanaDiscovery(byID[sigOf["pull"]].Raw)
	require.Equal(t, solanaDiscoveryKindPull, pull.Kind)
	require.Empty(t, pull.CustomerID)
	require.Equal(t, pullSub.String(), byID[sigOf["pull"]].SubscriptionID)
	require.Contains(t, string(byID[sigOf["failed"]].Raw), failedID.String(), "failed attempts keep the memo for forensics")

	t.Run("the walk is capped", func(t *testing.T) {
		for _, c := range []struct {
			cap, pages int
			found      bool
		}{{3, 2, false}, {10, 3, true}} {
			var sigs []solanaint.SignatureInfo
			for b := byte(1); b <= 4; b++ {
				sigs = append(sigs, solanaint.SignatureInfo{Signature: sigFromByte(b), BlockTime: &inWindow})
			}
			rpc := &fakeSolanaRPC{signatures: map[string][]solanaint.SignatureInfo{wallet.String(): sigs},
				transactions: map[string]*solrpc.GetTransactionResult{sigFromByte(4): txResult(t, paid, memo(cleanID))}}
			snap, err := (&SolanaFetcher{RPC: rpc, Source: staticRefs(), Resolve: resolve, MerchantWallet: wallet.String(),
				WalletScanPageSize: 2, WalletScanCap: c.cap}).Fetch(context.Background(), FetchParams{})
			require.NoError(t, err)
			require.Equal(t, c.found, len(snap.Transactions) == 1, "cap %d", c.cap)
			require.LessOrEqual(t, rpc.pageCalls, c.pages, "cap %d", c.cap)
		}
	})
}

// #714 scan 2 + #720 cadence: subscriptions under OUR plans the mirror does not
// know are enumerated on the plan's slow discovery slot only.
func TestSolanaPlanEnumeration(t *testing.T) {
	known, stranger, plan, otherPlan := newKey(), newKey(), newKey(), newKey()
	knownSub, discoveredSub := newKey(), newKey()
	periodStart := time.Now().Add(-100 * time.Hour).Unix()
	knownBlob := subBlob(t, known, plan, 9_990_000, 720, 0, periodStart, 0)
	rpc := &fakeSolanaRPC{
		accounts: map[string][]byte{knownSub.String(): knownBlob, plan.String(): planBlob(t, usdcMint, 9_990_000, 720, 0)},
		programAccounts: []solanaint.ProgramAccount{
			{Address: knownSub, Data: knownBlob},
			{Address: discoveredSub, Data: subBlob(t, stranger, plan, 9_990_000, 720, 0, periodStart, 0)},
			{Address: newKey(), Data: subBlob(t, stranger, otherPlan, 9_990_000, 720, 0, periodStart, 0)},
		},
	}
	fetcher := &SolanaFetcher{RPC: rpc,
		Source: staticRefs(SolanaSubscriptionRef{SubscriptionPDA: knownSub.String(), PlanPDA: plan.String(), SubscriberWallet: known.String()}),
		Plans:  func(context.Context) ([]string, error) { return []string{plan.String()}, nil }}

	notDue := discoverySlot(t, plan.String(), false)
	fetcher.Now = func() time.Time { return notDue }
	snap, err := fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Zero(t, rpc.programAccountCalls, "enumeration waits for the plan's discovery slot")
	require.Len(t, snap.Subscriptions, 1)

	due := discoverySlot(t, plan.String(), true)
	fetcher.Now = func() time.Time { return due }
	snap, err = fetcher.Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Equal(t, 1, rpc.programAccountCalls)
	require.Len(t, snap.Subscriptions, 2, "known once, discovered once, foreign plan filtered")
	require.NotContains(t, string(snap.Subscriptions[0].Raw), "discovered_not_local")
	d := snap.Subscriptions[1]
	require.Equal(t, []string{discoveredSub.String(), stranger.String(), plan.String(), "active", "USD"}, []string{d.RailSubscriptionID, d.CustomerID, d.PlanID, string(d.Status), d.Currency})
	require.Equal(t, int64(999), d.AmountCents)
	require.Contains(t, string(d.Raw), `"discovered_not_local":true`)
	require.Contains(t, string(d.Raw), `"source":"solana_program_scan"`)

	t.Run("a narrowed probe bypasses the due window", func(t *testing.T) {
		target := newKey()
		f := &SolanaFetcher{RPC: &fakeSolanaRPC{accounts: map[string][]byte{target.String(): {0x02}}},
			Source: staticRefs(SolanaSubscriptionRef{SubscriptionPDA: target.String()}),
			Due:    func(context.Context, time.Time) (map[string]struct{}, error) { return map[string]struct{}{}, nil },
			Now:    func() time.Time { return due }}
		snap, err := f.Fetch(context.Background(), FetchParams{SubscriptionID: target.String()})
		require.NoError(t, err)
		require.Len(t, snap.Subscriptions, 1)
		require.Equal(t, target.String(), snap.Subscriptions[0].RailSubscriptionID)
	})
}

func TestWalletTransferMoney(t *testing.T) {
	wallet, other := newKey(), newKey()
	tb := func(idx uint16, owner, mint solanago.PublicKey, amt uint64) solrpc.TokenBalance {
		return solrpc.TokenBalance{AccountIndex: idx, Mint: mint, Owner: &owner, UiTokenAmount: &solrpc.UiTokenAmount{Amount: fmt.Sprint(amt)}}
	}
	got, ok, _ := walletTransferMoney(&solrpc.TransactionMeta{
		PreTokenBalances:  []solrpc.TokenBalance{tb(1, wallet, usdcMint, 10_000)},
		PostTokenBalances: []solrpc.TokenBalance{tb(1, wallet, usdcMint, 10_000_000)},
	}, nil, wallet)
	require.True(t, ok)
	require.Equal(t, walletTransfer{Mint: usdcMint.String(), BaseUnits: 9_990_000}, got)

	payer := newKey()
	tx, err := solanago.NewTransaction([]solanago.Instruction{system.NewTransferInstruction(5_000, payer, wallet).Build()}, solanago.Hash{}, solanago.TransactionPayer(payer))
	require.NoError(t, err)
	pre, post := make([]uint64, len(tx.Message.AccountKeys)), make([]uint64, len(tx.Message.AccountKeys))
	for i, key := range tx.Message.AccountKeys {
		if key.Equals(wallet) {
			pre[i], post[i] = 100, 5_100
		}
	}
	got, ok, _ = walletTransferMoney(&solrpc.TransactionMeta{PreBalances: pre, PostBalances: post}, tx, wallet)
	require.True(t, ok)
	require.Equal(t, walletTransfer{Mint: "", BaseUnits: 5_000}, got, "native SOL: lamports at the wallet's key")

	for note, meta := range map[string]*solrpc.TransactionMeta{
		"no transfer into the merchant wallet": {PostTokenBalances: []solrpc.TokenBalance{tb(1, other, usdcMint, 9_990_000)}},
		"multiple assets":                      {PostTokenBalances: []solrpc.TokenBalance{tb(1, wallet, usdcMint, 100), tb(2, wallet, other, 5)}},
		"exceeds representable range":          {PostTokenBalances: []solrpc.TokenBalance{tb(1, wallet, usdcMint, math.MaxInt64+1)}},
	} {
		_, ok, got := walletTransferMoney(meta, nil, wallet)
		require.False(t, ok, note)
		require.Contains(t, got, note)
	}
	_, ok, _ = walletTransferMoney(nil, nil, wallet)
	require.False(t, ok)
}

// #817: the shift is the mint's decimals; only registry USD stablecoins at
// exactly cent-representable values produce fiat.
func TestSolanaFiatCents(t *testing.T) {
	usdc := usdcMint.String()
	for _, c := range []struct {
		mint     string
		decimals int
		base     uint64
		cents    int64
		ok       bool
	}{
		{usdc, 6, 1_000_000, 100, true}, {usdc, 6, 10_000, 1, true}, {usdc, 6, 9_990_000, 999, true},
		{usdc, 6, 1_234_567, 0, false}, {usdc, 6, 9_999, 0, false}, {usdc, 9, 1_000_000_000, 100, true},
		{usdc, 9, 1_000_000, 0, false}, {usdc, 2, 100, 100, true}, {usdc, 0, 1, 0, false},
		{usdc, 19, 1, 0, false}, {usdc, 2, math.MaxUint64, 0, false},
		{"USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB", 6, 5_000_000, 500, true},
		{"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", 6, 5_000_000, 500, true},
		{"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", 6, 5_000_000, 500, true},
		{"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", 6, 1_000_000, 100, true},
		{"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", 6, 1_000_000, 0, false}, // devnet USDC: not registry
		{"So11111111111111111111111111111111111111112", 9, 1_000_000_000, 0, false},
		{"", 6, 1_000_000, 0, false},
	} {
		cents, ok := solanaFiatCents(c.mint, c.decimals, c.base)
		require.Equal(t, c.ok, ok, "%s/%d/%d", c.mint, c.decimals, c.base)
		require.Equal(t, c.cents, cents, "%s/%d/%d", c.mint, c.decimals, c.base)
	}
}

func TestDiffSolanaDiscoveries(t *testing.T) {
	now := time.Now().UTC()
	customerID, priceID, sessionID := uuid.New(), uuid.New(), uuid.New()

	t.Run("clean one-offs backfill, everything unverifiable goes to an operator", func(t *testing.T) {
		env := func(d *solanaDiscovery, extra map[string]any) json.RawMessage {
			raw := map[string]any{"solana_discovery": d}
			for k, v := range extra {
				raw[k] = v
			}
			return rawJSON(raw)
		}
		clean := &solanaDiscovery{Verdict: "clean", Kind: solanaDiscoveryKindOneOff, MemoLocalID: sessionID.String(), LocalKind: SolanaLocalKindCheckoutSession, CustomerID: customerID.String(), PriceID: priceID.String()}
		parked := &solanaDiscovery{Verdict: "park", ParkReason: "transfer of 1 base units disagrees with the session's quoted 2", Kind: solanaDiscoveryKindOneOff, MemoLocalID: uuid.New().String(), LocalKind: SolanaLocalKindCheckoutSession}
		unpriced := &solanaDiscovery{Verdict: "clean", Kind: solanaDiscoveryKindOneOff, MemoLocalID: uuid.New().String(), LocalKind: SolanaLocalKindCheckoutSession, CustomerID: customerID.String(), PriceID: priceID.String()}
		pull := &solanaDiscovery{Verdict: "clean", Kind: solanaDiscoveryKindPull, MemoLocalID: uuid.New().String(), LocalKind: SolanaLocalKindPullIntent}
		sale := func(sig string, cents int64, raw json.RawMessage) RemoteTransaction {
			txn := RemoteTransaction{TransactionID: sig, Type: TransactionTypeSale, Success: true, AmountCents: cents, OccurredAt: now, Raw: raw}
			if cents > 0 {
				txn.Currency = "USD"
			}
			return txn
		}
		pullTxn := sale(sigFromByte(5), 999, env(pull, nil))
		pullTxn.SubscriptionID = newKey().String()
		snap := &RemoteSnapshot{Provider: ProviderSolana, Capabilities: Capabilities{Subscriptions: true, Transactions: true}, Transactions: []RemoteTransaction{
			sale(sigFromByte(1), 999, env(clean, nil)),
			sale(sigFromByte(2), 999, env(parked, nil)),
			sale(sigFromByte(3), 0, env(unpriced, map[string]any{"lamports": uint64(5_000)})),
			sale(sigFromByte(4), 999, env(clean, nil)),
			pullTxn,
		}}
		recorded := []LocalPayment{{ID: uuid.New(), Rail: "solana", TransactionID: sigFromByte(4), AmountCents: 999, Status: "completed"}}
		findings := diffProvider(ProviderSolana, snap, &LocalState{}, recorded, now, diffOptions{})
		require.Len(t, findings, 4, "an already-recorded payment produces nothing")
		by := map[string]Finding{}
		for _, f := range findings {
			by[f.SubjectKey] = f
		}

		fc := by[sigFromByte(1)]
		require.Equal(t, FindingChargeMissingLocal, fc.Type)
		require.Equal(t, FindingStatusReconcileRequired, fc.Status)
		require.False(t, fc.RequiresAdmin)
		b := fc.Apply.BackfillPayment
		require.Equal(t, "solana", b.Rail)
		require.Equal(t, int64(999), b.AmountCents, "money from the transfer")
		require.Equal(t, "USD", b.Currency, "CUR-6 canonical upper case")
		require.Equal(t, customerID, b.CustomerID)
		require.Equal(t, priceID, b.PriceID)
		require.Equal(t, openrails.CheckoutSessionID(sessionID).String(), fc.LocalEvidence["checkout_session_id"])
		require.Equal(t, "purchase_memo", fc.LocalEvidence["correlated_via"])

		for sig, reason := range map[string]string{sigFromByte(2): "disagrees", sigFromByte(3): "unpriced"} {
			f := by[sig]
			require.Equal(t, FindingStatusAdminRequired, f.Status, reason)
			require.True(t, f.RequiresAdmin, reason)
			require.Nil(t, f.Apply, reason)
			require.Contains(t, f.RemoteEvidence["park_reason"], reason)
		}
		require.Contains(t, by[sigFromByte(2)].RecommendedAction, "verify-not-decline")
		fp := by[sigFromByte(5)]
		require.Equal(t, FindingChargeMissingLocal, fp.Type, "an unknown pull takes the generic lane")
		require.Equal(t, FindingStatusAdminRequired, fp.Status)
		require.Nil(t, fp.Apply, "no local identity is ever guessed")
	})

	t.Run("a chain-discovered subscription never materializes", func(t *testing.T) {
		wallet, plan, subPDA := newKey().String(), newKey().String(), newKey().String()
		local := &LocalState{
			PaymentMethods: []LocalPaymentMethod{{ID: uuid.New(), CustomerID: customerID, Rail: "solana", RailCustomerRef: wallet}},
			Prices: []LocalPrice{{ID: priceID, ProductID: uuid.New(), Amount: 9_990_000, Currency: "USD",
				PSPLinks: map[string]map[string]string{"solana": {"rail": "solana", "plan_pda": plan, "provider": "solana"}}}},
		}
		diff := func(raw map[string]any) Finding {
			snap := &RemoteSnapshot{Provider: ProviderSolana, Capabilities: Capabilities{Subscriptions: true, Transactions: true},
				Subscriptions: []RemoteSubscription{{RailSubscriptionID: subPDA, Status: SubscriptionStatusActive, CustomerID: wallet, PlanID: plan, Raw: rawJSON(raw)}}}
			findings := diffProvider(ProviderSolana, snap, local, nil, now, diffOptions{Materialize: true})
			require.Len(t, findings, 1)
			require.Equal(t, FindingRemoteSubMissingLocal, findings[0].Type)
			return findings[0]
		}
		require.NotNil(t, diff(map[string]any{"source": "solana_subscription_account"}).Apply.Materialize, "control: identity and plan resolve")
		f := diff(map[string]any{"discovered_not_local": true})
		require.Equal(t, FindingStatusAdminRequired, f.Status)
		require.True(t, f.RequiresAdmin)
		require.Nil(t, f.Apply)
		require.Contains(t, f.RemoteEvidence["materialize_blocked"], "#714")
	})

	t.Run("a ref skipped this tick is never read as disappeared", func(t *testing.T) {
		require.False(t, traitsFor(ProviderSolana).absenceMeansCancelled)
		local := &LocalState{Subscriptions: []LocalSubscription{{ID: uuid.New(), CustomerID: uuid.New(), Status: "active", Rail: "solana", RailSubscriptionID: newKey().String()}}}
		snap := &RemoteSnapshot{Provider: ProviderSolana, Capabilities: Capabilities{Subscriptions: true}}
		require.Empty(t, diffProvider(ProviderSolana, snap, local, nil, now, diffOptions{}))
	})
}
