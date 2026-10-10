package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/apperr"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerrecovery"
)

// Store persists intents on the ledger. Producers call Enqueue/Supersede from
// request contexts (merchant-pinned connections, explicit merchant stamp);
// the Runner's claims and transitions run on the worker pool.
type Store struct {
	db *db.DB
	// ceiling, when set, gates every destructive enqueue before the write-ahead
	// row is created. nil = ungated (unit tests). It carries its own root pool
	// DB, so it survives tx-rebind (withTxDB).
	ceiling *RateCeiling
}

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// NewStoreGated builds a Store whose destructive enqueues pass the rate
// ceiling, preserved across tx-rebinds (withTxDB).
func NewStoreGated(d *db.DB, ceiling *RateCeiling) *Store {
	return &Store{db: d, ceiling: ceiling}
}

// withTxDB rebinds this Store onto a tx-scoped DB, keeping the rate ceiling
// (whose pool-backed DB is independent of the tx), so an enqueue committed
// with the caller's write stays gated.
func (s *Store) withTxDB(txdb *db.DB) *Store {
	return &Store{db: txdb, ceiling: s.ceiling}
}

// EnqueueParams describes one logical intent. MerchantID is stamped explicitly;
// IdempotencyKey makes the enqueue effectively-once (see the query's conflict
// semantics: pending refreshed, superseded/expired revived, rest untouched).
type EnqueueParams struct {
	// operationID is set only after validating a new engine renewal admission.
	// Existing operations always retain their stored identity.
	operationID    uuid.UUID
	MerchantID     uuid.UUID
	Provider       string
	IntentType     string
	SubscriptionID *uuid.UUID
	PaymentID      *uuid.UUID
	PriceID        *uuid.UUID
	// PspID is the PSP the outbound write is addressed to. Required unless the
	// intent is custodian-addressed: an intent nobody can attribute cannot be
	// executed against the right credentials.
	PspID uuid.UUID
	// CustodianID addresses the write to a custodian instead (the batch account
	// updater uploads to a custodian that backs many PSPs). Per
	// provider_intents_addressed_check, one of the two must be set.
	CustodianID    uuid.UUID
	Payload        any
	IdempotencyKey string
	NextAttemptAt  time.Time
	Origin         Origin
	OriginReason   string
	// Actor is the authenticated principal id (admin user id / self-service
	// customer id) that produced this intent, stamped on the row and keying the
	// per-actor ceiling. Empty = resolved from the admitted principal on the
	// context; system/background paths carry none.
	Actor     string
	ExpiresAt *time.Time
}

