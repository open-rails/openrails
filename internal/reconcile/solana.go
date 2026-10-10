package reconcile

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	solrpc "github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/providerrecovery"
)

// Solana on-chain subscription lifecycle events (no money movement): the
// subscription PDA's history interleaves pulls with lifecycle instructions,
// and classifying a subscribe/cancel as a "sale" would fabricate payment
// evidence. The money paths (diff PS-4/PS-5, decider, forensics) ignore these
// types by construction.
const (
	TransactionTypeSubscribe TransactionType = "subscribe"
	TransactionTypeCancel    TransactionType = "cancel"
	TransactionTypeResume    TransactionType = "resume"
	TransactionTypeRevoke    TransactionType = "revoke"
)

// SolanaSubscriptionRef identifies one locally-known on-chain subscription:
// the subscription PDA (which is also the local
// subscriptions.rail_subscription_id for solana memberships) and its
// plan PDA. The caller supplies these from billing.solana_subscriptions.
type SolanaSubscriptionRef struct {
	SubscriptionPDA  string
	PlanPDA          string
	SubscriberWallet string
}

// SolanaSubscriptionSource lists the locally-known solana subscriptions whose
// on-chain accounts the fetcher should read.
type SolanaSubscriptionSource func(ctx context.Context) ([]SolanaSubscriptionRef, error)

// SolanaPlanSource lists our plan PDAs (locally-known subscriptions plus the
// catalog's solana plan_pda links) for the permissionless-subscriber
// enumeration. Nil disables that scan.
type SolanaPlanSource func(ctx context.Context) ([]string, error)

// SolanaDueSubscriptionSource returns the locally-known subscription PDAs
// whose next_pull_at is at or before `before`, answered server-side so the
// read stays due-proportional. Only the routine bulk fetch consults it; Source
// stays exhaustive because narrowed probes and the discovery de-dup set need
// every known ref. Nil disables due-window filtering (every ref every tick).
type SolanaDueSubscriptionSource func(ctx context.Context, before time.Time) (map[string]struct{}, error)

// SolanaLocalRecord kinds: the two record types a memo is stamped for.
const (
	SolanaLocalKindCheckoutAttempt = "checkout_attempt"
	SolanaLocalKindPullIntent      = "pull_intent"
)

// SolanaLocalRecord is what a memo local-id resolves to locally. The
// expected* fields are the checkout attempt's bound quote, which the wallet
// scan verifies the on-chain transfer against (a mismatch parks the finding).
type SolanaLocalRecord struct {
	Kind string // SolanaLocalKindCheckoutAttempt | SolanaLocalKindPullIntent
	Rail string

	// checkout_attempt fields.
	CustomerID           uuid.UUID
	PriceID              uuid.UUID
	SessionStatus        string
	SettledTransactionID string // session.transaction_id once succeeded
	ExpectedRecipient    string
	ExpectedMint         string // "" / wrapped-SOL mint = native SOL
	ExpectedTokenAmount  uint64 // token base units bound at quote time

	// pull_intent fields.
	SubscriptionPDA string
}

// SolanaLocalRecordResolver resolves a memo local-id against local records.
// (nil, nil) = no local record with that id exists.
type SolanaLocalRecordResolver func(ctx context.Context, localID uuid.UUID) (*SolanaLocalRecord, error)

// solanaRPC is the read slice of *solanaint.RPCClient the fetcher uses.
type solanaRPC interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	GetSignaturesForAddressPage(ctx context.Context, address, before string, limit int) ([]solanaint.SignatureInfo, error)
	GetTransaction(ctx context.Context, signature solanago.Signature) (*solrpc.GetTransactionResult, error)
	GetProgramAccounts(ctx context.Context, program solanago.PublicKey, filters []solanaint.ProgramAccountFilter) ([]solanaint.ProgramAccount, error)
}

// SolanaFetcher reports the on-chain state of the merchant's Solana rail:
// subscriptions and transactions only (no refunds, chargebacks or vault).
// Three lanes:
//
//  1. Locally-known subscription PDAs (Source): account decode plus per-PDA
//     signature classification. The routine bulk fetch reads only the
//     due-window cohort (Due): a cancel/resume has no billing effect before
//     the period boundary. A narrowed probe always reads.
//  2. Per-plan enumeration (Plans): `subscribe` is permissionless, so
//     subscriptions under our plans are enumerated via getProgramAccounts;
//     discovered-not-local ones are marked in Raw and never auto-create
//     billing state. Its cost scales with subscribers, so it runs on a slow
//     per-plan cadence (planDiscoveryDue).
//  3. Merchant-wallet scan: the wallet's signature history within
//     Since/Until. Only transactions carrying an `openrails:` memo count, and
//     the memo is a discovery hint, never money truth: money comes from the
//     transfer (pull instruction data or the balance delta), kind from tx
//     shape, and the local-id must resolve consistently and uniquely. Any
//     mismatch or duplicate parks the finding (verify-not-decline).
//
// Normalization:
//   - Status comes from the decoded SubscriptionDelegation account
//     (expires_at_ts 0 => active; future => cancel-at-period-end, normalized
//     active like Stripe; past => canceled). A missing PDA is canceled (revoke
//     closes it). Undecodable bytes fall back to presence inference; the raw
//     bytes stay in Raw either way.
//   - Transactions classify by instruction discriminator: pulls => sale or
//     decline with the instruction's amount; subscribe/cancel/resume/revoke =>
//     lifecycle types. An unfetchable tx falls back to success=>sale /
//     failure=>decline with a note, never an error that kills the window.
//   - Amounts are mint base units, normalized to AmountCents only for
//     registry USD stablecoins at exactly cent-representable values;
//     otherwise AmountCents is 0 and base units stay in Raw. Never rounded.
//   - These are bulk point-in-time reads, not post-tx reads, so plain reads
//     (no *AtSlot / ReadUntilConsistent) are correct.
type SolanaFetcher struct {
	RPC    solanaRPC
	Source SolanaSubscriptionSource
	// Plans feeds the per-plan subscription enumeration, which runs on a slow
	// cadence (planDiscoveryDue). Nil disables it.
	Plans SolanaPlanSource
	// Due feeds the due-window filter for the routine bulk fetch. Nil reads
	// every locally-known ref every tick; narrowed probes always bypass it.
	Due SolanaDueSubscriptionSource
	// DueWindowLead extends the due window this far before a subscription's
	// local next_pull_at, so reconcile sees it in the cycle it could next be
	// pulled (default 4h, above the provider refresh interval). Past-due refs
	// stay in-window every tick.
	DueWindowLead time.Duration
	// Resolve resolves memo local-ids against local records for the wallet
	// scan. Nil parks every memo-recognized discovery (unverifiable).
	Resolve SolanaLocalRecordResolver
	// MerchantWallet is the PSP's account_id (the receiving wallet) the
	// wallet scan walks. FetchParams.AccountID overrides.
	MerchantWallet string
	// SignatureLimit bounds the per-subscription signature listing (default 50).
	SignatureLimit int
	// mintDecimalsCache memoizes on-chain mint decimals for this fetcher's
	// lifetime; mint decimals are immutable.
	mintDecimalsCache map[string]int
	// WalletScanPageSize / WalletScanCap bound the wallet signature walk
	// (defaults 200 / 1000 signatures per window).
	WalletScanPageSize int
	WalletScanCap      int
	// Now returns the current time; overridable in tests (nil uses time.Now).
	Now func() time.Time
}

