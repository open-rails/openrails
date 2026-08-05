package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
)

// TypeSolanaPull is the recurring on-chain pull (#674): one durable intent per
// (subscription row, period anchor). The signed tx SIGNATURE is persisted onto
// the intent BEFORE submission, so a crash between submit and record resolves
// by reading the chain for that signature and running the renewal repair —
// never a subscriber who paid on-chain and got nothing.
const TypeSolanaPull = "solana_pull"

// SolanaPullIdempotencyKey addresses one pull episode: the anchor is the
// row's persisted next_pull_at, which only advances when the period resolves
// (renewal, dunning reschedule, already-paid advance).
func SolanaPullIdempotencyKey(rowID uuid.UUID, nextPullAt time.Time) string {
	return fmt.Sprintf("%s:%s:%d", TypeSolanaPull, rowID, nextPullAt.UTC().Unix())
}

// SolanaPullPayload is the stored payload for TypeSolanaPull.
type SolanaPullPayload struct {
	SubscriptionPDA string    `json:"subscription_pda"`
	RowID           uuid.UUID `json:"row_id"`
	NextPullAt      time.Time `json:"next_pull_at"`
}

// SolanaTxReader is the chain-read surface the verify leg needs (satisfied by
// *solana.RPCClient).
type SolanaTxReader interface {
	GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error)
}

// SolanaPullIntentHandler drives the crank state machine through the intent
// ledger. Effectively-once on-chain is double-walled: the ledger dedupes
// intents per period AND the subscriptions program itself rejects a second
// pull in an already-paid period (Custom:400) — so a re-crank after an
// unresolved read can never move money twice. What the intent ADDS is the
// pre-submit signature record: the renewal repair runs off it after any crash.
type SolanaPullIntentHandler struct {
	Core   *SolanaCrankWorker // DB/Config/Clock/Cranker/Lifecycle (no Intents)
	Store  *intents.Store     // pre-submit signature write-ahead
	Chain  SolanaTxReader     // nil tolerated: verification degrades to re-crank
	Policy intents.BackoffPolicy
}

func NewSolanaPullIntentHandler(core *SolanaCrankWorker, store *intents.Store, chain SolanaTxReader) *SolanaPullIntentHandler {
	return &SolanaPullIntentHandler{Core: core, Store: store, Chain: chain, Policy: intents.DefaultBackoff}
}

func (h *SolanaPullIntentHandler) Type() string { return TypeSolanaPull }
func (h *SolanaPullIntentHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func decodeSolanaPullPayload(intent gen.OpenrailsRailIntent) (SolanaPullPayload, error) {
	var p SolanaPullPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("solana pull intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode solana pull payload: %w", err)
	}
	if p.SubscriptionPDA == "" || p.RowID == uuid.Nil || p.NextPullAt.IsZero() {
		return p, errors.New("solana pull payload is incomplete")
	}
	return p, nil
}

// recordedSignature reads the pre-submit signature off the intent's evidence.
func recordedSignature(intent gen.OpenrailsRailIntent) string {
	if len(intent.ResultEvidence) == 0 {
		return ""
	}
	var evidence struct {
		TransactionID string `json:"transaction_id"`
	}
	if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
		return ""
	}
	return evidence.TransactionID
}

func (h *SolanaPullIntentHandler) loadRow(ctx context.Context, p SolanaPullPayload) (*models.SolanaSubscription, error) {
	return solanasubs.NewSolanaSubscriptionRepo(h.Core.DB).GetBySubscriptionPDA(ctx, p.SubscriptionPDA)
}

