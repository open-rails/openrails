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
	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// ErrInsufficientUSDC is the typed, sentinel error returned by the pre-flight
// balance check (#286 part A) when the subscriber's USDC ATA holds less than the
// first-period amount. HTTP/checkout callers map this to a clear client-facing
// "buy USDC" code (it is NOT an internal/RPC failure — it is an actionable user
// state). Use errors.Is(err, ErrInsufficientUSDC) to detect it. The concrete
// amounts (have/need) are attached via InsufficientUSDCError for the frontend.
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

// subscriptionAuthorityInitIDOffset is the byte offset of the SubscriptionAuthority
// account's initId field (an i64 LE). The subscribe instruction must echo this
// value back as ExpectedSubscriptionAuthInitID; it is only knowable AFTER
// init_subscription_authority has executed and written the account. Proven by
// the devnet lifecycle test (reads offset 98 between init and subscribe).
const subscriptionAuthorityInitIDOffset = 98

// authorityReadMaxAttempts / authorityReadBackoff bound the read-after-write
// retry on the SubscriptionAuthority account (#274). After init_subscription_authority
// confirms, the RPC node serving the re-prepare may still lag behind that write,
// so a single GetAccountData can observe a missing or short account. We retry up
// to ~10 attempts ~1s apart (capped, context-aware) until the account is present
// AND long enough to read initId.
const authorityReadMaxAttempts = 10

// authorityReadBackoff is a var (not const) only so tests can shrink the delay;
// production keeps the ~1s read-after-write pacing.
var authorityReadBackoff = time.Second

// prepareRPC is the minimal RPC surface PrepareSubscribe needs (satisfied by
// *solanaint.RPCClient): read on-chain account state + a recent blockhash to
// build unsigned transactions, and read the subscriber's USDC ATA balance for
// the pre-flight check (#286).
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
	submitter Submitter // resolves the merchant's merchant (plan owner / cranker) address
	// signer is the cranker key. The subscribe step (#286) is now an ATOMIC
	// co-signed bundle [subscribe + transfer_subscription(first period)]; the
	// transfer requires the cranker as the caller-signer, so this signer pre-signs
	// that slot via BuildPartiallySignedTx while the wallet completes the
	// subscribe/fee-payer slot. It MUST be the same key as the cranker/merchant.
	signer  solanaint.Signer
	rpc     prepareRPC
	network string
	tokens  map[string]config.TokenConfig
}

// NewPrepareSubscribeService builds a PrepareSubscribeService. signer is the
// cranker key used to pre-sign the atomic subscribe bundle's transfer slot (#286)
// and MUST be the same key the Submitter resolves as the merchant address.
func NewPrepareSubscribeService(submitter Submitter, signer solanaint.Signer, rpc prepareRPC, network string, tokens ...map[string]config.TokenConfig) *PrepareSubscribeService {
	return &PrepareSubscribeService{submitter: submitter, signer: signer, rpc: rpc, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens))}
}

// PrepareSubscribeInput describes the plan + subscriber to enroll. Plan terms are
// the canonical, server-resolved values from the price's Solana config (never
// client-supplied amounts).
type PrepareSubscribeInput struct {
	MerchantID       merchant.ID
	SubscriberWallet string // the connected wallet (signer + fee payer)
	PlanID           uint64
	MintSymbol       string
	AmountBaseUnits  uint64
	PeriodHours      uint64
	PlanCreatedAt    int64

	// Reference, when set, attaches a Solana Pay REFERENCE (read-only, non-signer
	// account meta) to the returned step's instruction so the reference poller can
	// find the landed tx via getSignaturesForAddress(reference). Used by the Solana
	// Pay transaction-request subscribe path (a QR scan); empty for the
	// wallet-connected subscribe (the browser confirms by signature directly).
	Reference string
}