// NewSolanaFetcher builds a fetcher over the shared RPC client and a local
// subscription source.
func NewSolanaFetcher(rpc *solanaint.RPCClient, source SolanaSubscriptionSource) *SolanaFetcher {
	return &SolanaFetcher{RPC: rpc, Source: source}
}

func (f *SolanaFetcher) Name() string { return string(ProviderSolana) }

func (f *SolanaFetcher) Capabilities() Capabilities {
	return Capabilities{
		Subscriptions: true,
		Transactions:  true,
		Refunds:       false,
		Chargebacks:   false,
		Vault:         false,
	}
}

func (f *SolanaFetcher) signatureLimit() int {
	if f.SignatureLimit > 0 {
		return f.SignatureLimit
	}
	return 50
}

func (f *SolanaFetcher) walletScanPageSize() int {
	if f.WalletScanPageSize > 0 {
		return f.WalletScanPageSize
	}
	return 200
}

func (f *SolanaFetcher) walletScanCap() int {
	if f.WalletScanCap > 0 {
		return f.WalletScanCap
	}
	return 1000
}

func (f *SolanaFetcher) dueWindowLead() time.Duration {
	if f.DueWindowLead > 0 {
		return f.DueWindowLead
	}
	return 4 * time.Hour
}

func (f *SolanaFetcher) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

func (f *SolanaFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	now := f.now()
	if !params.ObservedAt.IsZero() {
		now = params.ObservedAt.UTC()
	}
	snap := &RemoteSnapshot{
		Provider:     ProviderSolana,
		FetchedAt:    now,
		Capabilities: f.Capabilities(),
	}

	refs, err := f.Source(ctx)
	if err != nil {
		return nil, fmt.Errorf("solana subscription source: %w", err)
	}

	// Plan accounts are shared across subscribers; decode each once.
	planCache := map[string]*subscriptions.PlanAccount{}
	// A narrowed fetch (one subscription/customer) always reads, bypassing
	// the due-window filter.
	narrowed := params.SubscriptionID != "" || params.CustomerID != ""

	// Due window: bound the routine bulk fetch's chain reads to subs near, at
	// or past their local next_pull_at. dueSet == nil means no filtering (Due
	// not wired, or a narrowed probe).
	var dueSet map[string]struct{}
	if f.Due != nil && !narrowed {
		dueSet, err = f.Due(ctx, now.Add(f.dueWindowLead()))
		if err != nil {
			return nil, fmt.Errorf("solana due-subscription source: %w", err)
		}
	}

	known := make(map[string]struct{}, len(refs))
	emittedSigs := map[string]struct{}{}
	for _, ref := range refs {
		// Marked known before the due-window skip: a ref not read this tick is
		// still locally-known, so the discovery lane must not re-surface it as
		// discovered_not_local.
		known[ref.SubscriptionPDA] = struct{}{}
		if params.SubscriptionID != "" && ref.SubscriptionPDA != params.SubscriptionID {
			continue
		}
		if params.CustomerID != "" && ref.SubscriberWallet != params.CustomerID {
			continue
		}
		if dueSet != nil {
			if _, ok := dueSet[ref.SubscriptionPDA]; !ok {
				continue // outside the due window this tick
			}
		}

		sub, err := f.fetchSubscription(ctx, ref, planCache, now)
		if err != nil {
			return nil, err
		}
		snap.Subscriptions = append(snap.Subscriptions, sub)

		txns, err := f.fetchSignatures(ctx, ref, params)
		if err != nil {
			return nil, err
		}
		snap.Transactions = append(snap.Transactions, txns...)
		for i := range txns {
			emittedSigs[txns[i].TransactionID] = struct{}{}
		}
	}

	// Discovery scans are bulk lanes; a narrowed fetch (per-subscription or
	// per-customer probe) skips them.
	if !narrowed {
		if f.Plans != nil {
			discovered, err := f.enumeratePlanSubscriptions(ctx, known, planCache, now)
			if err != nil {
				return nil, err
			}
			snap.Subscriptions = append(snap.Subscriptions, discovered...)
		}
		txns, err := f.scanMerchantWallet(ctx, params, emittedSigs)
		if err != nil {
			return nil, err
		}
		snap.Transactions = append(snap.Transactions, txns...)
	}

	return snap, nil
}

// maxSanePeriodHours bounds on-chain period_hours before it is turned into a
// time.Duration (100 years; a corrupt value must not overflow the math).
const maxSanePeriodHours = 24 * 366 * 100

func (f *SolanaFetcher) fetchSubscription(ctx context.Context, ref SolanaSubscriptionRef, planCache map[string]*subscriptions.PlanAccount, now time.Time) (RemoteSubscription, error) {
	subPDA, err := solanago.PublicKeyFromBase58(ref.SubscriptionPDA)
	if err != nil {
		return RemoteSubscription{}, fmt.Errorf("solana: invalid subscription PDA %q: %w", ref.SubscriptionPDA, err)
	}

	data, err := f.RPC.GetAccountData(ctx, subPDA)
	if err != nil {
		return RemoteSubscription{}, fmt.Errorf("solana: read subscription account %s: %w", ref.SubscriptionPDA, err)
	}

	raw := map[string]any{
		"source":              "solana_subscription_account",
		"subscription_pda":    ref.SubscriptionPDA,
		"plan_pda":            ref.PlanPDA,
		"subscriber_wallet":   ref.SubscriberWallet,
		"account_exists":      len(data) > 0,
		"account_data_base64": base64.StdEncoding.EncodeToString(data),
	}

	sub := RemoteSubscription{
		RailSubscriptionID: ref.SubscriptionPDA,
		CustomerID:         ref.SubscriberWallet,
		PlanID:             ref.PlanPDA,
	}
	if len(data) == 0 {
		// The program closes the subscription account on revoke; absence of a
		// once-known account is the on-chain canceled state.
		sub.Status = SubscriptionStatusCanceled
		sub.RawStatus = "account_closed"
		sub.Raw = rawJSON(raw)
		return sub, nil
	}

	dec, decErr := subscriptions.DecodeSubscriptionAccount(data)
	if decErr != nil {
		// Fall back to presence inference; the base64 in Raw keeps the evidence.
		raw["subscription_decode_error"] = decErr.Error()
		sub.Status = SubscriptionStatusActive
		sub.RawStatus = "account_open"
	} else {
		applyDecodedSubscription(&sub, dec, raw, now)
	}

	f.applyPlanAndFiat(ctx, &sub, dec, ref.PlanPDA, raw, planCache, now)

	sub.Raw = rawJSON(raw)
	return sub, nil
}

