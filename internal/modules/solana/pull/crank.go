package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Cranker signs and sends a pull (satisfied by *recurring.CrankService):
// presubmit(sig) persists the signed tx signature before submission, and
// memoLocalID (the durable pull-intent id) is stamped on the pull tx as its
// self-recognition memo.
type Cranker interface {
	CrankWithPresubmit(ctx context.Context, merchantID billing.MerchantID, sub *models.SolanaSubscription, amountBaseUnits uint64, memoLocalID uuid.UUID, presubmit func(signature string) error) (string, error)
}

// Lifecycle is the membership surface a pull drives (satisfied by
// *subscriptions.SubscriptionLifecycleService): renew on a confirmed pull, fail
// (-> dunning) on a charge failure.
type Lifecycle interface {
	RenewMembership(ctx context.Context, params *subscriptions.RenewMembershipParams) error
	FailMembership(ctx context.Context, params *subscriptions.FailMembershipParams) error
	// CancelMembership terminates the subscription (an out-of-band cancel
	// detected at rebill time: the subscriber revoked the delegation on-chain).
	CancelMembership(ctx context.Context, params *subscriptions.CancelMembershipParams) error
	ApplyLocalUnknown(ctx context.Context, d *db.DB, sub *models.Subscription, from ...models.SubscriptionStatus) error
}

// solanaSubStore is the persistence surface crankOne mutates (satisfied by
// *solanasubs.SolanaSubscriptionRepo).
type solanaSubStore interface {
	SetStatus(ctx context.Context, id uuid.UUID, status string) error
	SetNextPullAt(ctx context.Context, id uuid.UUID, nextPullAt time.Time) error
	AdvanceAfterPull(ctx context.Context, id, subscriptionID uuid.UUID, episodeAt, periodStart time.Time, signature string, nextPullAt time.Time) error
}

// resolvedPlan is the per-subscription billing terms crankOne acts on: the
// on-chain pull amount + period + ghost-plan fingerprint, plus the fiat
// amount/currency recorded on renewal.
type resolvedPlan struct {
	priceID, productID    uuid.UUID
	description           string
	accessDurationHours   *int
	providerAccessDefault bool
	quantity              *int
	amountBaseUnits       uint64
	periodHours           uint64
	fingerprint           int64
	fiatAmount            int64
	currency              string
	// retryAttempts is the subscription's consecutive-failure count so far.
	retryAttempts int
	// The subscription a failed pull is an attempt of.
	customerID, pspID uuid.UUID
	policy            models.CollectionPolicy
	periodEnd         *time.Time
}

// crankKind classifies a completed (error-free) crankOne for the intent ledger.
type crankKind int

const (
	crankSucceeded       crankKind = iota // pulled + renewed + advanced
	crankUnqualifiedPaid                  // paid on-chain without a qualified accepted receipt
	crankCanceled                         // delegate revoked → membership canceled (no dunning)
	crankDunned                           // recoverable decline → FailMembership + rescheduled
	crankGhostExpired                     // ghost plan → row expired
	crankNotDue                           // the chain's clock had not reached the period end
)

// crankOutcome is crankOne's classified result. signature is set on success
// (and, via the presubmit hook, durably recorded before submission).
type crankOutcome struct {
	kind      crankKind
	signature string
	reason    string
	evidence  map[string]any
	// retryAt is when a crankNotDue pull is tried again.
	retryAt time.Time
}

