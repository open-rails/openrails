package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/apperr"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Store persists intents on the ledger. Producers call Enqueue/Supersede from
// request contexts (merchant-pinned connections, RLS double-checks the stamp);
// the Runner's claims and transitions run on the worker pool.
type Store struct {
	db *db.DB
	// ceiling is the #732 anti-credential-compromise rate ceiling. When set,
	// Enqueue passes every destructive user/admin intent through it BEFORE the
	// write-ahead row is created (the producer chokepoint). nil ⇒ ungated (unit
	// tests, and system-only Stores where the gate is inert anyway). The gate
	// carries its OWN root pool DB, so it survives tx-rebind (WithTx).
	ceiling *RateCeiling
}

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// NewStoreGated builds a Store whose destructive user/admin enqueues are gated
// by the #732 rate ceiling. The ceiling references the ROOT pool DB and is
// preserved across tx-rebinds (withTxDB).
func NewStoreGated(d *db.DB, ceiling *RateCeiling) *Store {
	return &Store{db: d, ceiling: ceiling}
}

// withTxDB rebinds this Store onto a tx-scoped DB while PRESERVING the rate
// ceiling (whose own pool-backed DB is independent of the tx). Producers that
// commit the enqueue atomically with their local write (ccbill/nmi cancel
// schedulers) use this so the gate is not lost on rebind.
func (s *Store) withTxDB(txdb *db.DB) *Store {
	return &Store{db: txdb, ceiling: s.ceiling}
}

// EnqueueParams describes one logical intent. MerchantID is stamped explicitly;
// IdempotencyKey makes the enqueue effectively-once (see the query's conflict
// semantics: pending refreshed, superseded/expired revived, rest untouched).
type EnqueueParams struct {
	MerchantID     uuid.UUID
	Provider       string
	IntentType     string
	SubscriptionID *uuid.UUID
	PaymentID      *uuid.UUID
	PriceID        *uuid.UUID
	// PspID is the PSP the outbound write is addressed to. Required (or#893)
	// unless the intent is CUSTODIAN-addressed: an intent nobody can attribute
	// cannot be executed against the right credentials.
	PspID uuid.UUID
	// CustodianID addresses the write to a custodian instead (or#795's batch
	// account updater uploads one token batch to a custodian that backs many
	// PSPs, so no single psp_id names it). Exactly the rail_intents_addressed
	// constraint: one of the two must be set.
	CustodianID    uuid.UUID
	Payload        any
	IdempotencyKey string
	NextAttemptAt  time.Time
	Origin         Origin
	OriginReason   string
	// Actor is the authenticated principal id (admin user id / self-service
	// customer id) that produced this intent, stamped on the row and used by the
	// #732 per-actor ceiling. Empty ⇒ resolved from the ambient principal on the
	// context (auth middleware); system/background paths carry none.
	Actor     string
	ExpiresAt *time.Time
}