// applyDecodedSubscription maps a decoded SubscriptionDelegation account onto
// the normalized RemoteSubscription (status doctrine + next billing window).
// The chain's own declarations override any local ref (drift between them is
// exactly what the diff engine exists to surface).
func applyDecodedSubscription(sub *RemoteSubscription, dec *subscriptions.SubscriptionAccount, raw map[string]any, now time.Time) {
	raw["subscription"] = map[string]any{
		"version":                 dec.Version,
		"delegator":               dec.Delegator.String(),
		"delegatee":               dec.Delegatee.String(),
		"payer":                   dec.Payer.String(),
		"init_id":                 dec.InitID,
		"amount":                  strconv.FormatUint(dec.Amount, 10), // mint base units
		"period_hours":            dec.PeriodHours,
		"created_at":              dec.CreatedAt,
		"amount_pulled_in_period": strconv.FormatUint(dec.AmountPulledInPeriod, 10),
		"current_period_start_ts": dec.CurrentPeriodStartTs,
		"expires_at_ts":           dec.ExpiresAtTs,
	}
	sub.CustomerID = dec.Delegator.String()
	sub.PlanID = dec.Delegatee.String()

	switch {
	case dec.ExpiresAtTs == 0:
		sub.Status = SubscriptionStatusActive
		sub.RawStatus = "active"
	case now.Unix() < dec.ExpiresAtTs:
		// cancel_subscription set a future valid-until boundary: the
		// on-chain cancel-at-period-end. Normalized active for card-rail
		// parity (Stripe cancel_at_period_end subs stay "active" too).
		sub.Status = SubscriptionStatusActive
		sub.RawStatus = "cancel_at_period_end"
	default:
		sub.Status = SubscriptionStatusCanceled
		sub.RawStatus = "expires_at_passed"
	}

	if dec.ExpiresAtTs == 0 && dec.PeriodHours > 0 && dec.PeriodHours <= maxSanePeriodHours {
		// Next billing window opens at the current period's end. Not set
		// for canceled subs: no billing follows expires_at_ts.
		next := time.Unix(dec.CurrentPeriodStartTs, 0).UTC().Add(time.Duration(dec.PeriodHours) * time.Hour)
		sub.NextBillingAt = &next
	}
}

// applyPlanAndFiat enriches the subscription with the plan account's
// declarations (plan_ended => expired) and the fiat normalization of the
// recurring amount (the subscription's own terms snapshot, denominated in the
// plan's mint — the subscription account does not store the mint).
func (f *SolanaFetcher) applyPlanAndFiat(ctx context.Context, sub *RemoteSubscription, dec *subscriptions.SubscriptionAccount, planPDA string, raw map[string]any, planCache map[string]*subscriptions.PlanAccount, now time.Time) {
	plan, planErr := f.planFor(ctx, planPDA, planCache)
	if planErr == nil && plan != nil {
		raw["plan"] = map[string]any{
			"status":       plan.Status,
			"plan_id":      plan.PlanID,
			"mint":         plan.Mint.String(),
			"amount":       strconv.FormatUint(plan.Amount, 10), // mint base units, NOT cents
			"period_hours": plan.PeriodHours,
			"end_ts":       plan.EndTs,
		}
		if sub.Status == SubscriptionStatusActive && plan.EndTs > 0 && time.Unix(plan.EndTs, 0).Before(now) {
			sub.Status = SubscriptionStatusExpired
			sub.RawStatus = "plan_ended"
		}
	}

	amountBase := uint64(0)
	if dec != nil {
		amountBase = dec.Amount
	} else if plan != nil {
		amountBase = plan.Amount
	}
	if plan != nil && amountBase > 0 {
		if cents, ok := f.solanaFiatCentsForMint(ctx, plan.Mint.String(), amountBase); ok {
			sub.AmountCents = cents
			sub.Currency = "USD"
		} else {
			raw["fiat_note"] = "unnormalized: mint is not a registry USD stablecoin or amount has sub-cent precision"
		}
	}
}

// planFor decodes the plan account once per PDA, caching results (nil entries
// cache misses/undecodable accounts so they are not refetched).
func (f *SolanaFetcher) planFor(ctx context.Context, planPDA string, cache map[string]*subscriptions.PlanAccount) (*subscriptions.PlanAccount, error) {
	if planPDA == "" {
		return nil, nil
	}
	if plan, ok := cache[planPDA]; ok {
		return plan, nil
	}
	pk, err := solanago.PublicKeyFromBase58(planPDA)
	if err != nil {
		cache[planPDA] = nil
		return nil, err
	}
	data, err := f.RPC.GetAccountData(ctx, pk)
	if err != nil {
		// Do not cache transient RPC failures.
		return nil, err
	}
	if len(data) == 0 {
		cache[planPDA] = nil
		return nil, nil
	}
	plan, err := subscriptions.DecodePlanAccount(data)
	if err != nil {
		cache[planPDA] = nil
		return nil, err
	}
	cache[planPDA] = plan
	return plan, nil
}

// solanaDiscoveryCadence bounds how often the permissionless-subscriber
// enumeration (getProgramAccounts per plan) runs for one plan. Its response
// scales with subscriber count, so it is never a per-tick lane.
const solanaDiscoveryCadence = 24 * time.Hour

// solanaDiscoverySlotWidth buckets solanaDiscoveryCadence into per-plan
// windows, so each plan's turn comes once a day. A hash-of-plan-id slot needs
// no stored state (MerchantFetcherBuilder builds a fresh fetcher every tick)
// and spreads plans across the day instead of one getProgramAccounts spike.
// A slot spans two refresh cycles, so a refresh lands in it however the
// scheduler staggers the merchant: a plan is enumerated once or twice a day.
const solanaDiscoverySlotWidth = 2 * providerrecovery.RefreshInterval