// Enqueue records the intent (idempotent) and returns the canonical row for
// its idempotency key.
func (s *Store) Enqueue(ctx context.Context, p EnqueueParams) (gen.BillingProviderIntent, error) {
	scope, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	if scope.UUID() != p.MerchantID {
		return gen.BillingProviderIntent{}, errors.New("intent merchant does not match context")
	}
	if p.IntentType == subscriptions.TypeInitialMembership {
		return s.enqueueInitialMembership(ctx, p)
	}
	if p.IntentType == TypeNMIPaymentMethodUpdate {
		return s.enqueueNMIMethodUpdate(ctx, p)
	}
	if p.IntentType == TypeNMIPaymentMethodDelete {
		return s.enqueueNMIMethodDelete(ctx, p)
	}
	if p.IntentType == TypeHyperSwitchMethodDelete {
		return gen.BillingProviderIntent{}, errors.New("custodian deletion requires owned method admission")
	}
	if p.IntentType == "nmi_sale" {
		return s.enqueueSale(ctx, p)
	}
	if p.IntentType != "nmi_upgrade" && p.IntentType != subscriptions.TypeManualRebill && p.IntentType != subscriptions.TypeSubscriptionCollection {
		return s.enqueue(ctx, p)
	}
	if p.SubscriptionID == nil {
		return gen.BillingProviderIntent{}, errors.New("recurring operation requires a subscription")
	}
	var engineCustomer uuid.UUID
	if p.IntentType == subscriptions.TypeSubscriptionCollection {
		observed, err := subscriptions.NewSubscriptionRepo(s.db).GetByID(ctx, *p.SubscriptionID)
		if err != nil {
			return gen.BillingProviderIntent{}, err
		}
		engineCustomer = observed.CustomerID
	}
	var row gen.BillingProviderIntent
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		if engineCustomer != uuid.Nil {
			if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: p.MerchantID, ID: engineCustomer}); err != nil {
				return err
			}
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, *p.SubscriptionID)
		if err != nil {
			return err
		}
		if sub.MerchantID != p.MerchantID {
			return errors.New("recurring operation merchant does not match subscription")
		}
		bound := s.withTxDB(d)
		// Existing keys still return their original operation; the caller's owned
		// payload validation decides whether it is the same accepted request.
		prior, err := bound.GetByIdempotencyKey(ctx, p.IdempotencyKey)
		if err == nil {
			row = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if p.IntentType == subscriptions.TypeSubscriptionCollection {
			if sub.CollectionPolicy != "engine" || sub.CustomerID != engineCustomer {
				return errors.New("engine operation requires engine scheduling ownership")
			}
			owner, err := d.Gen(ctx).GetUnresolvedSubscriptionCollection(ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
			if err == nil {
				return fmt.Errorf("subscription already owned by accepted engine operation %s", owner.ID)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			payload, ok := p.Payload.(subscriptions.SubscriptionCollectionPayload)
			if !ok || p.PspID == uuid.Nil || ((p.Origin != OriginSystem || payload.Initiator != charge.InitiatorMerchant) && (p.Origin != OriginUser || payload.Initiator != charge.InitiatorCustomer || p.Actor != engineCustomer.String())) {
				return errors.New("engine admission requires typed system-owned terms")
			}
			if p.CustodianID != uuid.Nil {
				handle := paymentmethods.CustodianHandle{Custodian: p.CustodianID, Method: payload.Instrument.RailMethodRef}
				if err := paymentmethods.LockCustodianHandles(ctx, d.Gen(ctx), p.MerchantID, handle); err != nil {
					return err
				}
				if err := paymentmethods.RequireCustodianHandleAvailable(ctx, d.Gen(ctx), p.MerchantID, handle); err != nil {
					return err
				}
			}
			method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: p.MerchantID, ID: payload.PaymentMethodID})
			if err != nil {
				return err
			}
			charged, err := subscriptions.PaymentMethodOf(ctx, d.Gen(ctx), sub)
			if err != nil {
				return err
			}
			if charged == nil || method.ID != *charged || method.CustomerID != engineCustomer || !paymentmethods.Chargeable(method) {
				return errors.New("engine admission payment method changed")
			}
			if err := payload.Instrument.Matches(method); err != nil {
				return err
			}
			if err := mandates.Recheck(ctx, d.Gen(ctx), p.MerchantID, payload.Instrument.Mandate); err != nil {
				return err
			}
			if !subscriptions.EngineCollectionDue(sub, payload.AcceptedAt, p.Origin == OriginUser) || !sub.CurrentPeriodEndsAt.Equal(payload.PreviousPeriodEnd) {
				return errors.New("engine admission is not a due obligation")
			}
			failures := 0
			if sub.RetryAttempts != nil {
				failures = *sub.RetryAttempts
			}
			if payload.FailureCount != failures {
				return errors.New("engine admission changed the accepted failure count")
			}
			terms, err := PrepareEngineRenewalTerms(ctx, d, sub, payload.AcceptedAt)
			if err != nil {
				return err
			}
			terms, err = subscriptions.SelectEngineRenewalPeriod(terms, payload.AcceptedAt)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(terms, payload.Renewal) {
				return errors.New("engine admission differs from the accepted recurring agreement")
			}
			latest, err := d.Gen(ctx).GetLatestSubscriptionCollectionForPeriod(ctx, gen.GetLatestSubscriptionCollectionForPeriodParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, PreviousPeriodEnd: payload.PreviousPeriodEnd})
			ordinal := 0
			if err == nil {
				if latest.Status != StatusFailedTerminal {
					return errors.New("previous engine obligation has not been released")
				}
				if err := ValidateSubscriptionCollectionTerminal(latest); err != nil {
					return err
				}
				previous, err := subscriptions.DecodeSubscriptionCollectionPayload(latest)
				if err != nil {
					return err
				}
				ordinal = previous.Attempt + 1
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if p.Origin == OriginUser && ordinal == 0 {
				return ErrRebillNotRetryable
			}
			if payload.Attempt != ordinal {
				return errors.New("engine admission changed its accepted attempt ordinal")
			}
			p.operationID = subscriptions.SubscriptionCollectionOperationID(p.MerchantID, p.PspID, payload)
		}
		if p.IntentType == "nmi_upgrade" {
			if err := subscriptions.RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
				return err
			}
		} else {
			_, err := d.Gen(ctx).GetLiveTierChangeProviderIntent(ctx, gen.GetLiveTierChangeProviderIntentParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
			if err == nil {
				return subscriptions.ErrRebillTermsCommitted
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		row, err = bound.enqueue(ctx, p)
		if err == nil && p.IntentType == subscriptions.TypeSubscriptionCollection {
			accepted, decodeErr := subscriptions.DecodeSubscriptionCollectionPayload(row)
			if decodeErr != nil {
				return decodeErr
			}
			charged, err := subscriptions.PaymentMethodOf(ctx, d.Gen(ctx), sub)
			if err != nil {
				return err
			}
			if accepted.Renewal.CustomerID != sub.CustomerID || charged == nil || accepted.PaymentMethodID != *charged || accepted.Instrument.PSPID != sub.PspID {
				return errors.New("engine admission contradicts locked subscription")
			}
		}
		if err != nil || p.IntentType != subscriptions.TypeNMIUpgrade {
			return err
		}
		// A different subscription can win the merchant-wide key while this
		// subscription is locked. Return its untouched canonical row to the
		// caller's mandatory ownership check before reading its instrument.
		if row.IntentType != p.IntentType || row.SubscriptionID == nil || *row.SubscriptionID != *p.SubscriptionID ||
			(p.PriceID != nil && (row.PriceID == nil || *row.PriceID != *p.PriceID)) {
			return nil
		}
		accepted, err := subscriptions.DecodeNMIUpgradePayload(row)
		if err != nil {
			return err
		}
		// This share lock participates in the same short admission transaction.
		// A remap that won first causes rollback; a later remap observes the
		// committed operation and cannot alter its accepted instrument.
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: row.MerchantID, ID: accepted.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID.String() != accepted.UserID || method.Rail != row.Rail || !paymentmethods.Chargeable(method) {
			return apperr.Conflictf("payment method changed before upgrade admission")
		}
		if accepted.Instrument.Matches(method) != nil || mandates.Recheck(ctx, d.Gen(ctx), row.MerchantID, accepted.Instrument.Mandate) != nil {
			return apperr.Conflictf("payment method changed before upgrade admission")
		}
		return nil
	})
	return row, err
}