// Enqueue records the intent (idempotent) and returns the canonical row for
// its idempotency key.
func (s *Store) Enqueue(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	scope, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	if scope.UUID() != p.MerchantID {
		return gen.OpenrailsRailIntent{}, errors.New("intent merchant does not match context")
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
		return gen.OpenrailsRailIntent{}, errors.New("custodian deletion requires owned method admission")
	}
	if p.IntentType == "nmi_sale" {
		return s.enqueueSale(ctx, p)
	}
	if p.IntentType != "nmi_upgrade" && p.IntentType != subscriptions.TypeManualRebill && p.IntentType != subscriptions.TypeSubscriptionCollection {
		return s.enqueue(ctx, p)
	}
	if p.SubscriptionID == nil {
		return gen.OpenrailsRailIntent{}, errors.New("recurring operation requires a subscription")
	}
	var engineCustomer uuid.UUID
	if p.IntentType == subscriptions.TypeSubscriptionCollection {
		observed, err := subscriptions.NewSubscriptionRepo(s.db).GetByID(ctx, *p.SubscriptionID)
		if err != nil {
			return gen.OpenrailsRailIntent{}, err
		}
		engineCustomer = observed.CustomerID
	}
	var row gen.OpenrailsRailIntent
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
			if !ok || p.PspID == uuid.Nil || (p.Origin != OriginSystem && (p.Origin != OriginUser || payload.Initiator != charge.InitiatorCustomer || p.Actor != engineCustomer.String())) {
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
			if sub.PaymentMethodID == nil || method.ID != *sub.PaymentMethodID || method.CustomerID != engineCustomer || method.ParkReason != "" {
				return errors.New("engine admission payment method changed")
			}
			if err := payload.Instrument.Matches(method, charge.AgreementRecurring); err != nil {
				return err
			}
			if sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(payload.PreviousPeriodEnd) || (sub.Status != "active" && sub.Status != "past_due") || (p.Origin == OriginSystem && sub.Status == "past_due" && (sub.NextRetryAt == nil || sub.NextRetryAt.After(payload.AcceptedAt))) || (p.Origin == OriginUser && sub.Status != "past_due") {
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
		}
		if p.IntentType == "nmi_upgrade" {
			if err := subscriptions.RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
				return err
			}
		} else {
			_, err := d.Gen(ctx).GetLiveTierChangeRailIntent(ctx, gen.GetLiveTierChangeRailIntentParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
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
			if accepted.Renewal.CustomerID != sub.CustomerID || sub.PaymentMethodID == nil || accepted.PaymentMethodID != *sub.PaymentMethodID || accepted.Instrument.PSPID != sub.PspID {
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
		if method.CustomerID.String() != accepted.UserID || method.Rail != row.Rail || method.ParkReason != "" {
			return apperr.Conflictf("payment method changed before upgrade admission")
		}
		for _, agreement := range []charge.Agreement{charge.AgreementRecurring, charge.AgreementUnscheduled} {
			if err := accepted.Instrument.Matches(method, agreement); err != nil {
				return apperr.Conflictf("payment method changed before upgrade admission")
			}
		}
		return nil
	})
	return row, err
}

func (s *Store) enqueue(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	if p.IntentType == TypeNMIPaymentMethodDelete || p.IntentType == TypeHyperSwitchMethodDelete {
		if err := validatePaymentMethodDeleteAuthority(ctx, p); err != nil {
			return gen.OpenrailsRailIntent{}, err
		}
	}
	if p.IntentType == "" || p.IdempotencyKey == "" {
		return gen.OpenrailsRailIntent{}, fmt.Errorf("intents: enqueue requires intent_type and idempotency_key")
	}
	// or#893/or#795 (rail_intents_addressed): the intent names the account it
	// will execute against — a PSP, or a custodian for the writes addressed to
	// one. An explicit value wins; otherwise the PSP the caller already routed
	// to and pinned on ctx (checkout's stampPSP, the webhook plane) is the
	// answer. Nothing else is: an intent nobody can attribute cannot be executed
	// against the right credentials.
	if p.CustodianID == uuid.Nil {
		p.CustodianID = db.CustodianIDFromContext(ctx)
	}
	if p.PspID == uuid.Nil && p.CustodianID == uuid.Nil {
		psp, err := db.RequirePSPID(ctx)
		if err != nil {
			return gen.OpenrailsRailIntent{}, fmt.Errorf("intents: enqueue %s: %w", p.IntentType, err)
		}
		p.PspID = psp
	}
	var payload []byte
	if p.Payload != nil {
		b, err := json.Marshal(p.Payload)
		if err != nil {
			return gen.OpenrailsRailIntent{}, fmt.Errorf("intents: marshal payload: %w", err)
		}
		payload = b
	}
	var originReason *string
	if p.OriginReason != "" {
		originReason = &p.OriginReason
	}
	// Resolve the actor (explicit override, else the ambient authenticated
	// principal) up front: it is both stamped on the row and the #732 per-actor
	// ceiling key.
	actor := ResolveActor(ctx, p.Actor)

	// #732 anti-credential-compromise rate ceiling: destructive user/admin ops
	// pass through the gate BEFORE the write-ahead intent is created. A trip
	// returns a typed refusal (no row created ⇒ the op does not happen); a gate
	// evaluation error FAILS CLOSED (also refuses). The gate is inert for
	// non-destructive types and system-origin ops.
	if s.ceiling != nil {
		if err := s.ceiling.Check(ctx, CheckParams{
			Actor:      actor,
			MerchantID: p.MerchantID,
			IntentType: p.IntentType,
			Origin:     p.Origin,
		}, time.Now().UTC()); err != nil {
			return gen.OpenrailsRailIntent{}, err
		}
	}

	var actorPtr *string
	if actor != "" {
		actorPtr = &actor
	}
	// psp_id is stamped only when the producer already has observed
	// provenance (for example an existing subscription pinned to an account).
	return s.db.Gen(ctx).EnqueueRailIntent(ctx, gen.EnqueueRailIntentParams{
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
		ExpiresAt:      p.ExpiresAt,
		PspID:          uuidPtrOrNil(p.PspID),
		CustodianID:    uuidPtrOrNil(p.CustodianID),
	})
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
	return s.db.Gen(ctx).SupersedeRailIntentsBySubject(ctx, gen.SupersedeRailIntentsBySubjectParams{
		MerchantID:     scopeMerchantID.UUID(),
		IntentType:     intentType,
		SubscriptionID: &subscriptionID,
		Reason:         &reason,
	})
}

// ClaimByID leases ONE specific intent for the synchronous execute path.
// ok=false means the row is not claimable (terminal, expired, or leased by a
// live executor) — the caller inspects the canonical row instead.
func (s *Store) ClaimByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.OpenrailsRailIntent, bool, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return gen.OpenrailsRailIntent{}, false, scopeErr
	}
	row, err := s.db.Gen(ctx).ClaimRailIntentByID(ctx, gen.ClaimRailIntentByIDParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id,
		Now:        now.UTC(),
		LeaseUntil: leaseUntil.UTC(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsRailIntent{}, false, nil
		}
		return gen.OpenrailsRailIntent{}, false, err
	}
	return row, true, nil
}