// planDiscoveryDue reports whether `now` falls in planPDA's slot of the
// day. hash(planPDA) assigns the slot permanently, so the same plan recurs on
// the same slot every cadence period with no stored state.
func planDiscoveryDue(planPDA string, now time.Time) bool {
	slots := int64(solanaDiscoveryCadence / solanaDiscoverySlotWidth)
	slot := now.UTC().UnixNano() / int64(solanaDiscoverySlotWidth)
	h := fnv.New32a()
	_, _ = h.Write([]byte(planPDA))
	return slot%slots == int64(h.Sum32()%uint32(slots))
}

// enumeratePlanSubscriptions lists (getProgramAccounts) every subscription
// account under our plans and normalizes the ones the local mirror does not
// know, marked discovered_not_local in Raw. They surface as PS-1 findings for
// operator triage and are never auto-materialized (makePS1 blocks them). Runs
// on a slow cadence (planDiscoveryDue).
func (f *SolanaFetcher) enumeratePlanSubscriptions(ctx context.Context, known map[string]struct{}, planCache map[string]*subscriptions.PlanAccount, now time.Time) ([]RemoteSubscription, error) {
	plans, err := f.Plans(ctx)
	if err != nil {
		return nil, fmt.Errorf("solana plan source: %w", err)
	}
	seen := map[string]struct{}{}
	var planPDAs []string
	for _, p := range plans {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		planPDAs = append(planPDAs, p)
	}
	sort.Strings(planPDAs)

	var out []RemoteSubscription
	for _, planPDA := range planPDAs {
		if !planDiscoveryDue(planPDA, now) {
			continue // full enumeration is a slow lane; not this plan's turn this tick
		}
		pk, err := solanago.PublicKeyFromBase58(planPDA)
		if err != nil {
			// A corrupt catalog link must not kill the scan; the other plans
			// still enumerate.
			log.WithContext(ctx).WithError(err).WithField("plan_pda", planPDA).
				Warn("solana reconcile: skipping undecodable plan PDA in enumeration")
			continue
		}
		accounts, err := f.RPC.GetProgramAccounts(ctx, subscriptions.ProgramID, []solanaint.ProgramAccountFilter{
			{Offset: 0, Bytes: []byte{subscriptions.SubscriptionAccountDiscriminator}},
			{Offset: subscriptions.SubscriptionAccountDelegateeOffset, Bytes: pk.Bytes()},
		})
		if err != nil {
			return nil, fmt.Errorf("solana: enumerate subscriptions for plan %s: %w", planPDA, err)
		}
		for _, acc := range accounts {
			pda := acc.Address.String()
			if _, ok := known[pda]; ok {
				continue // locally-known: lane 1 already normalized it
			}
			raw := map[string]any{
				"source":               "solana_program_scan",
				"discovered_not_local": true,
				"subscription_pda":     pda,
				"plan_pda":             planPDA,
				"account_exists":       len(acc.Data) > 0,
				"account_data_base64":  base64.StdEncoding.EncodeToString(acc.Data),
			}
			sub := RemoteSubscription{RailSubscriptionID: pda, PlanID: planPDA}
			dec, decErr := subscriptions.DecodeSubscriptionAccount(acc.Data)
			if decErr != nil {
				raw["subscription_decode_error"] = decErr.Error()
				sub.Status = SubscriptionStatusActive
				sub.RawStatus = "account_open"
			} else {
				applyDecodedSubscription(&sub, dec, raw, now)
			}
			f.applyPlanAndFiat(ctx, &sub, dec, planPDA, raw, planCache, now)
			sub.Raw = rawJSON(raw)
			out = append(out, sub)
		}
	}
	return out, nil
}

func (f *SolanaFetcher) fetchSignatures(ctx context.Context, ref SolanaSubscriptionRef, params FetchParams) ([]RemoteTransaction, error) {
	sigs, err := f.RPC.GetSignaturesForAddressPage(ctx, ref.SubscriptionPDA, "", f.signatureLimit())
	if err != nil {
		return nil, fmt.Errorf("solana: signatures for %s: %w", ref.SubscriptionPDA, err)
	}

	var out []RemoteTransaction
	for _, sig := range sigs {
		txn := RemoteTransaction{
			TransactionID:  sig.Signature,
			SubscriptionID: ref.SubscriptionPDA,
			Success:        !sig.HasError,
		}
		if sig.BlockTime != nil {
			txn.OccurredAt = *sig.BlockTime
			// Date-window filtering is only possible when the node reported a
			// block time. Filter before the per-tx fetch to save RPC calls.
			if !params.Since.IsZero() && txn.OccurredAt.Before(params.Since) {
				continue
			}
			if !params.Until.IsZero() && txn.OccurredAt.After(params.Until) {
				continue
			}
		}

		raw := map[string]any{
			"source":           "solana_signature",
			"subscription_pda": ref.SubscriptionPDA,
			"signature":        sig.Signature,
			"has_error":        sig.HasError,
		}

		class, meta, cerr := f.classifySignature(ctx, sig.Signature)
		if cerr != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("solana: classify %s: %w", sig.Signature, ctx.Err())
			}
			// Resilient fallback: classification is best-effort, the window
			// must survive an unfetchable/unparseable tx.
			raw["classification"] = "signature_only"
			raw["classification_note"] = cerr.Error()
			txn.Type = TransactionTypeSale
			if sig.HasError {
				txn.Type = TransactionTypeDecline
				txn.DeclineReason = "transaction failed on-chain"
			}
			txn.Raw = rawJSON(raw)
			out = append(out, txn)
			continue
		}

		raw["instruction"] = string(class.Kind)
		if len(class.Kinds) > 1 {
			raw["instructions"] = class.Kinds
		}

		switch class.Kind {
		case subscriptions.KindTransferSubscription:
			txn.Type = TransactionTypeSale
			if class.Transfer != nil {
				raw["amount_base_units"] = strconv.FormatUint(class.Transfer.Amount, 10)
				raw["mint"] = class.Transfer.Mint.String()
				if cents, ok := f.solanaFiatCentsForMint(ctx, class.Transfer.Mint.String(), class.Transfer.Amount); ok {
					txn.AmountCents = cents
					txn.Currency = "USD"
				} else {
					raw["fiat_note"] = "unnormalized: mint is not a registry USD stablecoin or amount has sub-cent precision"
				}
			}
			if sig.HasError {
				txn.Type = TransactionTypeDecline
				txn.DeclineReason = "transaction failed on-chain"
				if meta != nil && meta.Err != nil {
					txn.DeclineReason = fmt.Sprintf("on-chain failure: %v", meta.Err)
				}
			}
		case subscriptions.KindSubscribe:
			txn.Type = TransactionTypeSubscribe
		case subscriptions.KindCancelSubscription:
			txn.Type = TransactionTypeCancel
		case subscriptions.KindResumeSubscription:
			txn.Type = TransactionTypeResume
		case subscriptions.KindRevokeDelegation:
			txn.Type = TransactionTypeRevoke
		default:
			// A recognized program instruction that is not a subscription
			// event (create/update plan, init authority) should not appear on
			// a subscription PDA; map it to a sale with the kind noted.
			txn.Type = TransactionTypeSale
			if sig.HasError {
				txn.Type = TransactionTypeDecline
				txn.DeclineReason = "transaction failed on-chain"
			}
		}

		txn.Raw = rawJSON(raw)
		out = append(out, txn)
	}
	return out, nil
}