func (s *Store) enqueue(ctx context.Context, p EnqueueParams) (gen.BillingProviderIntent, error) {
	if p.IntentType == TypeNMIPaymentMethodDelete || p.IntentType == TypeHyperSwitchMethodDelete {
		if err := validatePaymentMethodDeleteAuthority(ctx, p); err != nil {
			return gen.BillingProviderIntent{}, err
		}
	}
	if p.IntentType == "" || p.IdempotencyKey == "" {
		return gen.BillingProviderIntent{}, fmt.Errorf("intents: enqueue requires intent_type and idempotency_key")
	}
	// The intent names the account it will execute against: a PSP, or a
	// custodian for writes addressed to one. An explicit value wins; otherwise
	// the PSP the caller already pinned on ctx. An intent nobody can attribute
	// cannot be executed against the right credentials.
	if p.CustodianID == uuid.Nil {
		p.CustodianID = db.CustodianIDFromContext(ctx)
	}
	if p.PspID == uuid.Nil && p.CustodianID == uuid.Nil {
		psp, err := db.RequirePSPID(ctx)
		if err != nil {
			return gen.BillingProviderIntent{}, fmt.Errorf("intents: enqueue %s: %w", p.IntentType, err)
		}
		p.PspID = psp
	}
	var payload []byte
	if p.Payload != nil {
		b, err := json.Marshal(p.Payload)
		if err != nil {
			return gen.BillingProviderIntent{}, fmt.Errorf("intents: marshal payload: %w", err)
		}
		payload = b
	}
	var originReason *string
	if p.OriginReason != "" {
		originReason = &p.OriginReason
	}
	// Resolve the actor (explicit override, else the admitted invoker) up
	// front: it is stamped on the row and keys the per-invoker ceiling. The
	// subject and credential it acted with are audit only.
	actor := ResolveActor(ctx, p.Actor)
	var subject, credential *string
	if who, ok := admitted(ctx); ok {
		subject, credential = textOrNil(who.Subject), textOrNil(billingauth.CredentialName(who))
	}

	// The rate ceiling gates destructive ops before the write-ahead intent is
	// created: a trip or an evaluation error refuses, and with no row the op
	// does not happen.
	if s.ceiling != nil {
		if err := s.ceiling.Check(ctx, CheckParams{
			Actor:      actor,
			MerchantID: p.MerchantID,
			IntentType: p.IntentType,
			Origin:     p.Origin,
		}, time.Now().UTC()); err != nil {
			return gen.BillingProviderIntent{}, err
		}
	}

	var actorPtr *string
	if actor != "" {
		actorPtr = &actor
	}
	// psp_id is stamped only when the producer already has observed
	// provenance (for example an existing subscription pinned to an account).
	var row gen.BillingProviderIntent
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = s.db.NewWithPgxTx(tx).Gen(ctx).EnqueueProviderIntent(ctx, gen.EnqueueProviderIntentParams{
			ID:             uuidPtrOrNil(p.operationID),
			MerchantID:     p.MerchantID,
			Rail:           p.Provider,
			IntentType:     p.IntentType,
			SubscriptionID: p.SubscriptionID,
			PaymentID:      p.PaymentID,
			PriceID:        p.PriceID,
			Payload:        payload,
			IdempotencyKey: p.IdempotencyKey,
			NextAttemptAt:  p.NextAttemptAt.UTC(),
			Origin:         string(p.Origin),
			OriginReason:   originReason,
			Actor:          actorPtr,
			Subject:        subject,
			Credential:     credential,
			ExpiresAt:      p.ExpiresAt,
			PspID:          uuidPtrOrNil(p.PspID),
			CustodianID:    uuidPtrOrNil(p.CustodianID),
		})
		if err != nil {
			return err
		}
		if OperationTerminal(row.Status) {
			return nil
		}
		return s.db.InsertRiverJobTx(ctx, tx, OperationArgs{MerchantID: row.MerchantID, IntentID: row.ID}, operationInsertOpts(row.NextAttemptAt))
	})
	return row, err
}