// crankOne runs the pull state machine for one row. presubmit
// durably records the signed tx signature before submission. memoLocalID (the
// pull intent id; Nil = unstamped) is stamped on the pull tx as its SPL Memo,
// so a chain reader can resolve the tx to the intent with no local send
// record. An error means the pull is unresolved (operational failure, or a
// landed pull whose local finalize failed; the recorded signature tells them
// apart); nil means the period resolved (see crankKind).
func (h *Handler) crankOne(ctx context.Context, repo solanaSubStore, row *models.SolanaSubscription, plan resolvedPlan, memoLocalID uuid.UUID, presubmit func(signature string) error) (crankOutcome, error) {
	merchantID := billing.MerchantID(row.MerchantID)

	amountBaseUnits := plan.amountBaseUnits
	periodHours := plan.periodHours
	if periodHours == 0 || periodHours > 24*366 {
		return crankOutcome{}, errors.New("invalid accepted Solana period duration")
	}
	fingerprint := plan.fingerprint

	// Ghost-plan guard: the plan was deleted + recreated at the same PDA. The
	// on-chain pull would fail PlanTermsMismatch; terminate this record.
	if fingerprint != 0 && row.PlanCreatedAtFingerprint != 0 && fingerprint != row.PlanCreatedAtFingerprint {
		log.WithContext(ctx).WithField("subscription_pda", row.SubscriptionPDA).
			Warn("Solana cranker: plan created_at fingerprint mismatch (ghost plan); expiring")
		if err := repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionExpired); err != nil {
			return crankOutcome{}, err
		}
		return crankOutcome{kind: crankGhostExpired, reason: "plan fingerprint mismatch (ghost plan); subscription expired"}, nil
	}

	if presubmit == nil {
		return crankOutcome{}, errors.New("Solana pull requires its durable submission fence")
	}
	var submitted string
	sig, crankErr := h.Cranker.CrankWithPresubmit(ctx, merchantID, row, amountBaseUnits, memoLocalID, func(signature string) error {
		err := presubmit(signature)
		if err == nil {
			submitted = signature
		}
		return err
	})
	if crankErr != nil {
		// ClassifyCrankError maps the on-chain error onto the billing
		// decline-code vocabulary and an action. Devnet codes: Custom:400 =
		// period already paid (idempotent); token OwnerMismatch (Custom:4) =
		// delegate revoked (terminal); token InsufficientFunds (Custom:1) =
		// recoverable; RPC/gas = operational.
		cf := recurring.ClassifyCrankError(crankErr)
		var chainAt *time.Time
		if cf.Category != recurring.Operational {
			failed, verdict := checkSolanaSignature(ctx, h.Chain, submitted)
			if verdict != sigVerdictNotLanded {
				return crankOutcome{}, fmt.Errorf("refused pull has no qualified nonexecution receipt: %w", crankErr)
			}
			raw, err := json.Marshal(failed.Meta.Err)
			if err != nil {
				return crankOutcome{}, fmt.Errorf("decode qualified pull failure: %w", err)
			}
			crankErr = fmt.Errorf("on-chain pull reverted: %s", raw)
			cf = recurring.ClassifyCrankError(crankErr)
			if failed.BlockTime != nil {
				at := failed.BlockTime.Time().UTC()
				chainAt = &at
			}
		}
		llog := log.WithContext(ctx).WithError(crankErr).WithFields(log.Fields{
			"subscription_pda": row.SubscriptionPDA,
			"decline_code":     string(cf.Code),
			"onchain_code":     cf.OnChainCode,
		})
		switch cf.Category {
		case recurring.Operational:
			// RPC/network or the cranker wallet out of SOL gas: retry next run, NEVER
			// dun — a shared outage would wrongly past-due a merchant's whole book.
			// Leave next_pull_at unchanged so it stays due.
			llog.Warn("Solana cranker: operational pull failure; retry next run (no dunning)")
			return crankOutcome{}, crankErr
		case recurring.AlreadyPaid:
			if chainAt != nil && chainAt.Before(row.NextPullAt) {
				// The chain's clock had not reached the period end: the paid
				// period is the one recorded. Pull again once it has.
				lag := row.NextPullAt.Sub(*chainAt)
				llog.WithField("chain_lag", lag.String()).Warn("Solana cranker: pull reached the chain before its period end; retrying")
				return crankOutcome{kind: crankNotDue, reason: fmt.Sprintf("the chain's clock was %s short of the period end", lag),
					retryAt: h.now().Add(lag + solanasubs.PullMargin)}, nil
			}
			// A refused duplicate pull proves no money moved in this attempt.
			// It proves neither who paid the other transaction nor its accepted
			// agreement. Preserve uncertainty, expose it, and never invent cash
			// or move the paid period from the current catalog/clock.
			reason := "period already paid on-chain without a qualified accepted receipt; review the original episode"
			llog.Warn(reason)
			sub, err := subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, row.SubscriptionID)
			if err != nil {
				return crankOutcome{}, err
			}
			if err := h.Lifecycle.ApplyLocalUnknown(ctx, h.DB, sub); err != nil {
				return crankOutcome{}, err
			}
			if err := alerting.RecordLedgerRepair(ctx, h.DB, h.now(), alerting.LedgerRepair{Provider: string(models.RailSolana),
				Operation: "solana_pull_receipt_required", IdempotencyKey: memoLocalID.String(), SubscriptionID: &row.SubscriptionID,
				Err: errors.New(reason), Metadata: map[string]any{"subscription_pda": row.SubscriptionPDA, "episode_at": row.NextPullAt}}); err != nil {
				return crankOutcome{}, err
			}
			return crankOutcome{kind: crankUnqualifiedPaid, reason: reason, evidence: map[string]any{"needs_qualification": true, "not_executed": true, "decline_code": string(cf.Code)}}, nil

		case recurring.Terminal:
			// The subscriber canceled in their wallet (cancel_subscription, or
			// the token delegate revoked): the paid period has ended, so cancel
			// and stop, never dun.
			llog.Warn("Solana cranker: terminal pull failure (canceled on-chain); cancelling subscription (no dunning)")
			if err := h.recordPullAttempt(ctx, row, plan, memoLocalID, string(cf.Code), crankErr.Error()); err != nil {
				return crankOutcome{}, err
			}
			if err := repo.SetStatus(ctx, row.ID, models.SolanaSubscriptionCanceled); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: set canceled status: %w", err)
			}
			subID := row.SubscriptionID
			proc := models.RailSolana
			reason := fmt.Sprintf("%s (%s)", crankErr.Error(), cf.Code)
			if err := h.Lifecycle.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
				Rail:           &proc,
				SubscriptionID: &subID,
				CancelType:     models.CancelTypeUser,
				CancelFeedback: &reason,
				RevokeAccess:   true,
			}); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: cancel membership: %w", err)
			}
			return crankOutcome{
				kind:     crankCanceled,
				reason:   reason,
				evidence: map[string]any{"decline_code": string(cf.Code)},
			}, nil
		default:
			// Recoverable subscriber decline (insufficient USDC, etc.) ->
			// dunning, on the membership's retry schedule.
			llog.Warn("Solana cranker: recoverable pull failure; routing to dunning")
			reason := crankErr.Error()
			code := string(cf.Code)
			subID := row.SubscriptionID
			if err := h.recordPullAttempt(ctx, row, plan, memoLocalID, code, reason); err != nil {
				return crankOutcome{}, err
			}
			if plan.periodEnd == nil {
				return crankOutcome{}, errors.New("Solana decline has no accepted paid boundary")
			}
			start := plan.periodEnd.UTC()
			accepted := &subscriptions.RenewalTerms{Rail: models.RailSolana, PSPID: row.PspID, SubscriptionID: row.SubscriptionID,
				CustomerID: plan.customerID, FromPriceID: plan.priceID, FromProductID: plan.productID, PriceID: plan.priceID,
				ProductID: plan.productID, ProductName: plan.description, Amount: plan.fiatAmount, Currency: plan.currency,
				Quantity: plan.quantity, AccessDurationHours: plan.accessDurationHours, ProviderAccessDefault: plan.providerAccessDefault, PeriodStart: start, PeriodEnd: start.Add(time.Duration(periodHours) * time.Hour)}
			if err := h.Lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
				Prepared:        accepted,
				Rail:            models.RailSolana,
				SubscriptionID:  &subID,
				FailureReason:   &reason,
				FailureCode:     &code,
				FailureID:       memoLocalID,
				AttemptRecorded: true,
			}); err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: fail membership: %w", err)
			}
			// The next pull is the membership's next retry, which FailMembership
			// set from the case's dunning policy. None means that failure ended
			// it; one period on keeps this record from hot-looping meanwhile.
			failed, err := subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, subID)
			if err != nil {
				return crankOutcome{}, fmt.Errorf("solana crank: reload membership: %w", err)
			}
			var nextRetry time.Time
			if failed.NextRetryAt != nil {
				nextRetry = failed.NextRetryAt.UTC()
			} else {
				periodHoursI64, err := safecast.Convert[int64](periodHours)
				if err != nil {
					return crankOutcome{}, fmt.Errorf("solana crank: period hours overflow: %w", err)
				}
				nextRetry = h.now().Add(time.Duration(periodHoursI64) * time.Hour)
			}
			if err := repo.SetNextPullAt(ctx, row.ID, nextRetry); err != nil {
				return crankOutcome{}, err
			}
			return crankOutcome{
				kind:     crankDunned,
				reason:   reason,
				evidence: map[string]any{"decline_code": code},
			}, nil
		}
	}

	return crankOutcome{kind: crankSucceeded, signature: sig}, nil
}