// solanaTxClass is the subscriptions-program classification of one fetched
// transaction: the primary instruction kind (a pull wins over lifecycle when a
// tx carries both), its decoded transfer payload for pulls, and every program
// instruction kind seen (for Raw).
type solanaTxClass struct {
	Kind     subscriptions.InstructionKind
	Transfer *subscriptions.TransferData
	// SubscriptionPDA is the pull instruction's first account (the
	// subscription PDA), when resolvable from the static account keys.
	SubscriptionPDA string
	Kinds           []string
}

// classifySignature fetches the transaction and classifies it. Any error is a
// signal to fall back to signature-only classification, never to fail the fetch.
func (f *SolanaFetcher) classifySignature(ctx context.Context, signature string) (solanaTxClass, *solrpc.TransactionMeta, error) {
	tx, meta, err := f.fetchParsedTx(ctx, signature)
	if err != nil {
		return solanaTxClass{}, nil, err
	}
	class, ok := classifySolanaTx(tx)
	if !ok {
		return solanaTxClass{}, meta, fmt.Errorf("no subscriptions-program instruction")
	}
	return class, meta, nil
}

// fetchParsedTx fetches and decodes one transaction.
func (f *SolanaFetcher) fetchParsedTx(ctx context.Context, signature string) (*solanago.Transaction, *solrpc.TransactionMeta, error) {
	sig, err := solanago.SignatureFromBase58(signature)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid signature: %w", err)
	}
	res, err := f.RPC.GetTransaction(ctx, sig)
	if err != nil {
		return nil, nil, fmt.Errorf("get transaction: %w", err)
	}
	if res == nil || res.Transaction == nil {
		return nil, nil, fmt.Errorf("empty transaction result")
	}
	tx, err := res.Transaction.GetTransaction()
	if err != nil || tx == nil {
		return nil, nil, fmt.Errorf("decode transaction: %w", err)
	}
	return tx, res.Meta, nil
}

// classifySolanaTx scans the top-level instructions for the subscriptions
// program and classifies by discriminator. ok=false when the tx carries no
// recognizable program instruction.
func classifySolanaTx(tx *solanago.Transaction) (solanaTxClass, bool) {
	var class solanaTxClass
	for _, ix := range tx.Message.Instructions {
		prog, err := tx.Message.Program(ix.ProgramIDIndex)
		if err != nil || !prog.Equals(subscriptions.ProgramID) {
			continue
		}
		kind, ok := subscriptions.ParseInstructionKind(ix.Data)
		if !ok {
			continue
		}
		class.Kinds = append(class.Kinds, string(kind))
		if kind == subscriptions.KindTransferSubscription {
			// The pull is the money event; it wins as the primary kind.
			class.Kind = kind
			if td, err := subscriptions.DecodeTransferData(ix.Data); err == nil {
				class.Transfer = td
			}
			if len(ix.Accounts) > 0 && int(ix.Accounts[0]) < len(tx.Message.AccountKeys) {
				class.SubscriptionPDA = tx.Message.AccountKeys[ix.Accounts[0]].String()
			}
		} else if class.Kind == "" {
			class.Kind = kind
		}
	}
	return class, class.Kind != ""
}

// wrappedSOLMint is the canonical wrapped-SOL mint; checkout attempts bind
// native-SOL quotes as "" or this mint.
const wrappedSOLMint = "So11111111111111111111111111111111111111112"

func isNativeSolanaMint(mint string) bool {
	mint = strings.TrimSpace(mint)
	return mint == "" || mint == wrappedSOLMint
}

// Wallet-scan discovery kinds — from tx shape, never from the memo.
const (
	solanaDiscoveryKindOneOff = "one_off"
	solanaDiscoveryKindPull   = "pull"
)

// solanaDiscovery is the wallet-scan verdict envelope carried in
// RemoteTransaction.Raw under "solana_discovery"; makeSolanaDiscoveryPS4
// routes on it (clean one-off => backfill lane; park => operator triage).
type solanaDiscovery struct {
	Verdict     string `json:"verdict"` // "clean" | "park"
	ParkReason  string `json:"park_reason,omitempty"`
	Kind        string `json:"kind"` // one_off | pull
	MemoLocalID string `json:"memo_local_id"`
	LocalKind   string `json:"local_kind"` // checkout_attempt | pull_intent | none
	CustomerID  string `json:"customer_id,omitempty"`
	PriceID     string `json:"price_id,omitempty"`
}

// walletTransfer is the single asset a transaction moved INTO the merchant
// wallet. Mint == "" means native SOL (BaseUnits are lamports).
type walletTransfer struct {
	Mint      string
	BaseUnits uint64
}

// walletScanCandidate is one memo-recognized wallet transaction awaiting the
// duplicate-local-id pass.
type walletScanCandidate struct {
	txn       RemoteTransaction
	raw       map[string]any
	disc      *solanaDiscovery // nil for lifecycle/failed txs (no money claim)
	localID   uuid.UUID
	multiMemo bool
	kind      string
	transfer  walletTransfer
	hasMoney  bool
	moneyNote string
	subPDA    string
	failed    bool
}