// SupersedeBySubject marks every live (pending / failed_retryable /
// unknown_needs_verify) intent of the type for the subscription superseded.
// in_flight rows are left to their executor, whose relevance re-check is the
// authoritative guard.
func (s *Store) SupersedeBySubject(ctx context.Context, intentType string, subscriptionID uuid.UUID, reason string) (int64, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return 0, scopeErr
	}
	return s.db.Gen(ctx).SupersedeProviderIntentsBySubject(ctx, gen.SupersedeProviderIntentsBySubjectParams{
		MerchantID:     scopeMerchantID.UUID(),
		IntentType:     intentType,
		SubscriptionID: &subscriptionID,
		Reason:         &reason,
	})
}

// ClaimByID leases ONE specific intent for the synchronous execute path.
// ok=false means the row is not claimable (terminal, expired, or leased by a
// live executor) — the caller inspects the canonical row instead.
func (s *Store) ClaimByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.BillingProviderIntent, bool, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return gen.BillingProviderIntent{}, false, scopeErr
	}
	row, err := s.db.Gen(ctx).ClaimProviderIntentByID(ctx, gen.ClaimProviderIntentByIDParams{
		MerchantID:     scopeMerchantID.UUID(),
		ID:             id,
		Now:            now.UTC(),
		LeaseExpiresAt: leaseUntil.UTC(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.BillingProviderIntent{}, false, nil
		}
		return gen.BillingProviderIntent{}, false, err
	}
	return row, true, nil
}

