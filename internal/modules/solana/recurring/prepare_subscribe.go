package recurring

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"

	solanago "github.com/doujins-org/solana-go"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

// subscriptionAuthorityInitIDOffset is the byte offset of the SubscriptionAuthority
// account's initId field (an i64 LE). The subscribe instruction must echo this
// value back as ExpectedSubscriptionAuthInitID; it is only knowable AFTER
// init_subscription_authority has executed and written the account. Proven by
// the devnet lifecycle test (reads offset 98 between init and subscribe).
const subscriptionAuthorityInitIDOffset = 98

// prepareRPC is the minimal RPC surface PrepareSubscribe needs (satisfied by
// *solanaint.RPCClient): read on-chain account state + a recent blockhash to
// build unsigned transactions.
type prepareRPC interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
}

// PrepareSubscribeService builds the UNSIGNED Subscriptions-Delegation-Program
// transactions the subscriber's wallet signs to start a recurring subscription
// (#261). All on-chain instruction encoding stays here (devnet-validated
// builders); the frontend only signs + sends, then confirms.
//
// Two-step on-chain reality (proven by lifecycle_devnet_test): subscribe needs
// the SubscriptionAuthority initId, only readable AFTER init_subscription_authority
// runs. So a FIRST-TIME subscriber (no authority for this mint yet) gets the
// init tx first, signs+sends it, then re-prepares to get the subscribe tx; a
// RETURNING subscriber (authority already exists) gets the subscribe tx directly.
type PrepareSubscribeService struct {
	submitter Submitter // resolves the tenant's merchant (plan owner / cranker) address
	rpc       prepareRPC
	network   string
}

// NewPrepareSubscribeService builds a PrepareSubscribeService.
func NewPrepareSubscribeService(submitter Submitter, rpc prepareRPC, network string) *PrepareSubscribeService {
	return &PrepareSubscribeService{submitter: submitter, rpc: rpc, network: network}
}

// PrepareSubscribeInput describes the plan + subscriber to enroll. Plan terms are
// the canonical, server-resolved values from the price's Solana config (never
// client-supplied amounts).
type PrepareSubscribeInput struct {
	TenantID         tenant.ID
	SubscriberWallet string // the connected wallet (signer + fee payer)
	PlanID           uint64
	MintSymbol       string
	AmountBaseUnits  uint64
	PeriodHours      uint64
	PlanCreatedAt    int64
}

// PrepareSubscribeResult is the set of unsigned transactions the wallet must sign
// next, plus the derived PDAs for the confirm step.
type PrepareSubscribeResult struct {
	// Transactions are base64-encoded unsigned transactions to sign+send in order.
	Transactions []string
	// Step is "init" when the returned tx is init_subscription_authority and the
	// caller must re-prepare afterwards for the subscribe tx; "subscribe" when the
	// returned tx is the subscribe (the final on-chain step before confirm).
	Step string
	// AuthorityExists reports whether the SubscriptionAuthority already existed
	// (returning subscriber → single subscribe tx, no init step).
	AuthorityExists bool
	MerchantAddress string
	PlanPDA         string
	SubscriptionPDA string
	AuthorityPDA    string
	Mint            string
}