// PrepareSubscribeResult is the set of unsigned transactions the wallet must sign
// next, plus the derived PDAs for the confirm step.
type PrepareSubscribeResult struct {
	// Transactions are base64-encoded unsigned transactions to sign+send in order.
	Transactions []string
	// Step is "init" when the returned tx is init_subscription_authority and the
	// caller must re-prepare afterwards for the subscribe tx; "subscribe" when the
	// returned tx is the ATOMIC co-signed bundle [subscribe + transfer(first
	// period)] (the cranker pre-signed the transfer slot; the wallet completes the
	// fee-payer slot). The subscribe step is the final on-chain step before confirm
	// and pulls the first period IN THE SAME TX (#286).
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

	mintStr, _, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens)
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

	// Read the authority with a bounded, read-after-write-tolerant retry (#274):
	// on the re-prepare right after init confirms, the RPC node may not yet serve
	// the just-written account, so we retry until it is present and readable rather
	// than racing a single read.
	initID, exists, err := readAuthorityInitID(ctx, s.rpc, saPDA)
	if err != nil {
		return nil, err
	}

	// First-time subscriber for this mint: the authority must be initialized
	// before subscribe can be built (subscribe needs its initId). Return the init
	// tx; the caller signs+sends it, then re-prepares for the subscribe tx.
	if !exists {
		var initIx solanago.Instruction = subscriptions.BuildInitSubscriptionAuthority(subscriptions.InitSubscriptionAuthorityParams{
			Owner:                 subscriber,
			SubscriptionAuthority: saPDA,
			TokenMint:             mint,
			UserATA:               subscriberATA,
			TokenProgram:          solanago.TokenProgramID,
		})
		// Solana Pay subscribe: tag the init tx with the reference so the poller can
		// detect THIS landed init and advance the session to the subscribe step.
		initIx, err = withReferenceMeta(initIx, in.Reference)
		if err != nil {
			return nil, err
		}
		tx, err := s.buildUnsignedTxBase64(ctx, subscriber, []solanago.Instruction{initIx})
		if err != nil {
			return nil, err
		}
		result.Transactions = []string{tx}
		result.Step = "init"
		result.AuthorityExists = false
		return result, nil
	}

	// Authority exists and its initId was read above; build the subscribe tx.
	//
	// TODO(#274): the repeated-subscribe-by-same-wallet path can still fail
	// on-chain with Custom:519. The leading hypothesis is that the program expects
	// the authority's CURRENT counter rather than the original init_id passed as
	// ExpectedSubscriptionAuthInitID — i.e. the value at offset 98 may need to track
	// accumulated authority state across subscribes. This is NOT yet confirmed on
	// devnet; do NOT change the offset/semantics until a devnet root-cause check
	// (current authority counter vs original init_id) proves what value subscribe
	// must echo back. This change only makes the read robust against RPC lag.
	// Pre-flight USDC balance check (#286 part A). The atomic subscribe bundle
	// pulls the FULL first period in the same tx, so an underfunded wallet would
	// produce a tx that reverts. Catch it server-side BEFORE signing anything and
	// return a typed insufficient-USDC error the caller maps to a "buy USDC" code.
	// Best-effort: an RPC blip must not hard-fail the flow (the atomic tx is the
	// real guarantee — it reverts on chain if the balance is short).
	if err := s.preflightBalance(ctx, subscriber, mint, in.AmountBaseUnits); err != nil {
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

	// ATOMIC co-signed subscribe (#286 part B). Bundle the first pull
	// (transfer_subscription, full first-period amount) into the SAME tx as
	// subscribe — exactly like the upgrade bundle (prepare_tier_change.go). The
	// transfer requires the merchant/cranker as the caller-signer, so the cranker
	// pre-signs that slot via BuildPartiallySignedTx and the wallet completes the
	// subscribe/fee-payer slot. Both land or both revert -> no
	// "subscribed-but-not-charged" window; confirm just verifies the bundle landed.
	delegatorATA, _, err := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive delegator ata: %w", err)
	}
	receiverATA, _, err := subscriptions.DeriveATA(merchant, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive receiver ata: %w", err)
	}
	transferIx := subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA:       subPDA,
		PlanPDA:               planPDA,
		SubscriptionAuthority: saPDA,
		DelegatorATA:          delegatorATA,
		ReceiverATA:           receiverATA,
		Caller:                merchant,
		Mint:                  mint,
		TokenProgram:          solanago.TokenProgramID,
		EventAuthority:        eventAuth,
		Amount:                in.AmountBaseUnits, // first period = full plan amount
		Delegator:             subscriber,
	})

	if s.signer == nil {
		return nil, fmt.Errorf("recurring: atomic subscribe requires a cranker signer")
	}
	// Solana Pay subscribe: tag the subscribe instruction with the reference so the
	// poller can detect the landed atomic [subscribe+transfer] bundle and route it
	// to ConfirmEnrollment. Extra trailing read-only accounts are ignored by the
	// program, so the on-chain action is unchanged.
	var taggedSubscribeIx solanago.Instruction = subscribeIx
	taggedSubscribeIx, err = withReferenceMeta(taggedSubscribeIx, in.Reference)
	if err != nil {
		return nil, err
	}
	tx, err := solanaint.BuildPartiallySignedTx(ctx, in.MerchantID, s.signer, s.rpc, subscriber,
		[]solanago.Instruction{taggedSubscribeIx, transferIx})
	if err != nil {
		return nil, err
	}
	result.Transactions = []string{tx}
	result.Step = "subscribe"
	result.AuthorityExists = true
	return result, nil
}

// preflightBalance reads the subscriber's USDC ATA balance for mint and returns a
// typed *InsufficientUSDCError (wrapping ErrInsufficientUSDC) when it is below
// needBaseUnits. It is read-lag tolerant + fail-open: a transient RPC read error
// is logged and the flow PROCEEDS (the atomic tx reverts on chain if the balance
// is truly short, so the on-chain tx — not this check — is the real guarantee).
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
// surfaces a cancelled context distinctly.
func isReadFailure(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "never succeeded")
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
	return marshalUnsignedTxBase64(tx)
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

// readAuthorityInitID reads the SubscriptionAuthority initId with a bounded,
// read-after-write-tolerant retry (#274). It returns:
//
//   - (initId, true, nil)  when the account is present and long enough to read initId;
//   - (0, false, nil)      when the account is genuinely absent (first-time subscriber:
//     GetAccountData returns empty across every attempt) — the caller returns the init tx;
//   - (0, false, err)      on a hard RPC error, or when an account appeared but stayed
//     too short to read initId after all attempts (a clear "never settled" error).
//
// The distinction matters: an empty read is ambiguous (either truly first-time OR
// the just-written account not yet visible), so we keep retrying empties up to the
// bound. If it is still empty after the bound, we treat the subscriber as first-time
// and return the init tx — re-preparing again will retry the read. A present-but-short
// account is always an error (it should never happen once the account exists).
func readAuthorityInitID(ctx context.Context, rpc accountDataReader, saPDA solanago.PublicKey) (int64, bool, error) {
	var lastShort int
	sawShort := false
	for attempt := 0; attempt < authorityReadMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, false, fmt.Errorf("recurring: subscription authority read canceled: %w", ctx.Err())
			case <-time.After(authorityReadBackoff):
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
	// Stayed empty across every attempt → treat as a genuinely absent authority
	// (first-time subscriber): caller returns the init tx.
	return 0, false, nil
}