// Get returns the intent row by id.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (gen.BillingProviderIntent, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return gen.BillingProviderIntent{}, scopeErr
	}
	return s.db.Gen(ctx).GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: scopeMerchantID.UUID(), ID: id})
}

// ClaimDue leases up to batch due executable intents (SKIP LOCKED). It must
// run on a merchant-pinned connection and claims only that merchant's intents.
func (s *Store) ClaimDue(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.BillingProviderIntent, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if err := s.db.AssertMerchantScope(ctx, "intent executor claim"); err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ClaimDueProviderIntents(ctx, gen.ClaimDueProviderIntentsParams{
		MerchantID:     scopeMerchantID.UUID(),
		Now:            now.UTC(),
		LeaseExpiresAt: leaseUntil.UTC(),
		BatchSize:      batch,
	})
}

// RenewClaim extends a live lease the caller still owns: the row must still
// carry the claim's status and attempts. false means the lease lapsed or
// another executor claimed the row; this one must send nothing more.
func (s *Store) RenewClaim(ctx context.Context, id uuid.UUID, status string, attempts int32, now, leaseUntil time.Time) (bool, error) {
	ctx, release, err := s.db.WithIndependentMerchantConn(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return false, scopeErr
	}
	n, err := s.db.Gen(ctx).RenewProviderIntentClaim(ctx, gen.RenewProviderIntentClaimParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Status: status, Attempts: attempts, Now: now.UTC(), LeaseExpiresAt: leaseUntil.UTC(),
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClaimDueVerify leases up to batch due unknown_needs_verify intents. Same
// merchant-pin requirement as ClaimDue.
func (s *Store) ClaimDueVerify(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.BillingProviderIntent, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if err := s.db.AssertMerchantScope(ctx, "intent verifier claim"); err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ClaimDueVerifyProviderIntents(ctx, gen.ClaimDueVerifyProviderIntentsParams{
		MerchantID:     scopeMerchantID.UUID(),
		Now:            now.UTC(),
		LeaseExpiresAt: leaseUntil.UTC(),
		BatchSize:      batch,
	})
}

// ClaimUnknownByID leases one unknown operation for operator resolution.
// ok=false means it is not unknown or another worker holds its lease.
func (s *Store) ClaimUnknownByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.BillingProviderIntent, bool, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return gen.BillingProviderIntent{}, false, scopeErr
	}
	row, err := s.db.Gen(ctx).ClaimUnknownProviderIntentByID(ctx, gen.ClaimUnknownProviderIntentByIDParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Now: now.UTC(), LeaseExpiresAt: leaseUntil.UTC(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.BillingProviderIntent{}, false, nil
	}
	if err != nil {
		return gen.BillingProviderIntent{}, false, err
	}
	return row, true, nil
}

// ReleaseUnknownClaim atomically drops a resolver lease and wakes verification
// at the operation's retained due time, without changing its status or evidence.
func (s *Store) ReleaseUnknownClaim(ctx context.Context, id uuid.UUID) (bool, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return false, scopeErr
	}
	rows, err := s.transitionAndWake(ctx, id, func(ctx context.Context, txs *Store) (int64, time.Time, error) {
		next, err := txs.db.Gen(ctx).ReleaseUnknownProviderIntentClaim(ctx, gen.ReleaseUnknownProviderIntentClaimParams{MerchantID: scopeMerchantID.UUID(), ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, time.Time{}, nil
		}
		if err != nil {
			return 0, time.Time{}, err
		}
		return 1, next, nil
	})
	return rows == 1 && err == nil, err
}

// ExpireOverdue expires every live intent whose relevance window elapsed,
// except destructive intents whose merchant has an open held_bulk finding:
// breaker-held intents never expire out from under the operator.
func (s *Store) ExpireOverdue(ctx context.Context, now time.Time) (int64, error) {
	if err := s.db.AssertMerchantScope(ctx, "intent expiry sweep"); err != nil {
		return 0, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return 0, scopeErr
	}
	return s.db.Gen(ctx).ExpireOverdueProviderIntents(ctx, gen.ExpireOverdueProviderIntentsParams{
		MerchantID:       scopeMerchantID.UUID(),
		Now:              now.UTC(),
		BreakerHeldTypes: DestructiveIntentTypes(),
	})
}

func (s *Store) MarkSucceeded(ctx context.Context, id uuid.UUID, now time.Time, evidence map[string]any) error {
	if err := refuseCustodyKeys(evidence); err != nil {
		return err
	}

	var ev []byte
	if len(evidence) > 0 {
		b, err := json.Marshal(evidence)
		if err != nil {
			return fmt.Errorf("intents: marshal evidence: %w", err)
		}
		ev = b
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	return oneTerminal(s.db.Gen(ctx).MarkProviderIntentSucceeded(ctx, gen.MarkProviderIntentSucceededParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Now: now.UTC(), ResultEvidence: ev,
	}))
}

// pruneEvidenceKeys are the result-pointer keys a slim succeeded tombstone
// keeps by default. The rest of the evidence is already in
// provider_mutation_logs, unless the handler's PrunePolicy keeps it.
var pruneEvidenceKeys = []string{"transaction_id", "response_code"}

// PruneSucceeded slims a just-succeeded intent to a dedupe tombstone: the
// payload is dropped and result_evidence reduced to pruneEvidenceKeys, unless
// the handler's PrunePolicy keeps them. The row itself stays: it is the
// effectively-once tombstone (UNIQUE merchant_id+idempotency_key), and
// deleting it would let a re-enqueue re-run the provider mutation. A no-op
// unless the row is still succeeded; a failed prune leaves the full row, which
// is safe.
func (s *Store) PruneSucceeded(ctx context.Context, id uuid.UUID, evidence map[string]any, keepPayload, keepEvidence bool) error {
	if err := refuseCustodyKeys(evidence); err != nil {
		return err
	}
	if keepPayload && keepEvidence {
		return nil // nothing to slim
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("intents: prune succeeded intent: %w", err)
	}
	q := s.db.Gen(ctx)
	if keepEvidence {
		// Drop the payload only; leave result_evidence intact for the handler's
		// post-success readers.
		return q.PruneSucceededProviderIntentPayload(ctx, gen.PruneSucceededProviderIntentPayloadParams{ID: id, MerchantID: mid.UUID()})
	}
	var ev []byte
	if slim := slimEvidence(evidence); len(slim) > 0 {
		b, err := json.Marshal(slim)
		if err != nil {
			return fmt.Errorf("intents: marshal slim evidence: %w", err)
		}
		ev = b
	}
	if keepPayload {
		return q.PruneSucceededProviderIntentEvidence(ctx, gen.PruneSucceededProviderIntentEvidenceParams{ID: id, MerchantID: mid.UUID(), Evidence: ev})
	}
	return q.PruneSucceededProviderIntent(ctx, gen.PruneSucceededProviderIntentParams{ID: id, MerchantID: mid.UUID(), Evidence: ev})
}

// PruneTerminalPayload removes a short-lived credential from a terminal
// intent while retaining its status, reason, and non-sensitive evidence.
func (s *Store) PruneTerminalPayload(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("intents: prune terminal intent: %w", err)
	}
	return s.db.Gen(ctx).PruneTerminalProviderIntentPayload(ctx, gen.PruneTerminalProviderIntentPayloadParams{ID: id, MerchantID: mid.UUID()})
}

