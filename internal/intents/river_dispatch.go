package intents

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
)

const OperationJobKind = "openrails.provider_operation"

// OperationArgs carries identities only. The ledger decides what is authorized.
type OperationArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	IntentID   uuid.UUID `json:"intent_id" river:"unique"`
}

func (OperationArgs) Kind() string { return OperationJobKind }

func operationInsertOpts(at time.Time) *river.InsertOpts {
	return &river.InsertOpts{Queue: "billing", ScheduledAt: at,
		// Business/infra waiting uses Snooze (which does not consume attempts).
		// Leave maximal room for River's crash rescue; never use its usual finite
		// provider-error retry budget to abandon an accepted financial operation.
		MaxAttempts: math.MaxInt16,
	}
}

func OperationTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailedTerminal, StatusSuperseded, StatusExpired:
		return true
	default:
		return false
	}
}

// PrepareDispatch touches only this operation. Expiration retains every existing
// submission/hold guard. A lost executor lease becomes an unknown result, never
// an execute retry: the provider may already have accepted the mutation.
func (s *Store) PrepareDispatch(ctx context.Context, id uuid.UUID, now time.Time) (gen.OpenrailsRailIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	if _, err := s.db.Gen(ctx).RecoverAbandonedRailIntentByID(ctx, gen.RecoverAbandonedRailIntentByIDParams{MerchantID: mid.UUID(), ID: id, Now: now}); err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	if _, err := s.db.Gen(ctx).ExpireRailIntentByID(ctx, gen.ExpireRailIntentByIDParams{MerchantID: mid.UUID(), ID: id, Now: now, BreakerHeldTypes: DestructiveIntentTypes()}); err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	return s.Get(ctx, id)
}

// WakeOperation adds a prompt wakeup after a caller has qualified provider
// evidence for this already accepted operation. Only an unknown result gets an
// earlier verification time; authorization, financial status, and live leases
// remain unchanged. Do not deduplicate against a running/scheduled job: it may
// be completing an earlier terminal state or sleeping until an obsolete time.
func (s *Store) WakeOperation(ctx context.Context, id uuid.UUID, now time.Time) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := s.withTxDB(s.db.NewWithPgxTx(tx)).Get(ctx, id)
		if err != nil {
			return err
		}
		if row.Status != StatusUnknownNeedsVerify && row.Status != StatusInFlight {
			return nil
		}
		if _, err := s.db.NewWithPgxTx(tx).Gen(ctx).AdvanceRailIntentVerification(ctx, gen.AdvanceRailIntentVerificationParams{MerchantID: row.MerchantID, ID: row.ID, Now: now}); err != nil {
			return err
		}
		return s.db.InsertRiverJobTx(ctx, tx, OperationArgs{MerchantID: row.MerchantID, IntentID: row.ID}, operationInsertOpts(now))
	})
}

// The worker installs this only around the Runner's own post-handler transitions.
// It is never inherited by handler transactions or unrelated inline requests.
type successorCommitHook func(merchantID, intentID uuid.UUID)
type successorCommitContextKey struct{}

// transitionAndWake commits a nonterminal ledger transition and its scheduled
// wakeup together. A running job may already have snoozed against an older lease;
// canonical claims, not active-job uniqueness, make duplicate wakeups harmless.
func (s *Store) transitionAndWake(ctx context.Context, id uuid.UUID, transition func(context.Context, *Store) (int64, time.Time, error)) (int64, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	var rows int64
	transitionTx := func(ctx context.Context, tx pgx.Tx) error {
		var err error
		var at time.Time
		rows, at, err = transition(ctx, s.withTxDB(s.db.NewWithPgxTx(tx)))
		if err != nil || rows == 0 {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("operation transition affected %d rows", rows)
		}
		return s.db.InsertRiverJobTx(ctx, tx, OperationArgs{MerchantID: mid.UUID(), IntentID: id}, operationInsertOpts(at.UTC()))
	}
	hook, _ := ctx.Value(successorCommitContextKey{}).(successorCommitHook)
	if pool := s.db.DataPool(); hook != nil && pool != nil {
		// Unlike a savepoint release, this explicitly verifies an independent
		// top-level commit before the consumed worker job may retire.
		err = pool.CommittedMerchantTx(ctx, mid, transitionTx)
		if err == nil && rows == 1 {
			hook(mid.UUID(), id)
		}
	} else {
		// Host transactions keep their existing rollback domain. They never
		// acknowledge a successor before the host's eventual commit.
		err = s.db.MerchantTx(ctx, transitionTx)
	}
	return rows, err
}