// scanMerchantWallet walks the declared merchant wallet's signature history
// inside the Since/Until window and normalizes only the transactions carrying
// a recognized memo. Random deposits and foreign memos produce zero findings
// by design.
func (f *SolanaFetcher) scanMerchantWallet(ctx context.Context, params FetchParams, emitted map[string]struct{}) ([]RemoteTransaction, error) {
	wallet := strings.TrimSpace(params.AccountID)
	if wallet == "" {
		wallet = strings.TrimSpace(f.MerchantWallet)
	}
	if wallet == "" {
		return nil, nil // no declared wallet: the scan is disarmed
	}
	walletPK, err := solanago.PublicKeyFromBase58(wallet)
	if err != nil {
		return nil, fmt.Errorf("solana: merchant wallet account_id %q is not a wallet address: %w", wallet, err)
	}

	// Bounded newest-first pagination; stop at the window floor or the cap.
	var sigs []solanaint.SignatureInfo
	before := ""
	listed := 0
	capHit := false
pageLoop:
	for {
		limit := f.walletScanPageSize()
		if remaining := f.walletScanCap() - listed; remaining < limit {
			limit = remaining
		}
		if limit <= 0 {
			capHit = true
			break
		}
		page, err := f.RPC.GetSignaturesForAddressPage(ctx, wallet, before, limit)
		if err != nil {
			return nil, fmt.Errorf("solana: wallet signatures for %s: %w", wallet, err)
		}
		if len(page) == 0 {
			break
		}
		listed += len(page)
		for _, sig := range page {
			if sig.BlockTime != nil {
				bt := *sig.BlockTime
				if !params.Since.IsZero() && bt.Before(params.Since) {
					break pageLoop // listings are newest-first: everything below is out of window
				}
				if !params.Until.IsZero() && bt.After(params.Until) {
					continue
				}
			} else if !params.Since.IsZero() || !params.Until.IsZero() {
				continue // no block time: cannot be proven in-window
			}
			sigs = append(sigs, sig)
		}
		if len(page) < limit {
			break // end of history
		}
		before = page[len(page)-1].Signature
	}
	if capHit {
		log.WithContext(ctx).WithFields(log.Fields{
			"wallet": wallet, "cap": f.walletScanCap(),
		}).Warn("solana reconcile: wallet scan hit the per-window signature cap; older window txs deferred to the next pass")
	}

	skipped := 0
	var candidates []*walletScanCandidate
	for _, sig := range sigs {
		if _, ok := emitted[sig.Signature]; ok {
			continue // already normalized by the per-PDA lane
		}
		c, drop, err := f.buildWalletCandidate(ctx, sig, wallet, walletPK)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("solana: wallet scan %s: %w", sig.Signature, ctx.Err())
			}
			skipped++ // unfetchable/unparseable: without the tx there is no memo to recognize
			continue
		}
		if drop {
			continue
		}
		candidates = append(candidates, c)
	}
	if skipped > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"wallet": wallet, "skipped": skipped}).
			Warn("solana reconcile: wallet scan skipped unfetchable/unparseable transactions")
	}

	// Duplicate pass: one local-id on several SUCCESSFUL money-bearing
	// signatures can never auto-land — park them all (verify-not-decline).
	counts := map[uuid.UUID]int{}
	for _, c := range candidates {
		if c.disc != nil && !c.failed && c.hasMoney {
			counts[c.localID]++
		}
	}
	out := make([]RemoteTransaction, 0, len(candidates))
	for _, c := range candidates {
		if c.disc != nil && !c.failed && c.hasMoney && counts[c.localID] > 1 {
			c.disc.Verdict = "park"
			c.disc.ParkReason = fmt.Sprintf("local-id %s is claimed by %d successful transactions in this window", c.localID, counts[c.localID])
			c.disc.CustomerID, c.disc.PriceID = "", "" // never backfill a duplicate
		}
		if c.disc != nil {
			c.raw["solana_discovery"] = c.disc
		}
		c.txn.Raw = rawJSON(c.raw)
		out = append(out, c.txn)
	}
	return out, nil
}

// buildWalletCandidate fetches, memo-gates, shape-classifies and verifies one
// wallet-history signature. drop=true means the tx is deliberately invisible
// (no recognized memo, or a valueless memo claiming nothing local).
func (f *SolanaFetcher) buildWalletCandidate(ctx context.Context, sig solanaint.SignatureInfo, wallet string, walletPK solanago.PublicKey) (*walletScanCandidate, bool, error) {
	tx, meta, err := f.fetchParsedTx(ctx, sig.Signature)
	if err != nil {
		return nil, false, err
	}

	ids := distinctUUIDs(solanaint.PurchaseMemoLocalIDs(tx))
	if len(ids) == 0 {
		return nil, true, nil // no recognized openrails memo: not ours, by design
	}

	c := &walletScanCandidate{localID: ids[0], multiMemo: len(ids) > 1}
	raw := map[string]any{
		"source":        "solana_wallet_scan",
		"wallet":        wallet,
		"signature":     sig.Signature,
		"has_error":     sig.HasError,
		"memo_local_id": ids[0].String(),
	}
	if len(ids) > 1 {
		all := make([]string, 0, len(ids))
		for _, id := range ids {
			all = append(all, id.String())
		}
		raw["memo_local_ids"] = all
	}
	if len(tx.Message.AccountKeys) > 0 {
		raw["fee_payer"] = tx.Message.AccountKeys[0].String()
	}

	txn := RemoteTransaction{TransactionID: sig.Signature, Success: !sig.HasError}
	if sig.BlockTime != nil {
		txn.OccurredAt = *sig.BlockTime
	}

	class, hasClass := classifySolanaTx(tx)
	if hasClass {
		raw["instruction"] = string(class.Kind)
		if len(class.Kinds) > 1 {
			raw["instructions"] = class.Kinds
		}
	}
	switch {
	case hasClass && class.Kind == subscriptions.KindTransferSubscription:
		// Pull: money truth is the instruction's transferData.
		c.kind = solanaDiscoveryKindPull
		c.subPDA = class.SubscriptionPDA
		txn.Type = TransactionTypeSale
		txn.SubscriptionID = class.SubscriptionPDA
		if class.Transfer != nil {
			c.transfer = walletTransfer{Mint: class.Transfer.Mint.String(), BaseUnits: class.Transfer.Amount}
			c.hasMoney = true
			raw["amount_base_units"] = strconv.FormatUint(class.Transfer.Amount, 10)
			raw["mint"] = c.transfer.Mint
			raw["payer_wallet"] = class.Transfer.Delegator.String()
		} else {
			c.moneyNote = "pull instruction data undecodable"
		}
	case hasClass:
		// Memo'd lifecycle tx (subscribe/cancel/resume/revoke reference the
		// merchant account): no money moved; evidence only, no verdict.
		switch class.Kind {
		case subscriptions.KindSubscribe:
			txn.Type = TransactionTypeSubscribe
		case subscriptions.KindCancelSubscription:
			txn.Type = TransactionTypeCancel
		case subscriptions.KindResumeSubscription:
			txn.Type = TransactionTypeResume
		case subscriptions.KindRevokeDelegation:
			txn.Type = TransactionTypeRevoke
		default:
			return nil, true, nil // plan admin traffic: not a purchase claim
		}
		if sig.HasError {
			txn.Type = TransactionTypeDecline
			txn.DeclineReason = "transaction failed on-chain"
		}
		c.txn, c.raw = txn, raw
		return c, false, nil
	default:
		// Plain transfer: money truth is the balance delta into the wallet.
		c.kind = solanaDiscoveryKindOneOff
		txn.Type = TransactionTypeSale
		if t, ok, note := walletTransferMoney(meta, tx, walletPK); ok {
			c.transfer = t
			c.hasMoney = true
			if t.Mint == "" {
				raw["lamports"] = t.BaseUnits
				raw["fiat_note"] = "unnormalized: native SOL needs operator pricing (no FX inside reconcile)"
			} else {
				raw["mint"] = t.Mint
				raw["amount_base_units"] = strconv.FormatUint(t.BaseUnits, 10)
			}
		} else {
			c.moneyNote = note
		}
	}

	if sig.HasError {
		// A failed purchase attempt moved no money; keep it as decline
		// evidence without a discovery verdict.
		txn.Type = TransactionTypeDecline
		txn.DeclineReason = "transaction failed on-chain"
		if meta != nil && meta.Err != nil {
			txn.DeclineReason = fmt.Sprintf("on-chain failure: %v", meta.Err)
		}
		c.failed = true
		c.txn, c.raw = txn, raw
		return c, false, nil
	}

	if c.hasMoney && c.transfer.Mint != "" {
		if cents, ok := f.solanaFiatCentsForMint(ctx, c.transfer.Mint, c.transfer.BaseUnits); ok {
			txn.AmountCents = cents
			txn.Currency = "USD"
		} else {
			raw["fiat_note"] = "unnormalized: mint is not a registry USD stablecoin or amount has sub-cent precision"
		}
	}

	c.txn, c.raw = txn, raw
	drop, err := f.verifyWalletDiscovery(ctx, c, wallet)
	if err != nil {
		return nil, false, err
	}
	return c, drop, nil
}

