package recurring

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	log "github.com/sirupsen/logrus"
)

// ErrInsufficientUSDC: the subscriber's ATA holds less than the first-period
// amount. An actionable user state, not an RPC failure: callers map it to a
// "buy USDC" code. InsufficientUSDCError carries the amounts.
var ErrInsufficientUSDC = errors.New("recurring: insufficient USDC balance for first period")

// InsufficientUSDCError carries the concrete have/need base-unit amounts so the
// HTTP layer can render "need $X, have $Y -> buy USDC". It wraps
// ErrInsufficientUSDC so errors.Is(err, ErrInsufficientUSDC) is true.
type InsufficientUSDCError struct {
	// HaveBaseUnits is the wallet's current USDC ATA balance (token base units).
	HaveBaseUnits uint64
	// NeedBaseUnits is the first-period amount required (token base units).
	NeedBaseUnits uint64
}

func (e *InsufficientUSDCError) Error() string {
	return fmt.Sprintf("recurring: insufficient USDC: have %d base units, need %d", e.HaveBaseUnits, e.NeedBaseUnits)
}

func (e *InsufficientUSDCError) Unwrap() error { return ErrInsufficientUSDC }

// subscriptionAuthorityInitIDOffset is the byte offset of the
// SubscriptionAuthority's initId (i64 LE), which a returning subscriber's
// subscribe echoes as ExpectedSubscriptionAuthInitID.
const subscriptionAuthorityInitIDOffset = 98

// authorityReadMaxAttempts / defaultAuthorityReadBackoff bound the
// read-after-write retry on the SubscriptionAuthority: a node may still lag a
// just-landed write and serve a missing or short account.
const (
	authorityReadMaxAttempts    = 10
	defaultAuthorityReadBackoff = time.Second
)

// prepareRPC reads account state, a recent blockhash and the subscriber's token
// balance for the pre-flight check.
type prepareRPC interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
	// GetTokenBalanceForMint returns the SPL token balance (base units) for the
	// owner's ATA of mint; 0 when the ATA does not exist.
	GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error)
}

// accountDataReader is the narrow read surface readAuthorityInitID needs (shared
// by both the subscribe + tier-change prepare paths). Kept separate from
// prepareRPC so the tier-change RPC (which has no balance read) still satisfies it.
type accountDataReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// PrepareSubscribeService builds the transaction the subscriber's wallet signs
// to start a recurring subscription. All instruction encoding stays here; the
// frontend only signs, sends and confirms.
type PrepareSubscribeService struct {
	submitter Submitter // resolves the merchant's merchant (plan owner / cranker) address
	// signer is the cranker key: it pre-signs the transfer slot of the atomic
	// [subscribe + transfer_subscription] bundle while the wallet completes the
	// fee-payer slot. It MUST be the merchant (cranker) key.
	signer  solanaint.Signer
	rpc     prepareRPC
	network string
	tokens  map[string]config.TokenConfig
	// authorityReadBackoff shortens the retry only in package tests; zero keeps
	// the production default.
	authorityReadBackoff time.Duration
}

// NewPrepareSubscribeService builds a PrepareSubscribeService. signer pre-signs
// the bundle's transfer slot and MUST be the key the Submitter resolves as the
// merchant address.
func NewPrepareSubscribeService(submitter Submitter, signer solanaint.Signer, rpc prepareRPC, network string, tokens ...map[string]config.TokenConfig) *PrepareSubscribeService {
	return &PrepareSubscribeService{submitter: submitter, signer: signer, rpc: rpc, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens))}
}

// PrepareSubscribeInput describes the plan + subscriber to enroll. Plan terms are
// the canonical, server-resolved values from the price's Solana config (never
// client-supplied amounts).
type PrepareSubscribeInput struct {
	MerchantID       billing.MerchantID
	SubscriberWallet string // the connected wallet (signer + fee payer)
	PlanID           uint64
	MintSymbol       string
	AmountBaseUnits  uint64
	PeriodHours      uint64
	PlanCreatedAt    int64

	// Reference is the checkout's Solana Pay REFERENCE (read-only, non-signer
	// account meta). The merchant co-signs the bundle carrying it, which binds
	// the first payment to that checkout (ConfirmEnrollment requires it), and the
	// reference poller finds the landed tx via getSignaturesForAddress.
	Reference string
}