// finalizePull is the renewal repair for a confirmed on-chain pull: renew the
// membership window (payment row, period advance, notifications; idempotent
// on the tx signature) and advance the row past the pulled period. Shared by
// the inline success path and the recorded-signature verify leg.
func (h *Handler) finalizePull(ctx context.Context, repo solanaSubStore, agreement *Agreement, receipt *recurring.PullReceipt, execution *observed.Execution) error {
	row := &agreement.Row
	ctx = db.WithPSPID(ctx, row.PspID)
	hours, err := safecast.Convert[int](agreement.PeriodHours)
	if err != nil {
		return err
	}
	kind := payments.AttemptRenewal
	payment := &models.Payment{CustomerID: agreement.CustomerID, SubscriptionID: &row.SubscriptionID, PspID: &row.PspID,
		Rail: models.RailSolana, TransactionID: receipt.Signature, Amount: agreement.Amount, ListAmount: agreement.Amount,
		Currency: agreement.Currency, Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail,
		PurchasedAt: receipt.PeriodStart, AttemptKind: &kind, Quantity: agreement.Quantity,
		Metadata: map[string]any{"solana_subscriber_wallet": row.SubscriberWallet, "solana_subscription_pda": row.SubscriptionPDA,
			"solana_token_mint": row.Mint, "solana_token_amount": strconv.FormatUint(receipt.Amount, 10), "solana_treasury": row.Treasury,
			"solana_pull_intent": execution.IntentID.String(), "solana_pull_episode_at": agreement.EpisodeAt.UTC().Format(time.RFC3339Nano)},
	}
	unit := agreement.Amount / int64(subscriptions.SeatCount(agreement.Quantity))
	bill := &observed.Bill{Execution: execution, PSPID: row.PspID, CustomerID: agreement.CustomerID, SubscriptionID: &row.SubscriptionID,
		Owner: observed.OwnerEngine, Kind: "renewal", SourceKind: solanasubs.PullType, SourceKey: execution.IntentID.String(), ProviderReference: row.SubscriptionPDA,
		Currency: agreement.Currency, Total: agreement.Amount, PeriodStart: receipt.PeriodStart, PeriodEnd: receipt.PeriodEnd, DueAt: agreement.EpisodeAt,
		Payment: payment, Settled: true, SettlementSource: "cash",
		Lines: []observed.Line{{PriceID: &agreement.PriceID, ProductID: &agreement.ProductID, Description: agreement.Description,
			UnitAmount: unit, Amount: agreement.Amount, Quantity: agreement.Quantity, BillingIntervalHours: &hours, AccessDurationHours: agreement.AccessDurationHours, ProviderAccessDefault: agreement.ProviderAccessDefault}},
	}
	if err := h.Lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{Rail: models.RailSolana, RailSubscriptionID: row.SubscriptionPDA,
		ProviderBill: bill, TransactionID: receipt.Signature, Amount: agreement.Amount, AmountProvided: true, Currency: agreement.Currency,
		CurrentPeriodStartsAt: &receipt.PeriodStart, CurrentPeriodEndsAt: &receipt.PeriodEnd, PurchasedAt: &receipt.PeriodStart}); err != nil {
		return fmt.Errorf("solana crank: settle verified pull: %w", err)
	}
	return repo.AdvanceAfterPull(ctx, row.ID, row.SubscriptionID, agreement.EpisodeAt, receipt.PeriodStart, receipt.Signature, receipt.PeriodEnd)
}