// CheckRelevance: the pull applies while the row is still active ON the same
// period anchor. A cancel/expiry or an advanced next_pull_at (renewal applied,
// dunning rescheduled, already-paid advanced) supersedes the intent.
func (h *SolanaPullIntentHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (intents.Relevance, error) {
	p, err := decodeSolanaPullPayload(intent)
	if err != nil {
		return intents.SupersededBy("unusable solana pull intent: " + err.Error()), nil
	}
	row, err := h.loadRow(ctx, p)
	if err != nil {
		if db.IsNotFound(err) {
			return intents.SupersededBy("solana subscription row no longer exists"), nil
		}
		return intents.Relevance{}, err
	}
	if row.Status != models.SolanaSubscriptionActive {
		return intents.SupersededBy(fmt.Sprintf("solana subscription no longer active (status=%s)", row.Status)), nil
	}
	if row.NextPullAt.UTC().Unix() != p.NextPullAt.UTC().Unix() {
		return intents.SupersededBy("pull period advanced past this intent's anchor"), nil
	}
	return intents.StillRelevant(), nil
}

func (h *SolanaPullIntentHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	if h.Core == nil || h.Core.Cranker == nil || h.Core.Lifecycle == nil {
		return intents.Parked("solana cranker not fully wired (no cranker/lifecycle)")
	}
	p, err := decodeSolanaPullPayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	row, err := h.loadRow(ctx, p)
	if err != nil {
		return intents.Retryable("load solana subscription: " + err.Error())
	}
	repo := solanasubs.NewSolanaSubscriptionRepo(h.Core.DB)

	// A previously-recorded signature means a tx WAS signed (and possibly
	// landed). Resolve it via the chain BEFORE any re-pull: landed ⇒ renewal
	// repair; read failure ⇒ never crank blind on top of an unresolved send.
	if sig := recordedSignature(intent); sig != "" {
		result, verdict := h.checkSignature(ctx, sig)
		switch verdict {
		case sigVerdictUnknown:
			return intents.Ambiguous("recorded pull signature unresolved (chain read failed); refusing to re-pull blind")
		case sigVerdictLanded:
			if err := verifyPullMemoMatchesIntent(result, intent.ID); err != nil {
				return intents.Parked("recorded pull signature landed but " + err.Error())
			}
			return h.repairFromSignature(ctx, repo, row, sig)
		case sigVerdictNotLanded:
			// Verified not executed (reverted or never landed): safe to
			// re-crank — the program's period guard backstops a stale read.
		}
	}

	var recorded string
	presubmit := func(sig string) error {
		recorded = sig
		if h.Store == nil {
			return errors.New("intent store unavailable for signature write-ahead")
		}
		return h.Store.RecordProgress(ctx, intent.ID, map[string]any{"transaction_id": sig})
	}

	// The intent id is the #713 memo local-id: it exists durably BEFORE the
	// send, so the stamped pull is chain-recognizable even if every local send
	// record is lost.
	outcome, crankErr := h.Core.crankOne(ctx, repo, row, intent.ID, presubmit)
	if crankErr != nil {
		if recorded != "" || outcome.signature != "" {
			// The tx was signed (and its signature durably recorded) before the
			// failure: submit or local finalize may have half-happened. The
			// verifier resolves via the signature.
			return intents.Ambiguous("pull submitted but unresolved: " + crankErr.Error())
		}
		return intents.Retryable("pull attempt failed cleanly: " + crankErr.Error())
	}

	evidence := map[string]any{}
	for k, v := range outcome.evidence {
		evidence[k] = v
	}
	if outcome.signature != "" {
		evidence["transaction_id"] = outcome.signature
	}
	switch outcome.kind {
	case crankSucceeded:
		return intents.Succeeded(evidence)
	case crankAlreadyPaid:
		// Out-of-band payment (this intent never recorded a signature): mirror
		// the legacy advance; the reconcile worker (#258) repairs the ledger.
		evidence["repair"] = "advanced_without_local_renewal"
		return intents.Succeeded(evidence)
	case crankGhostExpired, crankCancelled, crankDunned:
		return intents.TerminalWithEvidence(outcome.reason, evidence)
	default:
		return intents.Ambiguous("unrecognized crank outcome")
	}
}

// Verify resolves an ambiguous pull via chain READS on the recorded
// signature: landed ⇒ renewal repair; verified not landed ⇒ the executor may
// re-crank (program period guard backstops); no signature ⇒ nothing was ever
// signed, plain retry.
func (h *SolanaPullIntentHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeSolanaPullPayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	sig := recordedSignature(intent)
	if sig == "" {
		return intents.Retryable("no signature recorded; nothing was submitted")
	}
	result, verdict := h.checkSignature(ctx, sig)
	switch verdict {
	case sigVerdictUnknown:
		return intents.Ambiguous("chain read failed; still unresolved")
	case sigVerdictNotLanded:
		return intents.Retryable("recorded signature verified not landed; safe to re-pull")
	}
	if err := verifyPullMemoMatchesIntent(result, intent.ID); err != nil {
		return intents.Parked("recorded pull signature landed but " + err.Error())
	}
	row, err := h.loadRow(ctx, p)
	if err != nil {
		return intents.Ambiguous("pull landed on-chain, but local row load failed: " + err.Error())
	}
	return h.repairFromSignature(ctx, solanasubs.NewSolanaSubscriptionRepo(h.Core.DB), row, sig)
}

// repairFromSignature runs the renewal repair for a pull CONFIRMED on-chain:
// RenewMembership (idempotent on the signature) + AdvanceAfterPull.
func (h *SolanaPullIntentHandler) repairFromSignature(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription, sig string) intents.Outcome {
	resolve := h.Core.resolvePlanFn
	if resolve == nil {
		resolve = h.Core.resolvePlan
	}
	plan, err := resolve(ctx, row)
	if err != nil {
		return intents.Ambiguous("pull landed on-chain, but plan resolution failed: " + err.Error())
	}
	if err := h.Core.finalizePull(ctx, repo, row, plan, sig); err != nil {
		return intents.Ambiguous("pull landed on-chain, but renewal repair failed: " + err.Error())
	}
	return intents.Succeeded(map[string]any{"transaction_id": sig, "verified_existing": true})
}

type sigVerdict int

const (
	sigVerdictUnknown sigVerdict = iota
	sigVerdictLanded
	sigVerdictNotLanded
)

// checkSignature reads the chain for the recorded signature. sigVerdictLanded
// reports a CONFIRMED, on-chain-successful transaction (result non-nil only
// then, for the #713 memo cross-check).
func (h *SolanaPullIntentHandler) checkSignature(ctx context.Context, signature string) (*rpc.GetTransactionResult, sigVerdict) {
	if h.Chain == nil {
		return nil, sigVerdictUnknown
	}
	sig, err := solanago.SignatureFromBase58(signature)
	if err != nil {
		// Not a real signature (corrupt evidence): nothing can have landed.
		return nil, sigVerdictNotLanded
	}
	result, err := h.Chain.GetTransaction(ctx, sig)
	if err != nil || result == nil {
		// gagliardetto's GetTransaction reports "not found" as an error too;
		// distinguishing it from transport failure is not reliable, so both are
		// UNKNOWN — the caller decides (executor refuses to re-pull blind on a
		// recorded signature; the program period guard covers a stale answer).
		if errors.Is(err, rpc.ErrNotFound) {
			return nil, sigVerdictNotLanded
		}
		return nil, sigVerdictUnknown
	}
	if result.Meta != nil && result.Meta.Err != nil {
		return nil, sigVerdictNotLanded // landed but REVERTED: no money moved
	}
	return result, sigVerdictLanded
}

// verifyPullMemoMatchesIntent applies the #713 verify rule to a landed pull:
// the stamped purchase memo must name THIS intent — a different local-id is
// cross-wired evidence, parked for operator triage, never auto-repaired.
//
// or#893: MemoRequired. OPENRAILS builds and signs the pull transaction and
// stamps the intent id on it before submission, and the signature being
// verified is the one we recorded pre-submit — so an unstamped tx at this
// signature is not "a pre-memo pull", it is not the transaction we built.
// Parking it is the safe direction; auto-repairing it is not.
func verifyPullMemoMatchesIntent(result *rpc.GetTransactionResult, intentID uuid.UUID) error {
	if result == nil || result.Transaction == nil {
		return nil
	}
	tx, err := result.Transaction.GetTransaction()
	if err != nil || tx == nil {
		return nil
	}
	return solanaint.VerifyPurchaseMemo(tx, intentID, solanaint.MemoRequired)
}