// Get returns the intent row by id.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (gen.OpenrailsRailIntent, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return gen.OpenrailsRailIntent{}, scopeErr
	}
	return s.db.Gen(ctx).GetRailIntent(ctx, gen.GetRailIntentParams{MerchantID: scopeMerchantID.UUID(), ID: id})
}

// DueExecuteMerchants lists the merchants with executor work (claimable or
// expirable intents) through migration 0022's SECURITY DEFINER work queue —
// the executor's fan-out list. Ids only; each merchant's pass then runs under
// its own pinned connection (or#862).
func (s *Store) DueExecuteMerchants(ctx context.Context, now time.Time, limit int32) ([]uuid.UUID, error) {
	rows, err := s.db.GenDirectory().ListDueRailIntentMerchants(ctx, gen.ListDueRailIntentMerchantsParams{
		Now: now.UTC(), MerchantLimit: limit,
	})
	return derefIDs(rows), err
}

// DueVerifyMerchants is DueExecuteMerchants for the verifier pass (or#862).
func (s *Store) DueVerifyMerchants(ctx context.Context, now time.Time, limit int32) ([]uuid.UUID, error) {
	rows, err := s.db.GenDirectory().ListDueVerifyRailIntentMerchants(ctx, gen.ListDueVerifyRailIntentMerchantsParams{
		Now: now.UTC(), MerchantLimit: limit,
	})
	return derefIDs(rows), err
}

// derefIDs drops the pointer indirection sqlc emits for a set-returning
// function's column. The definer bodies select a NOT NULL column, so a nil
// here is impossible; it is skipped rather than dereferenced.
func derefIDs(rows []*uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, id := range rows {
		if id != nil {
			out = append(out, *id)
		}
	}
	return out
}

// ClaimDue leases up to batch due executable intents (SKIP LOCKED).
//
// or#862: this MUST run on a merchant-pinned connection. rail_intents FORCEs
// RLS, so a bare-context claim matched `merchant_id = NULL` and leased ZERO
// intents — silently, with no error — which is how the entire outbound
// provider-mutation plane came to be inert while its tests passed.
func (s *Store) ClaimDue(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsRailIntent, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if err := s.db.AssertMerchantScope(ctx, "intent executor claim"); err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ClaimDueRailIntents(ctx, gen.ClaimDueRailIntentsParams{
		MerchantID: scopeMerchantID.UUID(),
		Now:        now.UTC(),
		LeaseUntil: leaseUntil.UTC(),
		BatchSize:  batch,
	})
}