// recordPullAttempt records a refused on-chain pull as its cycle's attempt:
// the cycle's rebill, or a dunning retry after an earlier failure. It is keyed
// on the pull's durable intent (one per subscription and pull slot), so a
// crank retried before the lifecycle applied the failure records nothing new.
// The failpoint after it lets tests crash there.
func (h *Handler) recordPullAttempt(ctx context.Context, row *models.SolanaSubscription, plan resolvedPlan, pullIntent uuid.UUID, code, text string) error {
	if plan.periodEnd == nil {
		return nil // no paid period: nothing came due
	}
	var intent *uuid.UUID
	if pullIntent != uuid.Nil {
		intent = &pullIntent
	}
	if err := attempts.Record(ctx, h.DB.Gen(ctx), attempts.Attempt{
		MerchantID: row.MerchantID, CustomerID: plan.customerID, PSPID: plan.pspID, Rail: string(models.RailSolana),
		Kind: attempts.RebillKind(false, plan.retryAttempts), Owner: attempts.OwnerOf(plan.policy),
		Answer: decline.Evidence{Code: code, Text: text}, Amount: plan.fiatAmount, Currency: plan.currency, At: h.now(),
		SubscriptionID: &row.SubscriptionID, Cycle: &attempts.Cycle{SubscriptionID: row.SubscriptionID, DueAt: *plan.periodEnd},
		ProviderIntentID: intent, Step: "pull",
	}); err != nil {
		return err
	}
	return failpoint.Hit(ctx, failpoint.Site{Point: failpoint.AfterAttempt, Kind: solanasubs.PullType, Operation: pullIntent, Subscription: row.SubscriptionID})
}