// verifyWalletDiscovery applies the memo trust model to one candidate: the
// memo is a discovery hint, never money truth. The local-id must resolve
// consistently and uniquely, and the resolved record's bound expectations
// (recipient wallet, mint, base units) must agree with the on-chain transfer;
// any disagreement parks the finding. drop=true only for a valueless memo
// claiming nothing local (a costless forgery must produce zero noise).
func (f *SolanaFetcher) verifyWalletDiscovery(ctx context.Context, c *walletScanCandidate, wallet string) (bool, error) {
	d := &solanaDiscovery{Verdict: "clean", Kind: c.kind, MemoLocalID: c.localID.String(), LocalKind: "none"}
	c.disc = d
	park := func(reason string) {
		d.Verdict = "park"
		d.ParkReason = reason
	}

	if c.multiMemo {
		park("multiple distinct purchase memos in one transaction")
		return false, nil
	}

	var rec *SolanaLocalRecord
	if f.Resolve != nil {
		var rerr error
		rec, rerr = f.Resolve(ctx, c.localID)
		if rerr != nil {
			return false, fmt.Errorf("solana: resolve memo local-id %s: %w", c.localID, rerr)
		}
	}

	if rec == nil {
		if !c.hasMoney {
			return true, nil // no money and no local claim: invisible
		}
		if f.Resolve == nil {
			park("memo local-id resolution is not wired; cannot verify against local records")
			return false, nil
		}
		park("memo names no local record (possible lost local state); importing is an operator decision")
		return false, nil
	}

	switch rec.Kind {
	case SolanaLocalKindCheckoutAttempt:
		d.LocalKind = rec.Kind
		switch {
		case c.kind != solanaDiscoveryKindOneOff:
			park("memo resolves to a checkout attempt but the transaction is a subscription pull")
		case rec.Rail != "" && rec.Rail != string(ProviderSolana):
			park(fmt.Sprintf("memo resolves to a %s checkout attempt, not a solana one", rec.Rail))
		case rec.ExpectedRecipient != "" && rec.ExpectedRecipient != wallet:
			park("checkout attempt expects a different recipient wallet than the scanned merchant wallet")
		case !c.hasMoney:
			park("memo claims a checkout attempt but the transaction moved no money into the wallet: " + c.moneyNote)
		case rec.ExpectedTokenAmount == 0:
			park("checkout attempt carries no bound solana token quote to verify against")
		case !sameSolanaAsset(rec.ExpectedMint, c.transfer.Mint):
			park(fmt.Sprintf("transfer asset %s disagrees with the session's quoted mint %s", assetName(c.transfer.Mint), assetName(rec.ExpectedMint)))
		case rec.ExpectedTokenAmount != c.transfer.BaseUnits:
			park(fmt.Sprintf("transfer of %d base units disagrees with the session's quoted %d", c.transfer.BaseUnits, rec.ExpectedTokenAmount))
		case rec.SettledTransactionID != "" && rec.SettledTransactionID != c.txn.TransactionID:
			park("checkout attempt already settled by a different signature: " + rec.SettledTransactionID)
		default:
			d.CustomerID = rec.CustomerID.String()
			d.PriceID = rec.PriceID.String()
		}
	case SolanaLocalKindPullIntent:
		d.LocalKind = rec.Kind
		switch {
		case c.kind != solanaDiscoveryKindPull:
			park("memo resolves to a pull intent but the transaction is not a subscription pull")
		case rec.Rail != "" && rec.Rail != string(ProviderSolana):
			park(fmt.Sprintf("memo resolves to a %s intent, not a solana one", rec.Rail))
		case !c.hasMoney:
			park("memo claims a pull but the instruction carries no decodable transfer amount")
		case rec.SubscriptionPDA != "" && rec.SubscriptionPDA != c.subPDA:
			park(fmt.Sprintf("pull intent addresses subscription %s but the transaction pulls %s", rec.SubscriptionPDA, c.subPDA))
		}
		// A clean pull routes through the generic PS-4 correlator lane by
		// subscription PDA; the intent only had to agree.
	default:
		park(fmt.Sprintf("memo resolves to an unrecognized local record kind %q", rec.Kind))
	}
	return false, nil
}

func sameSolanaAsset(expectedMint, actualMint string) bool {
	if isNativeSolanaMint(expectedMint) {
		return isNativeSolanaMint(actualMint)
	}
	return strings.TrimSpace(expectedMint) == strings.TrimSpace(actualMint)
}

func assetName(mint string) string {
	if isNativeSolanaMint(mint) {
		return "SOL"
	}
	return mint
}