// RenewClaim extends a live lease while the handler runs (xs-007 row 32).
// false means the lease had already lapsed — another executor may own the row
// now, and this one must not write over it without its per-type verify.
func (s *Store) RenewClaim(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (bool, error) {
	ctx, release, err := s.db.WithIndependentMerchantConn(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return false, scopeErr
	}
	n, err := s.db.Gen(ctx).RenewRailIntentClaim(ctx, gen.RenewRailIntentClaimParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Now: now.UTC(), LeaseUntil: leaseUntil.UTC(),
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClaimDueVerify leases up to batch due unknown_needs_verify intents. Same
// merchant-pin requirement as ClaimDue (or#862).
func (s *Store) ClaimDueVerify(ctx context.Context, now, leaseUntil time.Time, batch int64) ([]gen.OpenrailsRailIntent, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if err := s.db.AssertMerchantScope(ctx, "intent verifier claim"); err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ClaimDueVerifyRailIntents(ctx, gen.ClaimDueVerifyRailIntentsParams{
		MerchantID: scopeMerchantID.UUID(),
		Now:        now.UTC(),
		LeaseUntil: leaseUntil.UTC(),
		BatchSize:  batch,
	})
}

// ClaimUnknownByID leases one unknown operation for operator resolution.
// ok=false means it is not unknown or another worker holds its lease.
func (s *Store) ClaimUnknownByID(ctx context.Context, id uuid.UUID, now, leaseUntil time.Time) (gen.OpenrailsRailIntent, bool, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return gen.OpenrailsRailIntent{}, false, scopeErr
	}
	row, err := s.db.Gen(ctx).ClaimUnknownRailIntentByID(ctx, gen.ClaimUnknownRailIntentByIDParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Now: now.UTC(), LeaseUntil: leaseUntil.UTC(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.OpenrailsRailIntent{}, false, nil
	}
	if err != nil {
		return gen.OpenrailsRailIntent{}, false, err
	}
	return row, true, nil
}

// ReleaseUnknownClaim drops a resolver lease without changing the operation.
func (s *Store) ReleaseUnknownClaim(ctx context.Context, id uuid.UUID) (bool, error) {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return false, scopeErr
	}
	n, err := s.db.Gen(ctx).ReleaseUnknownRailIntentClaim(ctx, gen.ReleaseUnknownRailIntentClaimParams{MerchantID: scopeMerchantID.UUID(), ID: id})
	return n == 1, err
}

// ExpireOverdue expires every live intent whose relevance window elapsed —
// except destructive intents whose merchant has an OPEN held_bulk finding
// (#679): breaker-held intents never expire out from under the operator.
func (s *Store) ExpireOverdue(ctx context.Context, now time.Time) (int64, error) {
	if err := s.db.AssertMerchantScope(ctx, "intent expiry sweep"); err != nil {
		return 0, err
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return 0, scopeErr
	}
	return s.db.Gen(ctx).ExpireOverdueRailIntents(ctx, gen.ExpireOverdueRailIntentsParams{
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
	return oneTerminal(s.db.Gen(ctx).MarkRailIntentSucceeded(ctx, gen.MarkRailIntentSucceededParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Now: now.UTC(), ResultEvidence: ev,
	}))
}

// pruneEvidenceKeys are the result-pointer keys a slim succeeded tombstone
// retains by default — the dunning repair path reads them back off
// result_evidence (internal/river/jobs_dunning.go: transaction_id for the
// lifecycle repair, response_code for hard/soft decline classification; the
// admin operations view also surfaces transaction_id). Everything else is
// forensic and was already durably logged to rail_mutation_logs before the
// success transition, so it is safe to drop from the intent row — UNLESS the handler asks to
// keep its evidence (PrunePolicy), which the catalog archive/sunset handlers do
// because internal/service/catalog_extras.go renders their verification booleans.
var pruneEvidenceKeys = []string{"transaction_id", "response_code"}

// PruneSucceeded slims a just-succeeded intent down to a dedupe tombstone:
// the heavy payload is dropped and result_evidence is reduced to the pointer
// keys downstream readers still need. The ROW ITSELF IS RETAINED — it is the
// effectively-once tombstone (UNIQUE on merchant_id+idempotency_key); deleting
// it would let a re-enqueue re-run the provider mutation (a double charge).
//
// keepPayload/keepEvidence are the handler's PrunePolicy: a handler whose
// payload is read AFTER success (the refund producer's conflict detection reads
// reservation_id off the durable succeeded row — admin_payments.go) sets
// keepPayload; a handler whose result_evidence is read after success (catalog
// archive/sunset detail — catalog_extras.go) sets keepEvidence.
//
// The WHERE status='succeeded' guard makes this a no-op on anything not (still)
// succeeded. Best-effort by design: a prune failure leaves the full row in
// place, which is correct — just larger — and never weakens the dedupe.
//
// Cutover tombstones retain accepted terms, step receipts and append-only account
// continuity history even if a caller supplies a weaker handler prune policy.
//
// RAW pgx (no sqlc): runs on Qx(ctx) so the schema rewriter (#471) and RLS
// apply exactly as they do for the generated queries.
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
	qx := s.db.Qx(ctx)
	if keepEvidence {
		// Drop the payload only; leave result_evidence intact for the handler's
		// post-success readers.
		_, err := qx.Exec(ctx,
			`UPDATE openrails.rail_intents
			    SET payload = CASE WHEN result_evidence ? 'qualified_receipt' THEN payload ELSE NULL END, updated_at = now()
			  WHERE id = $1 AND merchant_id = $2 AND status = 'succeeded' AND intent_type NOT IN ('nmi_provider_cutover','subscription_collection') AND NOT (COALESCE(result_evidence,'{}'::jsonb) ? 'qualified_enrollment')`, id, mid.UUID())
		return err
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
		_, err := qx.Exec(ctx,
			`UPDATE openrails.rail_intents
			    SET result_evidence = CASE WHEN result_evidence ? 'qualified_receipt' THEN coalesce($2::jsonb,'{}'::jsonb) || jsonb_build_object('qualified_receipt',result_evidence->'qualified_receipt') ELSE $2::jsonb END, updated_at = now()
			  WHERE id = $1 AND merchant_id = $3 AND status = 'succeeded' AND intent_type NOT IN ('nmi_provider_cutover','subscription_collection') AND NOT (COALESCE(result_evidence,'{}'::jsonb) ? 'qualified_enrollment')`, id, ev, mid.UUID())
		return err
	}
	_, err = qx.Exec(ctx,
		`UPDATE openrails.rail_intents
		    SET payload = CASE WHEN result_evidence ? 'qualified_receipt' THEN payload ELSE NULL END, result_evidence = CASE WHEN result_evidence ? 'qualified_receipt' THEN coalesce($2::jsonb,'{}'::jsonb) || jsonb_build_object('qualified_receipt',result_evidence->'qualified_receipt') ELSE $2::jsonb END, updated_at = now()
		  WHERE id = $1 AND merchant_id = $3 AND status = 'succeeded' AND intent_type NOT IN ('nmi_provider_cutover','subscription_collection') AND NOT (COALESCE(result_evidence,'{}'::jsonb) ? 'qualified_enrollment')`, id, ev, mid.UUID())
	return err
}

// PruneTerminalPayload removes a short-lived credential from a terminal
// intent while retaining its status, reason, and non-sensitive evidence.
func (s *Store) PruneTerminalPayload(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("intents: prune terminal intent: %w", err)
	}
	_, err = s.db.Qx(ctx).Exec(ctx,
		`UPDATE openrails.rail_intents
		    SET payload = CASE WHEN result_evidence ? 'qualified_receipt' THEN payload ELSE NULL END, updated_at = now()
		  WHERE id = $1 AND merchant_id = $2 AND status = 'failed_terminal' AND intent_type NOT IN ('nmi_provider_cutover','subscription_collection') AND NOT (COALESCE(result_evidence,'{}'::jsonb) ? 'qualified_enrollment')`, id, mid.UUID())
	return err
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

// RecordProgress merges keys into result_evidence on a LIVE (non-terminal)
// intent — handlers use it to durably pin provider references (e.g. a signed
// Solana tx signature) BEFORE the side effect is sent (#674), so a crash
// mid-send resolves via a provider read keyed on the recorded reference.
//
// RAW pgx (no sqlc): runs on Qx(ctx) so the schema rewriter (#471) and RLS
// apply exactly as for the generated queries.
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
	_, err = s.db.Qx(ctx).Exec(ctx,
		`UPDATE openrails.rail_intents
		    SET result_evidence = coalesce(result_evidence, '{}'::jsonb) || $2::jsonb,
		        updated_at = now()
		  WHERE id = $1
		    AND merchant_id = $3
		    AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')`, id, b, mid.UUID())
	return err
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
	result, err := s.db.Qx(ctx).Exec(ctx,
		`UPDATE openrails.rail_intents
		    SET result_evidence = coalesce(result_evidence, '{}'::jsonb) || jsonb_build_object($2::text, $3::jsonb),
		        updated_at = now()
		  WHERE id = $1
		    AND merchant_id = $4
		    AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')
		    AND NOT (coalesce(result_evidence, '{}'::jsonb) ? $2::text)
 AND ($2::text <> 'initial_submitted' OR NOT (coalesce(result_evidence,'{}'::jsonb) ? 'qualified_initial_refusal'))`, id, key, b, mid.UUID())
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) MarkFailedRetryable(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	rows, err := s.db.Gen(ctx).MarkRailIntentFailedRetryable(ctx, gen.MarkRailIntentFailedRetryableParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
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
	return one(s.db.Gen(ctx).MarkRailIntentUnknown(ctx, gen.MarkRailIntentUnknownParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason, ResultEvidence: raw,
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
	return oneTerminal(s.db.Gen(ctx).MarkRailIntentFailedTerminal(ctx, gen.MarkRailIntentFailedTerminalParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, Reason: &reason, ResultEvidence: ev,
	}))
}

func (s *Store) Park(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time, reason string) error {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	rows, err := s.db.Gen(ctx).ParkRailIntent(ctx, gen.ParkRailIntentParams{
		MerchantID: scopeMerchantID.UUID(),
		ID:         id, NextAttemptAt: nextAttemptAt.UTC(), Reason: &reason,
	})
	if err != nil || rows != 0 {
		return err
	}
	// Park deliberately cannot clear a payment submission fence. A blocked
	// executor still owns that payment: retain it for read-only verification,
	// rather than leaving its claim in flight until lease expiry.
	current, err := s.Get(ctx, id)
	if db.IsNotFound(err) {
		return nil
	}
	if err != nil || current.Status != StatusInFlight {
		return err
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal(current.ResultEvidence, &evidence); err != nil {
		return err
	}
	key := ""
	switch current.IntentType {
	case "invoice_collection", "subscription_collection":
		key = "submitted_at"
	case "nmi_sale":
		key = "sale_submitted"
	case "initial_membership":
		key = "initial_submitted"
	}
	if _, submitted := evidence[key]; key != "" && submitted {
		// MarkUnknown's SQL accepts only live states, so a concurrent sealed
		// completion is never overwritten by this stale blocked outcome.
		return s.MarkUnknown(ctx, id, nextAttemptAt, reason, nil)
	}
	return nil
}

func (s *Store) MarkSuperseded(ctx context.Context, id uuid.UUID, reason string) error {
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	return one(s.db.Gen(ctx).MarkRailIntentSuperseded(ctx, gen.MarkRailIntentSupersededParams{
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
func (s *Store) GetByIdempotencyKey(ctx context.Context, key string) (gen.OpenrailsRailIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	return s.db.Gen(ctx).GetRailIntentByIdempotencyKey(ctx, gen.GetRailIntentByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: key})
}

// LiveTierChange returns the unresolved tier change (NMI upgrade or Stripe
// tier change) that owns the subscription; db.IsNotFound when none does.
func (s *Store) LiveTierChange(ctx context.Context, subscriptionID uuid.UUID) (gen.OpenrailsRailIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	return s.db.Gen(ctx).GetLiveTierChangeRailIntent(ctx, gen.GetLiveTierChangeRailIntentParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID})
}