// resolvePlan loads the on-chain pull amount + period + fingerprint and the fiat
// amount/currency for the subscription's price.
func (h *Handler) resolvePlan(ctx context.Context, row *models.SolanaSubscription) (resolvedPlan, error) {
	subRepo := subscriptions.NewSubscriptionRepo(h.DB)
	sub, err := subRepo.GetByID(ctx, row.SubscriptionID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load subscription: %w", err)
	}

	// A due scheduled change applies before the pull plan resolves: OpenRails
	// itself picks the amount withdrawn on-chain.
	if err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(d)
		locked, err := repo.GetByIDForUpdate(ctx, sub.ID)
		if err != nil {
			return err
		}
		change, err := subscriptions.PendingChange(ctx, d, locked.ID)
		if err != nil || !change.IsDue(h.now()) || change.FromPriceID != locked.PriceID {
			return err
		}
		price, err := catalog.NewPriceService(d).GetByID(ctx, change.PriceID)
		if err != nil {
			return err
		}
		locked.PriceID, locked.ProductID = price.ID, price.ProductID
		if err := repo.UpdateAt(ctx, locked, h.now()); err != nil {
			return err
		}
		sub = locked
		return subscriptions.ApplyChange(ctx, d, change.ID, h.now())
	}); err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: apply the scheduled change: %w", err)
	}

	price, err := catalog.NewPriceService(h.DB).GetByID(ctx, sub.PriceID)
	if err != nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: load price: %w", err)
	}
	retryAttempts := 0
	if sub.RetryAttempts != nil {
		retryAttempts = *sub.RetryAttempts
	}
	cfg := price.ForPSP(sub.PspID).PSPLinkForRail(models.RailSolana)
	if cfg == nil {
		return resolvedPlan{}, fmt.Errorf("solana crank: price %s has no solana rail config", price.ID)
	}
	amountBaseUnits, err := strconv.ParseUint(cfg["amount_base_units"], 10, 64)
	if err != nil || amountBaseUnits == 0 {
		return resolvedPlan{}, fmt.Errorf("solana crank: invalid amount_base_units for price %s", price.ID)
	}
	periodHours, err := strconv.ParseUint(cfg["period_hours"], 10, 64)
	if err != nil || periodHours == 0 {
		return resolvedPlan{}, fmt.Errorf("solana crank: invalid period_hours for price %s", price.ID)
	}
	product, err := catalog.NewProductService(h.DB).GetByID(ctx, price.ProductID)
	if err != nil {
		return resolvedPlan{}, err
	}
	fingerprint, _ := strconv.ParseInt(cfg["created_at"], 10, 64)
	return resolvedPlan{
		priceID: price.ID, productID: price.ProductID, description: product.DisplayName,
		accessDurationHours: sub.AccessDurationHoursSnapshot, providerAccessDefault: sub.ProviderAccessDefaultSnapshot, quantity: subscriptions.CloneQuantity(sub.Quantity),
		amountBaseUnits: amountBaseUnits,
		periodHours:     periodHours,
		fingerprint:     fingerprint,
		fiatAmount:      price.Amount,
		currency:        price.Currency,
		retryAttempts:   retryAttempts,
		customerID:      sub.CustomerID,
		pspID:           sub.PspID,
		policy:          sub.CollectionPolicy,
		periodEnd:       sub.CurrentPeriodEndsAt,
	}, nil
}