// u64ToI64 converts a uint64 balance/amount to int64, refusing values above
// math.MaxInt64 rather than silently wrapping to negative. Real Solana
// balances never approach this (total SOL supply and any sane SPL mint sit
// many orders of magnitude below it), so a rejection here means the data is
// bogus — safer to treat the transfer as unusable than trust a wrapped delta.
func u64ToI64(u uint64) (int64, bool) {
	if u > math.MaxInt64 {
		return 0, false
	}
	return int64(u), true
}

// i64Delta converts a pre/post uint64 pair to int64 in one call, propagating
// failure if either side overflows.
func i64Delta(post, pre uint64) (int64, int64, bool) {
	p, ok := u64ToI64(post)
	if !ok {
		return 0, 0, false
	}
	q, ok := u64ToI64(pre)
	if !ok {
		return 0, 0, false
	}
	return p, q, true
}

// walletTransferMoney extracts the ONE asset a transaction moved into the
// wallet from the balance metas (the transfer IS the money truth — never the
// memo). ok=false with a note when nothing moved in, several assets did, or
// the meta is unusable.
func walletTransferMoney(meta *solrpc.TransactionMeta, tx *solanago.Transaction, wallet solanago.PublicKey) (walletTransfer, bool, string) {
	if meta == nil {
		return walletTransfer{}, false, "transaction meta unavailable"
	}

	// SPL deltas: sum post-pre over token accounts owned by the wallet, per mint.
	type tokenSnap struct {
		mint   string
		amount uint64
	}
	readTokenBalances := func(balances []solrpc.TokenBalance) map[uint16]tokenSnap {
		out := map[uint16]tokenSnap{}
		for _, tb := range balances {
			if tb.Owner == nil || !tb.Owner.Equals(wallet) || tb.UiTokenAmount == nil {
				continue
			}
			amt, err := strconv.ParseUint(tb.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				continue
			}
			out[tb.AccountIndex] = tokenSnap{mint: tb.Mint.String(), amount: amt}
		}
		return out
	}
	pre := readTokenBalances(meta.PreTokenBalances)
	post := readTokenBalances(meta.PostTokenBalances)
	deltas := map[string]int64{}
	for idx, p := range post {
		postAmt, preAmt, ok := i64Delta(p.amount, pre[idx].amount)
		if !ok {
			return walletTransfer{}, false, "token amount exceeds representable range"
		}
		deltas[p.mint] += postAmt - preAmt
	}
	for idx, p := range pre {
		if _, ok := post[idx]; !ok {
			amt, ok := u64ToI64(p.amount)
			if !ok {
				return walletTransfer{}, false, "token amount exceeds representable range"
			}
			deltas[p.mint] -= amt // account emptied/closed
		}
	}

	var received []walletTransfer
	for mint, delta := range deltas {
		if delta > 0 {
			received = append(received, walletTransfer{Mint: mint, BaseUnits: uint64(delta)})
		}
	}

	// Native SOL delta at the wallet's own account index.
	if tx != nil {
		for i, key := range tx.Message.AccountKeys {
			if !key.Equals(wallet) {
				continue
			}
			if i < len(meta.PreBalances) && i < len(meta.PostBalances) {
				postLamports, preLamports, ok := i64Delta(meta.PostBalances[i], meta.PreBalances[i])
				if !ok {
					return walletTransfer{}, false, "lamport balance exceeds representable range"
				}
				if delta := postLamports - preLamports; delta > 0 {
					received = append(received, walletTransfer{Mint: "", BaseUnits: uint64(delta)})
				}
			}
			break
		}
	}

	switch len(received) {
	case 1:
		return received[0], true, ""
	case 0:
		return walletTransfer{}, false, "no transfer into the merchant wallet"
	default:
		sort.Slice(received, func(i, j int) bool { return received[i].Mint < received[j].Mint })
		names := make([]string, 0, len(received))
		for _, r := range received {
			names = append(names, assetName(r.Mint))
		}
		return walletTransfer{}, false, "ambiguous: multiple assets moved into the wallet (" + strings.Join(names, ", ") + ")"
	}
}

func distinctUUIDs(ids []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// mintDecimals reads a mint's on-chain base-unit precision, memoized for the
// fetcher's lifetime: `decimals` is written once by InitializeMint.
func (f *SolanaFetcher) mintDecimals(ctx context.Context, mint string) (int, bool) {
	mint = strings.TrimSpace(mint)
	if mint == "" || f.RPC == nil {
		return 0, false
	}
	if d, ok := f.mintDecimalsCache[mint]; ok {
		return d, true
	}
	pk, err := solanago.PublicKeyFromBase58(mint)
	if err != nil {
		return 0, false
	}
	d, err := solanaint.ReadMintDecimals(ctx, f.RPC, pk)
	if err != nil {
		return 0, false
	}
	if f.mintDecimalsCache == nil {
		f.mintDecimalsCache = make(map[string]int)
	}
	f.mintDecimalsCache[mint] = d
	return d, true
}

// isUSDStablecoinMint reports whether the mint is a canonical USD-pegged
// stablecoin (the trust anchor: KnownStablecoinByMint). Devnet mints fall out by
// construction — they are not in the registry — so devnet amounts stay
// unnormalized. Devnet money is fake.
func isUSDStablecoinMint(mint string) bool {
	sc, ok := solanatokens.KnownStablecoinByMint(mint)
	return ok && sc.Peg == "usd"
}

// solanaFiatCents converts mint base units to integer cents (the fetchers'
// AmountCents unit) when the mint is a registry USD stablecoin and the value
// is exactly cent-representable at the mint's on-chain decimals. Sub-cent
// precision is never rounded; the caller keeps base units in Raw instead.
func solanaFiatCents(mint string, decimals int, baseUnits uint64) (int64, bool) {
	if !isUSDStablecoinMint(mint) || decimals < 2 || decimals > config.MaxTokenDecimals {
		return 0, false
	}
	perCent := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-2)), nil)
	cents, rem := new(big.Int).QuoRem(new(big.Int).SetUint64(baseUnits), perCent, new(big.Int))
	if rem.Sign() != 0 || !cents.IsInt64() {
		return 0, false
	}
	return cents.Int64(), true
}

// solanaFiatCentsForMint resolves the mint's on-chain decimals and normalizes.
func (f *SolanaFetcher) solanaFiatCentsForMint(ctx context.Context, mint string, baseUnits uint64) (int64, bool) {
	if !isUSDStablecoinMint(mint) {
		return 0, false
	}
	decimals, ok := f.mintDecimals(ctx, mint)
	if !ok {
		return 0, false
	}
	return solanaFiatCents(mint, decimals, baseUnits)
}