// slimEvidence keeps only pruneEvidenceKeys (when present) off a succeeded
// intent's evidence; returns nil when none are present so the column is set
// NULL rather than to an empty object.
func slimEvidence(evidence map[string]any) map[string]any {
	if len(evidence) == 0 {
		return nil
	}
	slim := map[string]any{}
	for _, k := range pruneEvidenceKeys {
		if v, ok := evidence[k]; ok {
			slim[k] = v
		}
	}
	if len(slim) == 0 {
		return nil
	}
	return slim
}

// RecordProgress merges keys into result_evidence on a live (non-terminal)
// intent. Handlers use it to pin provider references (e.g. a signed Solana tx
// signature) before the side effect is sent, so a crash mid-send resolves via
// a provider read keyed on the recorded reference.
func (s *Store) RecordProgress(ctx context.Context, id uuid.UUID, keys map[string]any) error {
	if _, ok := keys["initial_submitted"]; ok {
		return errors.New("initial submission fence is write-once")
	}
	if err := refuseCustodyKeys(keys); err != nil {
		return err
	}

	if len(keys) == 0 {
		return nil
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("intents: marshal progress evidence: %w", err)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("intents: record progress: %w", err)
	}
	return s.db.Gen(ctx).RecordProviderIntentProgress(ctx, gen.RecordProviderIntentProgressParams{ID: id, MerchantID: mid.UUID(), Progress: b})
}