// Prepare derives the PDAs, checks whether the subscriber's SubscriptionAuthority
// for this mint exists, and returns the next unsigned transaction(s) to sign:
// [init] (first-time, then re-prepare) or [subscribe] (authority present).
func (s *PrepareSubscribeService) Prepare(ctx context.Context, in PrepareSubscribeInput) (*PrepareSubscribeResult, error) {
	if in.SubscriberWallet == "" {
		return nil, fmt.Errorf("recurring: subscriber wallet is required")
	}
	if in.AmountBaseUnits == 0 || in.PeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid plan terms (amount/period)")
	}

	mintStr, _, err := ResolveRecurringMint(in.MintSymbol, s.network)
	if err != nil {
		return nil, err
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid mint: %w", err)
	}
	subscriber, err := solanago.PublicKeyFromBase58(in.SubscriberWallet)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid subscriber wallet: %w", err)
	}
	merchant, err := s.submitter.MerchantAddress(ctx, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant: %w", err)
	}

	planPDA, planBump, err := subscriptions.DerivePlanPDA(merchant, in.PlanID)
	if err != nil {
		return nil, err
	}
	subPDA, _, err := subscriptions.DeriveSubscriptionPDA(planPDA, subscriber)
	if err != nil {
		return nil, err
	}
	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subscriber, mint)
	if err != nil {
		return nil, err
	}
	subscriberATA, _, err := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive subscriber ata: %w", err)
	}

	result := &PrepareSubscribeResult{
		MerchantAddress: merchant.String(),
		PlanPDA:         planPDA.String(),
		SubscriptionPDA: subPDA.String(),
		AuthorityPDA:    saPDA.String(),
		Mint:            mint.String(),
	}

	authData, err := s.rpc.GetAccountData(ctx, saPDA)
	if err != nil {
		return nil, fmt.Errorf("recurring: read subscription authority: %w", err)
	}

	// First-time subscriber for this mint: the authority must be initialized
	// before subscribe can be built (subscribe needs its initId). Return the init
	// tx; the caller signs+sends it, then re-prepares for the subscribe tx.
	if len(authData) == 0 {
		initIx := subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
			Owner:                 subscriber,
			SubscriptionAuthority: saPDA,
			TokenMint:             mint,
			UserATA:               subscriberATA,
			TokenProgram:          solanago.TokenProgramID,
		})
		tx, err := s.buildUnsignedTxBase64(ctx, subscriber, []solanago.Instruction{initIx})
		if err != nil {
			return nil, err
		}
		result.Transactions = []string{tx}
		result.Step = "init"
		result.AuthorityExists = false
		return result, nil
	}

	// Authority exists: read its initId and build the subscribe tx.
	initID, err := readInitID(authData)
	if err != nil {
		return nil, err
	}
	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	if err != nil {
		return nil, fmt.Errorf("recurring: derive event authority: %w", err)
	}
	subscribeIx := subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
		Subscriber:                     subscriber,
		Merchant:                       merchant,
		PlanPDA:                        planPDA,
		SubscriptionPDA:                subPDA,
		SubscriptionAuthorityPDA:       saPDA,
		EventAuthority:                 eventAuth,
		PlanID:                         in.PlanID,
		PlanBump:                       planBump,
		ExpectedMint:                   mint,
		ExpectedAmount:                 in.AmountBaseUnits,
		ExpectedPeriodHours:            in.PeriodHours,
		ExpectedCreatedAt:              in.PlanCreatedAt,
		ExpectedSubscriptionAuthInitID: initID,
	})
	tx, err := s.buildUnsignedTxBase64(ctx, subscriber, []solanago.Instruction{subscribeIx})
	if err != nil {
		return nil, err
	}
	result.Transactions = []string{tx}
	result.Step = "subscribe"
	result.AuthorityExists = true
	return result, nil
}

// buildUnsignedTxBase64 assembles an unsigned transaction (payer = subscriber,
// who signs + pays gas) with a recent blockhash and returns it base64-encoded for
// the wallet to deserialize, sign, and send.
func (s *PrepareSubscribeService) buildUnsignedTxBase64(ctx context.Context, payer solanago.PublicKey, ixs []solanago.Instruction) (string, error) {
	blockhash, err := s.rpc.GetLatestBlockhash(ctx)
	if err != nil {
		return "", fmt.Errorf("recurring: get recent blockhash: %w", err)
	}
	tx, err := solanago.NewTransaction(ixs, blockhash, solanago.TransactionPayer(payer))
	if err != nil {
		return "", fmt.Errorf("recurring: build transaction: %w", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("recurring: serialize transaction: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// readInitID reads the i64 LE initId from the SubscriptionAuthority account data.
func readInitID(data []byte) (int64, error) {
	end := subscriptionAuthorityInitIDOffset + 8
	if len(data) < end {
		return 0, fmt.Errorf("recurring: subscription authority data too short (%d bytes, need %d)", len(data), end)
	}
	return int64(binary.LittleEndian.Uint64(data[subscriptionAuthorityInitIDOffset:end])), nil
}