// PrepareSubscribeResult is the set of unsigned transactions the wallet must sign
// next, plus the derived PDAs for the confirm step.
type PrepareSubscribeResult struct {
	// Transactions are base64-encoded unsigned transactions to sign+send in order.
	Transactions []string
	// The single transaction is the atomic co-signed [subscribe + first-period
	// transfer] bundle, prefixed with initialize_subscription_authority for a
	// first-time subscriber (via the UNKNOWN_INIT_ID sentinel).
	//
	// AuthorityExists reports whether the SubscriptionAuthority already existed.
	AuthorityExists bool
	MerchantAddress string
	PlanPDA         string
	SubscriptionPDA string
	AuthorityPDA    string
	Mint            string
}

// Prepare derives the PDAs, checks for the subscriber's SubscriptionAuthority on
// this mint, and returns the one transaction to sign: the atomic [subscribe +
// transfer] bundle, with initialize_subscription_authority prepended for a
// first-time subscriber.
func (s *PrepareSubscribeService) Prepare(ctx context.Context, in PrepareSubscribeInput) (*PrepareSubscribeResult, error) {
	if in.SubscriberWallet == "" {
		return nil, fmt.Errorf("recurring: subscriber wallet is required")
	}
	if in.AmountBaseUnits == 0 || in.PeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid plan terms (amount/period)")
	}

	mintStr, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens)
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
	merchant, err := s.submitter.MerchantAddress(ctx, in.MerchantID)
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

	// Bounded retry: right after a bundle lands the node may not yet serve the
	// authority. Absent after the retries means first-time subscriber.
	initID, exists, err := readAuthorityInitIDWithBackoff(ctx, s.authorityBackoff(), s.rpc, saPDA)
	if err != nil {
		return nil, err
	}

	// Pre-flight: the bundle pulls the full first period, so an underfunded
	// wallet gets a typed error before signing. Fail-open on an RPC blip: the
	// atomic tx reverts on-chain if the balance is short.
	if err := s.preflightBalance(ctx, subscriber, mint, in.AmountBaseUnits); err != nil {
		return nil, err
	}

	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	if err != nil {
		return nil, fmt.Errorf("recurring: derive event authority: %w", err)
	}

	// One-step signup: a first-time subscriber has no SubscriptionAuthority, so
	// initialize_subscription_authority rides the same tx and subscribe passes
	// UNKNOWN_INIT_ID (the program checks init_id against the current slot,
	// which the same-tx init satisfies). Init, subscribe and first pull land or
	// revert together. A returning subscriber echoes its real init_id.
	expectedInitID := initID
	var ixs []solanago.Instruction
	if !exists {
		expectedInitID = subscriptions.UnknownInitID
		ixs = append(ixs, subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
			Owner:                 subscriber,
			SubscriptionAuthority: saPDA,
			TokenMint:             mint,
			UserATA:               subscriberATA,
			TokenProgram:          solanago.TokenProgramID,
		}))
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
		ExpectedSubscriptionAuthInitID: expectedInitID,
	})

	// Atomic co-signed subscribe: the first pull rides the subscribe tx, as in
	// the upgrade bundle. The cranker pre-signs the transfer's caller slot; the
	// wallet completes the fee-payer slot. The wallet could send subscribe alone,
	// so ConfirmEnrollment requires this pull in the landed tx.
	receiverATA, _, err := subscriptions.DeriveATA(merchant, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive receiver ata: %w", err)
	}
	transferIx := subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA:       subPDA,
		PlanPDA:               planPDA,
		SubscriptionAuthority: saPDA,
		DelegatorATA:          subscriberATA,
		ReceiverATA:           receiverATA,
		Caller:                merchant,
		Mint:                  mint,
		TokenProgram:          solanago.TokenProgramID,
		EventAuthority:        eventAuth,
		Amount:                in.AmountBaseUnits, // first period = full plan amount
		Delegator:             subscriber,
	})
	ixs = append(ixs, subscribeIx, transferIx)

	if s.signer == nil {
		return nil, fmt.Errorf("recurring: atomic subscribe requires a cranker signer")
	}
	// Carry the checkout's reference (its own instruction — see
	// referenceTagInstruction): it binds the bundle to the checkout and lets the
	// poller detect it.
	ixs, err = withReference(ixs, subscriber, in.Reference)
	if err != nil {
		return nil, err
	}
	tx, err := solanaint.BuildPartiallySignedTx(ctx, in.MerchantID, s.signer, s.rpc, subscriber, ixs)
	if err != nil {
		return nil, err
	}
	result.Transactions = []string{tx}
	result.AuthorityExists = exists
	return result, nil
}