// RecordProgressIfAbsent writes one write-ahead progress marker atomically.
// It is intentionally narrower than RecordProgress: a provider step's
// "started" marker must never be overwritten by a racing executor or by a
// retry with a different operation identity. The returned boolean is false
// when the key already exists (the caller must reconcile that step instead of
// sending another provider request).
func (s *Store) RecordProgressIfAbsent(ctx context.Context, id uuid.UUID, key string, value any) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "initial_submitted" && value != true {
		return false, errors.New("initial submission fence must be true")
	}
	if err := refuseCustodyKeys(map[string]any{key: value}); err != nil {
		return false, err
	}
	if key == "" {
		return false, fmt.Errorf("intents: progress key is required")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("intents: marshal progress value: %w", err)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, fmt.Errorf("intents: record progress: %w", err)
	}
	n, err := s.db.Gen(ctx).RecordProviderIntentProgressIfAbsent(ctx, gen.RecordProviderIntentProgressIfAbsentParams{ID: id, MerchantID: mid.UUID(), Key: key, Value: b})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) MarkFailedRetryable(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	rows, err := s.transitionAndWake(ctx, id, func(ctx context.Context, txs *Store) (int64, time.Time, error) {
		rows, err := txs.db.Gen(ctx).MarkProviderIntentFailedRetryable(ctx, gen.MarkProviderIntentFailedRetryableParams{
			MerchantID: scopeMerchantID.UUID(), ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
		})
		return rows, nextAttemptAt, err
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("intent retry transition did not commit")
	}
	return nil
}

func (s *Store) MarkUnknown(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string, evidence map[string]any) error {
	if err := refuseCustodyKeys(evidence); err != nil {
		return err
	}

	var raw []byte
	if len(evidence) > 0 {
		var err error
		raw, err = json.Marshal(evidence)
		if err != nil {
			return fmt.Errorf("marshal provider receipt: %w", err)
		}
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	return one(s.transitionAndWake(ctx, id, func(ctx context.Context, txs *Store) (int64, time.Time, error) {
		rows, err := txs.db.Gen(ctx).MarkProviderIntentUnknown(ctx, gen.MarkProviderIntentUnknownParams{
			MerchantID: scopeMerchantID.UUID(), ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason, ResultEvidence: raw,
		})
		wake := nextAttemptAt
		if held, _ := evidence["recovery_held"].(bool); held {
			wake = time.Time{} // durable immediate readiness check; the held retry date remains
		}
		return rows, wake, err
	}))
}

// MarkFailedTerminal records an unretryable failure. evidence (optional)
// carries structured forensics — e.g. the gateway decline response code that
// the dunning worker's decline classification reads back off the ledger.
func (s *Store) MarkFailedTerminal(ctx context.Context, id uuid.UUID, reason string, evidence map[string]any) error {
	if err := refuseCustodyKeys(evidence); err != nil {
		return err
	}

	var ev []byte
	if len(evidence) > 0 {
		b, err := json.Marshal(evidence)
		if err != nil {
			return fmt.Errorf("intents: marshal evidence: %w", err)
		}
		ev = b
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	return oneTerminal(s.db.Gen(ctx).MarkProviderIntentFailedTerminal(ctx, gen.MarkProviderIntentFailedTerminalParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Reason: &reason, ResultEvidence: ev,
	}))
}

func (s *Store) Park(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	return s.park(ctx, id, nextAttemptAt, reason, nil)
}

// ParkForRecovery marks a known-unsent operation for the catch-up wakeup.
func (s *Store) ParkForRecovery(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	return s.park(ctx, id, nextAttemptAt, reason, []byte(`{"recovery_held":true}`))
}

func (s *Store) park(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string, recovery []byte) error {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	return one(s.transitionAndWake(ctx, id, func(ctx context.Context, txs *Store) (int64, time.Time, error) {
		wake := nextAttemptAt
		if len(recovery) > 0 {
			wake = time.Time{}
		}
		rows, err := txs.db.Gen(ctx).ParkProviderIntent(ctx, gen.ParkProviderIntentParams{
			MerchantID: scopeMerchantID.UUID(), ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
			RecoveryEvidence: recovery,
		})
		if err != nil || rows != 0 {
			return rows, wake, err
		}
		// The SQL fence refuses to return a submitted payment to pending.
		// Retain it as unknown inside this same transition/wakeup transaction.
		current, err := txs.Get(ctx, id)
		if db.IsNotFound(err) {
			return 0, time.Time{}, nil
		}
		if err != nil || current.Status != StatusInFlight {
			return 0, time.Time{}, err
		}
		submitted, err := hasSubmissionEvidence(current)
		if err != nil {
			return 0, time.Time{}, err
		}
		if submitted {
			// The live-state predicate still protects a concurrently sealed outcome.
			rows, err := txs.db.Gen(ctx).MarkProviderIntentUnknown(ctx, gen.MarkProviderIntentUnknownParams{
				MerchantID: scopeMerchantID.UUID(), ID: id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
				ResultEvidence: recovery,
			})
			return rows, wake, err
		}
		return 0, time.Time{}, nil
	}))
}

func (s *Store) MarkSuperseded(ctx context.Context, id uuid.UUID, reason string) error {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	return one(s.db.Gen(ctx).MarkProviderIntentSuperseded(ctx, gen.MarkProviderIntentSupersededParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Reason: &reason,
	}))
}

// Terminal transitions must actually commit one row; a stale or prohibited
// update is not a successful operation and must not trigger result pruning.
func oneTerminal(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("intent terminal transition did not commit")
	}
	return nil
}

