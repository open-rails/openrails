package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
)

func workerNow(c clockwork.Clock) time.Time {
	if c != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// ProviderOperationWorker keeps one durable River job alive until the accepted
// financial operation is terminal. River owns waking/retrying; the ledger owns
// authorization, submission fences, provider evidence, and final accounting.
type ProviderOperationWorker struct {
	river.WorkerDefaults[intents.OperationArgs]
	DB             *db.DB
	Config         *config.Config
	Clock          clockwork.Clock
	Registry       *intents.Registry
	MutationLogger intents.MutationLogger
}

func (ProviderOperationWorker) Kind() string { return intents.OperationJobKind }

const operationInfrastructureRetry = time.Minute

func (w ProviderOperationWorker) Work(ctx context.Context, job *river.Job[intents.OperationArgs]) (result error) {
	var successorCommitted bool
	// A handler panic is still unresolved money. River snoozing leaves the job
	// active without spending its finite infrastructure-crash rescue attempts.
	defer func() {
		if p := recover(); p != nil {
			log.WithContext(ctx).WithField("panic", p).Error("provider operation panicked; retained for retry")
			if successorCommitted {
				result = nil
			} else {
				result = river.JobSnooze(operationInfrastructureRetry)
			}
		}
	}()
	if job == nil || job.Args.MerchantID == uuid.Nil || job.Args.IntentID == uuid.Nil {
		return river.JobCancel(fmt.Errorf("provider operation requires merchant and operation ids"))
	}
	if w.DB == nil || w.Registry == nil {
		return river.JobSnooze(operationInfrastructureRetry)
	}
	args := job.Args
	store := intents.NewStore(w.DB)
	runner := &intents.Runner{Store: store, Registry: w.Registry, Config: w.Config, Clock: w.Clock, Logger: w.MutationLogger, Breaker: intents.NewVolumeBreaker(w.DB), Destructive: destructive.New(w.DB)}
	runner.OnSuccessorCommitted = func(mid, id uuid.UUID) {
		if mid == args.MerchantID && id == args.IntentID {
			successorCommitted = true
		}
	}
	delay := time.Duration(0)
	terminal := false
	err := w.DB.RunInMerchantScope(ctx, merchant.ID(args.MerchantID), "provider operation", func(ctx context.Context) error {
		now := workerNow(w.Clock)
		row, err := store.PrepareDispatch(ctx, args.IntentID, now)
		if err != nil {
			return err
		}
		if intents.OperationTerminal(row.Status) {
			terminal = true
			return nil
		}
		wake := row.NextAttemptAt
		if row.ClaimedUntil != nil && (row.Status == intents.StatusInFlight || row.ClaimedUntil.After(wake)) {
			wake = *row.ClaimedUntil
		}
		if wake.After(now) {
			delay = wake.Sub(now)
			return nil
		}
		switch row.Status {
		case intents.StatusPending, intents.StatusFailedRetryable:
			row, err = runner.ExecuteByID(ctx, row.ID)
		case intents.StatusUnknownNeedsVerify:
			row, err = runner.VerifyByID(ctx, row.ID)
		case intents.StatusInFlight:
			// A concurrent executor acquired/renewed its claim after PrepareDispatch.
			delay = operationInfrastructureRetry
			return nil
		default:
			return fmt.Errorf("unknown financial operation status %q", row.Status)
		}
		if err != nil {
			return err
		}
		if intents.OperationTerminal(row.Status) {
			terminal = true
			return nil
		}
		wake = row.NextAttemptAt
		if row.ClaimedUntil != nil && (row.Status == intents.StatusInFlight || row.ClaimedUntil.After(wake)) {
			wake = *row.ClaimedUntil
		}
		delay = wake.Sub(workerNow(w.Clock))
		if delay <= 0 && row.Status == intents.StatusPending && row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
			// Expiry preserved a held/submitted operation whose financial gates
			// refused a claim. Keep its existing hold cadence, not a busy loop.
			delay = intents.ParkRetryInterval
		}
		return nil
	})
	if successorCommitted {
		// The replacement wake is already durable. Retaining this consumed job
		// as another snooze would grow active jobs on every unresolved cycle.
		return nil
	}
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("intent_id", args.IntentID).Error("provider operation unresolved; River will retry")
		return river.JobSnooze(operationInfrastructureRetry)
	}
	if terminal {
		return nil
	}
	if delay <= 0 {
		delay = time.Second
	}
	return river.JobSnooze(delay)
}