// preflightBalance returns *InsufficientUSDCError when the subscriber's balance
// is below needBaseUnits. Fail-open on a read failure: the atomic tx reverts
// on-chain if the balance is truly short.
func (s *PrepareSubscribeService) preflightBalance(ctx context.Context, subscriber, mint solanago.PublicKey, needBaseUnits uint64) error {
	if needBaseUnits == 0 {
		return nil
	}
	have, err := solanaint.ReadUntilConsistent(ctx, solanaint.ReadUntilConsistentOpts{Attempts: 2},
		func(ctx context.Context) (uint64, error) { return s.rpc.GetTokenBalanceForMint(ctx, subscriber, mint) },
		func(b uint64) bool { return b >= needBaseUnits },
	)
	if err != nil {
		// Either the balance is genuinely short (predicate never satisfied) or the
		// read failed transiently. Distinguish: if we got a value < need, it is the
		// insufficient case; otherwise an RPC blip -> log + proceed (fail-open).
		if have < needBaseUnits && have > 0 {
			return &InsufficientUSDCError{HaveBaseUnits: have, NeedBaseUnits: needBaseUnits}
		}
		if have == 0 {
			// Zero is ambiguous (no ATA OR lagging read). Treat as insufficient when
			// the read SUCCEEDED with 0 (no ATA), but proceed on a read failure.
			if isReadFailure(err) {
				log.WithError(err).Warn("recurring: USDC pre-flight balance read failed; proceeding (atomic tx is the guarantee)")
				return nil
			}
			return &InsufficientUSDCError{HaveBaseUnits: 0, NeedBaseUnits: needBaseUnits}
		}
		return nil
	}
	return nil
}

// isReadFailure reports whether a ReadUntilConsistent error was an underlying RPC
// read failure (every attempt errored) rather than a satisfied-vs-short predicate
// outcome. ReadUntilConsistent prefixes the former with "never succeeded" and
// surfaces a canceled context distinctly.
func isReadFailure(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "never succeeded")
}

// readInitID reads the i64 LE initId from the SubscriptionAuthority account data.
func readInitID(data []byte) (int64, error) {
	end := subscriptionAuthorityInitIDOffset + 8
	if len(data) < end {
		return 0, fmt.Errorf("recurring: subscription authority data too short (%d bytes, need %d)", len(data), end)
	}
	var v int64
	if err := binary.Read(bytes.NewReader(data[subscriptionAuthorityInitIDOffset:end]), binary.LittleEndian, &v); err != nil {
		return 0, fmt.Errorf("recurring: read initId: %w", err)
	}
	return v, nil
}

// readAuthorityInitID reads the SubscriptionAuthority initId, retrying within a
// bound because a node may lag a just-landed write. It returns (initId, true,
// nil) when readable; (0, false, nil) when empty on every attempt (first-time
// subscriber); an error on an RPC failure or an account that stayed too short.
func readAuthorityInitID(ctx context.Context, rpc accountDataReader, saPDA solanago.PublicKey) (int64, bool, error) {
	return readAuthorityInitIDWithBackoff(ctx, defaultAuthorityReadBackoff, rpc, saPDA)
}

func readAuthorityInitIDWithBackoff(
	ctx context.Context,
	backoff time.Duration,
	rpc accountDataReader,
	saPDA solanago.PublicKey,
) (int64, bool, error) {
	var lastShort int
	sawShort := false
	for attempt := 0; attempt < authorityReadMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, false, fmt.Errorf("recurring: subscription authority read canceled: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}
		data, err := rpc.GetAccountData(ctx, saPDA)
		if err != nil {
			return 0, false, fmt.Errorf("recurring: read subscription authority: %w", err)
		}
		if len(data) == 0 {
			continue // absent OR not-yet-visible: keep retrying within the bound
		}
		if len(data) < subscriptionAuthorityInitIDOffset+8 {
			// Account exists but is shorter than the initId offset — likely a
			// partially-visible read under RPC lag; retry within the bound.
			sawShort = true
			lastShort = len(data)
			continue
		}
		initID, err := readInitID(data)
		if err != nil {
			return 0, false, err
		}
		return initID, true, nil
	}
	if sawShort {
		return 0, false, fmt.Errorf(
			"recurring: subscription authority never settled (last read %d bytes, need %d) after %d attempts",
			lastShort, subscriptionAuthorityInitIDOffset+8, authorityReadMaxAttempts)
	}
	// Empty on every attempt: a first-time subscriber.
	return 0, false, nil
}

func (s *PrepareSubscribeService) authorityBackoff() time.Duration {
	if s.authorityReadBackoff > 0 {
		return s.authorityReadBackoff
	}
	return defaultAuthorityReadBackoff
}