// one normalizes :execrows transitions: 0 rows means the row raced into a
// state the transition no longer applies to — not an error, the next sweep
// re-evaluates.
func one(rows int64, err error) error {
	if err != nil {
		return err
	}
	return nil
}

// uuidPtrOrNil maps the zero uuid to a NULL column: the two addressing columns
// are nullable and constrained as a pair, so "unset" must reach the DB as NULL
// rather than as a zero uuid no row could ever reference.
func uuidPtrOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// GetByIdempotencyKey reads the immutable operation for a request replay.
func (s *Store) GetByIdempotencyKey(ctx context.Context, key string) (gen.BillingProviderIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	return s.db.Gen(ctx).GetProviderIntentByIdempotencyKey(ctx, gen.GetProviderIntentByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: key})
}

// LiveTierChange returns the unresolved tier change (NMI upgrade, Stripe tier
// change or engine upgrade) that owns the subscription; db.IsNotFound when none does.
func (s *Store) LiveTierChange(ctx context.Context, subscriptionID uuid.UUID) (gen.BillingProviderIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	return s.db.Gen(ctx).GetLiveTierChangeProviderIntent(ctx, gen.GetLiveTierChangeProviderIntentParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID})
}

// CheckRecovery holds writes on an inherited account until provider observation
// and financial receipt recovery complete. First purchases and card enrollment
// keep their own request idempotency; they are not old-obligation recovery.
func (s *Store) CheckRecovery(ctx context.Context, in gen.BillingProviderIntent, now time.Time) error {
	switch in.IntentType {
	case subscriptions.TypeInitialMembership, "nmi_sale", TypeNMICardVault:
		return nil
	}
	if in.PspID == nil {
		return providerrecovery.CheckMerchant(ctx, s.db, in.MerchantID, now)
	}
	return providerrecovery.CheckPSP(ctx, s.db, in.MerchantID, *in.PspID, now)
}

// textOrNil is v, or nil when it is blank.
func textOrNil(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}
