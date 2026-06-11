package reconcile

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// SolanaSubscriptionRef identifies one locally-known on-chain subscription:
// the subscription PDA (which is also the local
// subscriptions.processor_subscription_id for solana memberships) and its
// plan PDA. The caller supplies these from billing.solana_subscriptions.
type SolanaSubscriptionRef struct {
	SubscriptionPDA  string
	PlanPDA          string
	SubscriberWallet string
}

// SolanaSubscriptionSource lists the locally-known solana subscriptions whose
// on-chain accounts the fetcher should read. A function type so phase-2
// wiring is a one-line closure over the repo.
type SolanaSubscriptionSource func(ctx context.Context) ([]SolanaSubscriptionRef, error)

// solanaRPC is the read slice of *solanaint.RPCClient the fetcher uses.
type solanaRPC interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	GetSignaturesForAddress(ctx context.Context, address string, limit int) ([]solanaint.SignatureInfo, error)
}

// SolanaFetcher reports the on-chain state of the locally-known subscriptions
// of the official Solana subscriptions program. It is deliberately MINIMAL:
// on-chain state is already self-reconciling via the #258 crank worker, so
// this fetcher just normalizes what exists for the cross-provider diff
// engine. Capabilities: subscriptions + transactions only (no refunds,
// chargebacks, or vault on-chain).
//
// Normalization notes:
//   - There is no on-chain subscription-account decoder yet (only
//     DecodePlanAccount), so subscription status is inferred: subscription PDA
//     account present => active (expired when the decoded plan's EndTs has
//     passed); PDA absent => cancelled (the program closes the account). The
//     raw account bytes are preserved base64 in Raw for when a decoder lands.
//   - Amounts are mint base units, not fiat: AmountCents stays 0 and the
//     plan's Amount/Mint live in Raw.
//   - Transactions are the subscription PDA's signature listing (bounded by
//     SignatureLimit); without parsing each transaction the kind of event
//     (subscribe/pull/cancel) is unknown, so successful signatures normalize
//     to sale and failed ones to decline.
//   - Reads here are bulk point-in-time listings, not post-tx reads, so plain
//     GetAccountData (no *AtSlot/ReadUntilConsistent gating) is correct.
type SolanaFetcher struct {
	RPC    solanaRPC
	Source SolanaSubscriptionSource
	// SignatureLimit bounds the per-subscription signature listing (default 50).
	SignatureLimit int
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

func (f *SolanaFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	snap := &RemoteSnapshot{
		Provider:     ProviderSolana,
		FetchedAt:    time.Now().UTC(),
		Capabilities: f.Capabilities(),
	}

	refs, err := f.Source(ctx)
	if err != nil {
		return nil, fmt.Errorf("solana subscription source: %w", err)
	}

	// Plan accounts are shared across subscribers; decode each once.
	planCache := map[string]*subscriptions.PlanAccount{}
	now := time.Now().UTC()

	for _, ref := range refs {
		if params.SubscriptionID != "" && ref.SubscriptionPDA != params.SubscriptionID {
			continue
		}
		if params.CustomerID != "" && ref.SubscriberWallet != params.CustomerID {
			continue
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
	}

	return snap, nil
}

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
		ProcessorSubscriptionID: ref.SubscriptionPDA,
		CustomerID:              ref.SubscriberWallet,
		PlanID:                  ref.PlanPDA,
	}
	if len(data) == 0 {
		// The program closes the subscription account on cancel; absence of a
		// once-known account is the on-chain cancelled state.
		sub.Status = SubscriptionStatusCancelled
		sub.RawStatus = "account_closed"
		sub.Raw = rawJSON(raw)
		return sub, nil
	}

	sub.Status = SubscriptionStatusActive
	sub.RawStatus = "account_open"

	if plan, err := f.planFor(ctx, ref.PlanPDA, planCache); err == nil && plan != nil {
		raw["plan"] = map[string]any{
			"status":       plan.Status,
			"plan_id":      plan.PlanID,
			"mint":         plan.Mint.String(),
			"amount":       plan.Amount, // mint base units, NOT cents
			"period_hours": plan.PeriodHours,
			"end_ts":       plan.EndTs,
		}
		if plan.EndTs > 0 && time.Unix(plan.EndTs, 0).Before(now) {
			sub.Status = SubscriptionStatusExpired
			sub.RawStatus = "plan_ended"
		}
	}

	sub.Raw = rawJSON(raw)
	return sub, nil
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

func (f *SolanaFetcher) fetchSignatures(ctx context.Context, ref SolanaSubscriptionRef, params FetchParams) ([]RemoteTransaction, error) {
	sigs, err := f.RPC.GetSignaturesForAddress(ctx, ref.SubscriptionPDA, f.signatureLimit())
	if err != nil {
		return nil, fmt.Errorf("solana: signatures for %s: %w", ref.SubscriptionPDA, err)
	}

	var out []RemoteTransaction
	for _, sig := range sigs {
		txn := RemoteTransaction{
			TransactionID:  sig.Signature,
			SubscriptionID: ref.SubscriptionPDA,
			Type:           TransactionTypeSale,
			Success:        !sig.HasError,
			Raw: rawJSON(map[string]any{
				"source":           "solana_signature",
				"subscription_pda": ref.SubscriptionPDA,
				"signature":        sig.Signature,
				"has_error":        sig.HasError,
			}),
		}
		if sig.HasError {
			txn.Type = TransactionTypeDecline
			txn.DeclineReason = "transaction failed on-chain"
		}
		if sig.BlockTime != nil {
			txn.OccurredAt = *sig.BlockTime
			// Date-window filtering is only possible when the node reported a
			// block time.
			if !params.Since.IsZero() && txn.OccurredAt.Before(params.Since) {
				continue
			}
			if !params.Until.IsZero() && txn.OccurredAt.After(params.Until) {
				continue
			}
		}
		out = append(out, txn)
	}
	return out, nil
}
