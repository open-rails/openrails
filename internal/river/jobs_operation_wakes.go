package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/intents/wake"
	"github.com/open-rails/openrails/internal/merchant"
)

const (
	KindOperationWakes = "openrails.operation_wakes"
	// OperationWakesInterval is how often lost wakes are looked for.
	OperationWakesInterval = 15 * time.Minute
	// operationWakeGrace is how long past its due time a live operation
	// waits before its wake counts as lost: River runs a due job in seconds.
	operationWakeGrace = 10 * time.Minute
	operationWakeBatch = 500
)

// OperationWakesArgs is one pass over overdue live operations.
type OperationWakesArgs struct{}

func (OperationWakesArgs) Kind() string { return KindOperationWakes }

func (OperationWakesArgs) InsertOpts() river.InsertOpts { return periodPass(OperationWakesInterval) }

// OperationWakesWorker re-queues the wake of each live provider operation
// overdue past the grace with none waiting in a running fleet's River schema:
// its job was deleted, its schema reset, or its fleet stopped. Every write
// that leaves an operation live queues its wake, so the pass finds nothing
// unless one was lost. A re-queued duplicate is harmless: claims are
// canonical.
type OperationWakesWorker struct {
	WorkerDefaults[OperationWakesArgs]
	DB    *db.DB
	Clock clockwork.Clock
	// Pool reads River's tables, which sqlc does not know.
	Pool *pgxpool.Pool
}

func (OperationWakesWorker) Kind() string { return KindOperationWakes }

func (w OperationWakesWorker) Work(ctx context.Context, _ *river.Job[OperationWakesArgs]) error {
	now := w.Clock.Now().UTC()
	overdue, err := w.DB.GenDirectory().ListOverdueProviderOperations(ctx, gen.ListOverdueProviderOperationsParams{Before: now.Add(-operationWakeGrace), RowLimit: operationWakeBatch})
	if err != nil {
		return fmt.Errorf("operation wakes: list overdue: %w", err)
	}
	if len(overdue) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(overdue))
	for i, op := range overdue {
		ids[i] = op.ID
	}
	waiting, err := liveWakes(ctx, w.Pool, ids)
	if err != nil {
		return fmt.Errorf("operation wakes: read River: %w", err)
	}
	var errs error
	for _, op := range overdue {
		if waiting[op.ID] {
			continue
		}
		err := w.DB.MerchantTx(merchant.WithID(ctx, billing.MerchantID(op.MerchantID)), func(ctx context.Context, tx pgx.Tx) error {
			row, err := intents.NewStore(w.DB.NewWithPgxTx(tx)).Get(ctx, op.ID)
			if err != nil || intents.OperationTerminal(row.Status) {
				return err
			}
			return wake.Queue(ctx, w.DB, tx, row)
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("operation %s: %w", op.ID, err))
			continue
		}
		log.WithContext(ctx).WithFields(log.Fields{"merchant_id": op.MerchantID, "intent_id": op.ID}).Warn("provider operation had no wake; re-queued")
	}
	return errs
}

// liveWakes reports which of ids have a wake waiting in a River schema of
// this database whose fleet runs: one whose leader holds its lease. Each
// fleet sharing the database has its own River schema.
func liveWakes(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	if pool == nil {
		return nil, errors.New("no pool")
	}
	rows, err := pool.Query(ctx, `
SELECT n.nspname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = 'river_job' AND c.relkind IN ('r', 'p')
  AND to_regclass(quote_ident(n.nspname) || '.river_leader') IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = id.String()
	}
	live := map[uuid.UUID]bool{}
	for _, schema := range schemas {
		jobs, leader := pgx.Identifier{schema, "river_job"}.Sanitize(), pgx.Identifier{schema, "river_leader"}.Sanitize()
		rows, err := pool.Query(ctx, `
SELECT x FROM unnest($1::text[]) x
WHERE EXISTS (SELECT 1 FROM `+leader+` l WHERE l.expires_at > now())
  AND EXISTS (SELECT 1 FROM `+jobs+` j WHERE j.kind = $2 AND j.args @> jsonb_build_object('intent_id', x)
              AND j.state IN ('available', 'pending', 'running', 'retryable', 'scheduled'))`, keys, wake.Kind)
		var found []string
		if err == nil {
			found, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if dropped(err) {
			continue // the schema went since it was listed: no fleet runs it
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", jobs, err)
		}
		for _, id := range found {
			live[uuid.MustParse(id)] = true
		}
	}
	return live, nil
}

// dropped reports a schema or table that no longer exists.
func dropped(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000")
}
